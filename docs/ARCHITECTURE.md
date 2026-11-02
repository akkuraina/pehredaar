# Pehredaar Architecture

Status: **proposal** (v1 / SSC MVP). Nothing here is implemented yet.
Read alongside [`CLAUDE.md`](../CLAUDE.md), which holds the product brief.

---

## 1. Goals and non-goals

**Goals (v1)**

- Detect meaningful edits to a curated set of SSC notices and PDFs, and new
  notices (e.g. a corrigendum) appearing in SSC feeds.
- Tell subscribers *what* changed in one or two plain sentences, on Telegram.
- Never alert on cosmetic noise. A false alert costs more trust than a late one.
- Be a polite client: few requests, clear identity, back off when a site struggles.
- Survive restarts and outages without losing or duplicating alerts.

**Non-goals (v1)**

- User-submitted URLs, a public web UI (the local operator dashboard, §18, is
  not one), other exam families, JS rendering (chromedp),
  OCR of scanned PDFs, multi-instance deployment.

---

## 2. What we learned about the source (drives the design)

Verified on 2026-10-30 (details in `CLAUDE.md`):

- `ssc.gov.in` is a JavaScript single-page app. The HTML is an empty shell.
- Its content comes from a JSON API (`/api/general-website/portal/...`) with
  feeds per `contentType` (notice-boards, ssc-calendar, results, ...).
- Each feed item has an `id`, `headline`, `createdAt` and attachments (PDF paths).
- PDFs are served with `ETag` / `Last-Modified` and extract cleanly with `pdftotext`.

Consequence: Pehredaar watches **two kinds of things**, not "web pages":

| Kind       | Example                          | "Changed" means                                  |
|------------|----------------------------------|--------------------------------------------------|
| **Feed**   | SSC notice-board JSON            | An item was added, removed, or its headline/attachments edited |
| **Document** | A notice PDF linked from a feed | The extracted text differs in a meaningful way    |

An `html` kind stays in the design (goquery) for future exam families, but
SSC does not need it.

**English and Hindi** (verified 2026-10-31). `language=hindi` returns a
*separate* feed: different item ids, far fewer items (143 vs 716 on the notice
board), and it lags. Its newest item was 14 Oct while English had items up to
29 Oct, and the 26 Oct revised-dates corrigendum had no Hindi entry yet. Hindi
**headlines** in the JSON are clean Unicode. Hindi **PDF text** extracted by
`pdftotext` is garbled: broken matras and wrong glyph mappings, e.g.
"महत्वपूर्ण सूचना" → "र्हत्वपरू ्म सचू ना". This comes from the PDFs'
font encoding. Digits and dates survive intact. Consequences:

- Change *detection* on Hindi PDFs still works, because the garbling is deterministic.
- Hindi PDF text must **not** be fed to the LLM as if it were readable. See §10.
- Worth trying later: poppler's `pdftotext` (vs the xpdf build on the dev
  machine) and OCR for Hindi PDFs. Neither is in v1.

Unknown, to be measured: whether SSC edits PDFs **in place** or only posts **new
notices** (corrigenda). The design handles both; the probe tool (§12) will tell
us which matters more.

---

## 3. System overview

```mermaid
flowchart LR
    subgraph Pehredaar process
        SCH[scheduler] -->|due source| F[fetcher]
        F -->|raw bytes + metadata| N[normalize]
        N -->|canonical text + hash| SCH
        SCH -->|hash changed| D[differ]
        D -->|structured diff| S[summarizer]
        S -->|summary + importance| SCH
        SCH -->|alert| NT[notifier]
        BOT[bot commands] --> ST
        SCH <--> ST[(store / SQLite)]
        NT <--> ST
    end
    F -->|HTTPS, rate-limited| SSC[(ssc.gov.in API + PDFs)]
    F -->|exec| PDF[pdftotext]
    S -->|HTTPS| LLM[(LLM provider)]
    NT -->|Bot API| TG[(Telegram)]
    BOT <-->|long polling| TG
```

It's one Go binary and one SQLite file. Everything else is an external service.

---

## 4. Packages

```
cmd/pehredaar/          main: wiring, config, signal handling, subcommands (run, probe)
internal/
  config/               env vars + sources.yaml loading
  store/                Store interface, domain types, SQLite implementation, migrations
  fetcher/              HTTP client, robots.txt, per-host rate limit, conditional GET, PDF download
  normalize/            raw bytes → canonical text (per kind + per-source rules)
  differ/               canonical text A vs B → structured Diff
  summarizer/           Summarizer interface; template impl (also the fallback); LLM impl
  notifier/             Telegram: alert sending (rate limits, retries), bot command handlers
  outbox/               pending changes → summaries → routed alerts (§7)
  scheduler/            polling loop, worker pool, backoff, fetch→normalize→diff→save
  dashboard/            local operator web UI (html/template, read-mostly)
testdata/               saved JSON/PDF/HTML fixtures (shared by tests)
docs/                   this file and future design notes
```

**Dependency rule:** packages depend "downward" only. `scheduler` imports
the others; nothing imports `scheduler`. `store` defines the domain types
(`Source`, `Snapshot`, `Change`, ...) and imports no other internal package.
`main` is the only place where concrete implementations are chosen and wired
together. *Go note:* this keeps the import graph acyclic, which Go enforces
at compile time anyway, and makes every package testable on its own.

### 4.1 Key interfaces

*Go note:* the idiom is "accept interfaces, return structs", with interfaces
kept small and ideally defined by the consumer. We make an exception for
`store.Store`: the brief asks for it explicitly as the Postgres migration seam.
Consumers can still declare narrower interfaces (e.g. the bot only needs
subscription methods).

```go
// store
type Store interface {
    // sources
    UpsertSource(ctx context.Context, s Source) (Source, error)
    DueSources(ctx context.Context, now time.Time, limit int) ([]Source, error)
    RecordCheck(ctx context.Context, id int64, r CheckResult) error // timestamps, etag, failures
    // snapshots & changes
    LatestSnapshot(ctx context.Context, sourceID int64) (*Snapshot, error)
    SaveSnapshotAndChange(ctx context.Context, snap Snapshot, ch *Change) (Snapshot, error) // one tx
    PendingChanges(ctx context.Context) ([]Change, error)
    UpdateChange(ctx context.Context, ch Change) error
    // subscribers
    UpsertSubscriber(ctx context.Context, telegramID, chatID int64) (Subscriber, error)
    SetLanguage(ctx context.Context, telegramID int64, lang Language) error
    Subscribe(ctx context.Context, telegramID int64, target Target) error
    Unsubscribe(ctx context.Context, telegramID int64, target Target) error
    Subscriptions(ctx context.Context, telegramID int64) ([]Subscription, error)
    SubscribersFor(ctx context.Context, src Source) ([]Subscriber, error)
    // deliveries (idempotent notify)
    MarkDelivered(ctx context.Context, changeID, subscriberID int64) (first bool, err error)
}

// fetcher
type Fetcher interface {
    Fetch(ctx context.Context, req Request) (*Response, error)
}
type Request struct {
    URL          string
    ETag         string // sent as If-None-Match
    LastModified string // sent as If-Modified-Since
}
type Response struct {
    NotModified  bool   // 304: nothing else is populated
    Body         []byte
    ContentType  string
    ETag         string
    LastModified string
    FetchedAt    time.Time
}

// normalize
type Normalizer interface {
    Normalize(src store.Source, resp *fetcher.Response) (Doc, error)
}
type Doc struct {
    Text  string       // canonical, line-oriented text: the thing we hash and diff
    Items []FeedItem   // feeds only: parsed items, used for discovery + item-level diff
}

// differ (plain functions, no interface needed)
func Diff(old, new normalize.Doc) Result

// summarizer
type Summarizer interface {
    Summarize(ctx context.Context, in Input) (Summary, error)
}
type Summary struct {
    Text       string
    Importance Importance // Date | Eligibility | Fee | Other | Cosmetic
}

// notifier
type Notifier interface {
    Send(ctx context.Context, chatID int64, a Alert) error
}
```

---

## 5. Data model

Snapshots are only written when the hash **changes**. An unchanged check just
updates `sources.last_checked_at`. That keeps the DB small even when polling
often.

```mermaid
erDiagram
    sources ||--o{ snapshots : has
    sources ||--o{ changes : has
    sources ||--o{ sources : "parent (discovered PDFs)"
    snapshots ||--o{ changes : "old/new"
    subscribers ||--o{ subscriptions : has
    subscriptions }o--o| sources : "source target"
    changes ||--o{ deliveries : "sent to"
    subscribers ||--o{ deliveries : receives
```

| Table | Columns | Notes |
|---|---|---|
| `sources` | `id, url UNIQUE, kind (feed/pdf/html), category, language (en/hi), label, parent_id NULL, check_interval_sec, enabled, etag, last_modified, last_checked_at, next_check_at, consecutive_failures, last_error, created_at` | `parent_id` set for PDFs discovered from a feed. HTTP validators live here so conditional GETs survive restarts. |
| `snapshots` | `id, source_id, content_hash, text, fetched_at` | `text` is the **normalized** text. Index `(source_id, fetched_at DESC)`. |
| `changes` | `id, source_id, old_snapshot_id NULL, new_snapshot_id, kind (added/edited/removed), diff, importance, status, attempts, created_at` | `status`: `pending → summarized → notified`, or `suppressed` (cosmetic) / `failed`. |
| `change_summaries` | `change_id, language, text` | PK `(change_id, language)`. One LLM summary per language actually needed, generated lazily and cached. |
| `subscribers` | `id, telegram_id UNIQUE, chat_id, language (en/hi, default en), active, created_at` | `active=false` when a user blocks the bot. `language` = alert language. |
| `subscriptions` | `id, subscriber_id, source_id NULL, category NULL, created_at` | `CHECK ((source_id IS NULL) <> (category IS NULL))`. Unique per target. |
| `deliveries` | `change_id, subscriber_id, sent_at` | PK on both columns. Guarantees **at most one** alert per user per change. |

**Category** is the unit users subscribe to in v1 (**feed-level, decided**),
e.g. `ssc:notice-board`, `ssc:calendar`, `ssc:results`. A discovered PDF
inherits its parent's category and language, so subscribing to
`ssc:notice-board` covers its PDFs too. Per-exam subscriptions are deferred to v1.1.

**Languages and categories.** Each feed is watched twice (en + hi). Both
sources share one category, so a user subscribes to "Notice Board", not
"Notice Board (Hindi)". Who gets which alert:

| Subscriber language | Alerts from English sources | Alerts from Hindi sources |
|---|---|---|
| `en` | ✅, summary in English | ❌ (would mostly duplicate English notices, late) |
| `hi` | ✅, **summary written in Hindi** | ✅, summary in Hindi |

Rationale: the Hindi feed lags and misses notices, so a Hindi-preferring
user who only got Hindi-feed alerts would miss things like the 26 Oct date
corrigendum. Accepted cost: a Hindi user may get the same notice twice
(English version first, Hindi weeks later).

**Migrations:** plain numbered `.sql` files embedded with `//go:embed`, applied
in order and tracked in `schema_migrations`. No ORM. Queries use `?`
placeholders. The Postgres port changes placeholders to `$n` and a few types,
which is contained inside `store/sqlite` vs `store/postgres`.

---

## 6. Sources: curation and discovery

Curated sources live in a checked-in **`sources.yaml`**, synced into the DB
on startup (upsert by URL; sources removed from the file are disabled, not deleted):

```yaml
discovered_interval: 6h          # re-check interval for PDFs found in feeds
sources:
  - label: SSC Notice Board
    kind: feed
    category: ssc:notice-board
    languages: [en, hi]          # one source per language; {lang} -> english / hindi
    url: https://ssc.gov.in/api/general-website/portal/notice-boards?page=1&limit=10&...&language={lang}&...
    interval: 15m
    windowed: true               # page 1 of a longer list; items scroll (section 9)
    discover_attachments: true   # watch PDFs attached to the feed's items
    drop_lines: []               # optional per-source noise regexes
  - label: SSC Calendar
    kind: feed
    category: ssc:calendar
    languages: [en]              # the Hindi calendar is empty
    url: ...&year={year}&...     # {year} = current year; returns the whole year
    interval: 1h
    windowed: false              # complete list: a missing item is a removal
```

**v1 feeds (decided):** notice-boards, ssc-calendar, admit-card, answer-key,
results, tentative-vacancy in English; notice-boards, answer-key and results
also in Hindi (SSC's Hindi calendar, admit-card and tentative-vacancy feeds
are empty), so 9 curated sources plus discovered PDFs. Unknown yaml fields
are rejected, so typos fail at startup. Exact query strings are copied from what the
SSC site itself sends (§2) and verified with `probe` before going in the file.

**Discovery.** Whenever a feed's content changes (and at its baseline), the
attachments of its current items become `pdf` child sources (same host only, `parent_id` set). They're watched
on a slower interval (e.g. 6h) and retired after a configurable age (e.g. 120
days) so the watch set doesn't grow forever. This is *not* "add any URL":
only attachments of curated feeds on the curated host are followed.

**First sight is not a change.** The first snapshot of any source is a baseline
and never alerts, so startup or adding a source doesn't flood users.
For feeds, *new items after the baseline* do alert (they're how corrigenda arrive).

---

## 7. The check pipeline

```mermaid
sequenceDiagram
    participant S as scheduler
    participant F as fetcher
    participant N as normalize
    participant DB as store
    participant D as differ
    participant L as summarizer
    participant T as notifier

    S->>DB: DueSources(now)
    S->>F: Fetch(url, etag, last-modified)
    alt 304 Not Modified
        S->>DB: RecordCheck(ok, unchanged)
    else 200
        S->>N: Normalize(source, body)
        S->>DB: LatestSnapshot
        alt hash equal
            S->>DB: RecordCheck(ok, unchanged)
        else hash differs
            S->>D: Diff(old, new)
            alt diff empty after noise rules
                S->>DB: RecordCheck(ok) (save snapshot, no change)
            else real diff
                S->>DB: SaveSnapshotAndChange (status=pending) [one tx]
            end
        end
    end
    Note over S,T: separate stage, driven by pending changes
    S->>L: Summarize(diff, context)
    S->>DB: UpdateChange(summary, importance, status)
    S->>DB: SubscribersFor(source)
    loop each subscriber
        S->>DB: MarkDelivered (skip if already)
        S->>T: Send(alert)
    end
```

The cheap path (304, or same hash) touches no LLM and writes almost nothing.
That's the common case.

**Outbox pattern.** Detecting a change and notifying are **two stages**
joined by the `changes` table. A change is committed (with its snapshot, in
one transaction) *before* any LLM or Telegram call. If the process crashes or
the LLM is down, the change stays `pending` and is retried. Combined with
`deliveries`, a restart can never lose an alert or send it twice.

---

## 8. Normalization and noise filtering

Noise filtering is applied in layers. Each layer is cheap and testable against fixtures.

| Layer | Feed (JSON) | PDF | HTML (future) |
|---|---|---|---|
| 1. Extract | Parse JSON, keep only `keep_fields` per item (drops `isCache`, pagination, etc.) | `pdftotext -layout -enc UTF-8` (temp file, timeout, size cap) | goquery: keep `content_selector`, drop `script, style, nav, header, footer` + `drop_selectors` |
| 2. Canonicalize | One line per item, sorted newest-first by `createdAt` then id (content-determined, independent of API order), fields in fixed order | Unicode NFC, collapse runs of spaces, trim lines, drop empty lines | Same as PDF |
| 3. Per-source regex | Rarely needed | Strip page numbers ("Page 3 of 7"), print timestamps | Visitor counters, "Last updated …" |
| 4. Hash | SHA-256 of canonical text | ← same | ← same |
| 5. Diff-level filter | — | Ignore diffs that are whitespace/punctuation only | ← same |
| 6. LLM | Classifies leftovers as `cosmetic` → suppressed | ← same | ← same |

Why sort feed items: the API orders by `createdAt`, and canonical order
must not depend on response order. Pagination is handled in the differ
(section 9), not here.

Why `-layout`: it keeps table columns aligned, so date tables (like the
corrigendum we saw) diff as whole rows rather than scrambled words.

---

## 9. Diffing

Package `differ`. Entry point `Diff(kind, oldText, newText, Options) Result`,
where `Result{Kind, Text, Truncated, Ignored}`; `Result.Empty()` means "save
the snapshot, record no change". It works on canonical text only, so it can
diff any two stored snapshots without re-parsing source bodies.

**Feeds** (`differ.Feed`): items are matched by id (the first field of each
canonical line), giving `Added`, `Removed` and `Edited` (with the fields that
differ, compared as multisets). The differ never interprets fields; it only
relies on the line shape `id | createdAt | field | ...`.

Pagination: SSC feeds are fetched as page 1 (`limit=10`), so items scroll in
and out. For a source marked **windowed**:

- a missing item older than everything now on the page fell off the bottom:
  scrolled, not removed;
- a new item older than everything previously on the page came up from page
  2: scrolled, not added (a genuinely new notice is the newest item).

This replaced the count-based rule first planned, which misreads an addition
and a deletion in the same interval. The calendar returns the whole year, so
it is not windowed and every missing item is a removal. `Change.Kind` is
`added` if anything was added, else `edited`, else `removed`.

**Documents** (`differ.Text`): line diff with `sergi/go-diff`
(`DiffLinesToChars` → `DiffMain` → `DiffCharsToLines`, timeout disabled so
results don't depend on machine load), grouped into hunks with 2 lines of
context. A hunk is dropped as cosmetic when its deleted and inserted lines
are equal after removing whitespace and punctuation (catches pdftotext
re-wrapping and quote style). Letters, digits, combining marks (Devanagari
vowel signs) and punctuation *between digits* are kept, so `1.5` vs `15`
and `12.11.2026` vs `1211.2026` still count.

**Rendering and cap.** Feed blocks are `+ line`, `- line`, and
`~ id | headline` followed by `- field` / `+ field`. Document hunks use
`@@ -old +new @@` with `  `/`- `/`+ ` prefixes. Text is capped at 4 KB by
whole blocks plus "... N more not shown"; the stored diff is the capped text.

---

## 10. Summarization and importance

Implemented in `summarizer.Claude` (decided 2026-11-02): Anthropic's
**Claude Haiku 5.5** (`claude-haiku-5-5`, the cheapest current model; override
with `PEHREDAAR_LLM_MODEL`), effort `low`, via the official Go SDK. Used when
`ANTHROPIC_API_KEY` is set; otherwise alerts use the template (§11). The
`Summarizer` interface keeps the provider swappable.

- **Input:** source label, source type, output language, and the change.
  Feed changes are sent as the *English template rendering*, not the raw
  diff: the raw diff holds UTC timestamps (`2026-11-11T18:30:00Z` is 12 Nov in
  India) and the template has already turned them into IST dates. Document
  changes are sent as the diff text. The change is wrapped in `<change>` tags
  and the prompt says it is data, never instructions.
- **Output:** JSON `{"summary", "importance"}`, enforced by a JSON schema
  (structured outputs) and validated again: known importance, summary 1–400
  runes. Invalid output, an API error, a refusal or a cut-off answer is an
  error; the outbox retries and, after 3 attempts, falls back to the template.
- **Alert text:** the model's summary, a blank line, then the template's exact
  lines, so a reader can check the summary against the real old → new values.
- **Number check:** every digit run in the summary (Devanagari digits
  normalized, leading zeros ignored) must appear in the input. If not, the
  summary is dropped and the alert is the plain template with importance
  `other`: an invented date never reaches a user. This is code, not a prompt.
- **Cosmetic:** only edits of existing items (feed `~` blocks, document
  diffs) may be `cosmetic`. A new or removed notice classed cosmetic is turned
  into `other`, so one wrong label can never hide a new notice.
- **Language:** the model writes the summary directly in the subscriber's
  language. Summaries are cached per `(change, language)`.
- **Hindi PDFs** skip the model entirely: their text is garbled, and the
  template already says "open the PDF". Importance is `other`.
- **Alert policy:** `cosmetic` → `suppressed`. Everything else alerts. `other`
  covers things like exam city notices, which aspirants still want.
- **Not yet:** a new feed item's attached PDF is not read, so its summary is
  based on the headline and fields only. Reading the PDF is the next
  improvement.
- **Tuning:** `pehredaar summarize [id]` prints a stored change's alert
  without saving or sending it. Tests use a fake API server; none call Anthropic.

---

## 11. Telegram

Library: **`github.com/go-telegram/bot`**. It has no dependencies, tracks the
current Bot API, and is context-aware. It uses long polling, so no public URL or
webhook is needed while running on a laptop.

| Command | Behavior |
|---|---|
| `/start` | Register subscriber, pick alert language (English / हिन्दी), show `/watch` |
| `/language` | Change alert language |
| `/watch` | Inline keyboard of categories (✅ on watched ones); tap to subscribe |
| `/list`  | Current subscriptions with an inline "stop" button each |
| `/stop`  | Inline keyboard of current subscriptions to remove (`/stop all` to clear) |

Bot messages follow the subscriber's language. Button taps edit the message
in place rather than sending new ones. Category names are the `label`s from
`sources.yaml` (the first entry per category). Any other private message gets
the help text. Messages use Telegram's HTML parse mode, since only `<>&`
need escaping (MarkdownV2 reserves `.-()`, which are everywhere in notices).

Alert format:

```
🔔 SSC Notice Board
Exam dates revised: JE 2026 Paper-I now 2–6 Nov and 30 Nov–1 Dec;
SI Delhi Police CBE now 23–28 Nov.
📄 <link to PDF>
```

With an Anthropic key, the alert opens with Claude's summary (§10); the
template text below it is always included. The template is built from the diff:
"New notice / Notice updated / Notice removed: <headline>" with changed fields
paired (`Start date: 1 May 2026 → 1 Jul 2026`, timestamps shown in IST), or
for PDFs "Document updated: <label>" with up to 3 Was/Now lines. Hindi PDFs
are never quoted (garbled text). Links: the PDF itself, or the attachments of
new/edited feed items (max 3). Link previews are off.

Sending respects Telegram limits (≈30 msgs/s globally, 1/s per chat) with a
limiter. A 403 "bot was blocked" response marks the subscriber `active=false`;
any later message to the bot (e.g. `/start`) reactivates them.

---

## 12. Scheduling and concurrency

- One **ticker loop** (e.g. every 30s) asks the store for due sources
  (`next_check_at <= now`) and pushes them onto a channel.
- A **fixed worker pool** (N goroutines, default 4) consumes the channel and
  runs the check pipeline. *Go note:* a bounded pool, not one goroutine per
  source, caps concurrent requests and memory. Channels give natural backpressure.
- A separate **change-processor goroutine** drains `pending` changes
  (summarize → notify), so a slow LLM never delays fetching.
- The **bot** runs in its own goroutine (long polling).
- Everything shares one root `context.Context` from `signal.NotifyContext`.
  Ctrl+C cancels it, workers finish their current item, and `errgroup` waits
  for them, giving a clean shutdown.
- **Jitter:** `next_check_at = now + interval ± 10%`, so sources don't all
  fire on the same tick.

`probe` subcommand (dev tool, step 2): `pehredaar probe <url> [--twice 5m]`
fetches and normalizes and prints canonical text, hash, and (with `--twice`)
the diff between two fetches. This is how we measure noise on real sources
before trusting the pipeline.

---

## 13. Politeness and failure handling

**Politeness**

- `User-Agent: Pehredaar/0.1 (+https://github.com/akkuraina/pehredaar)`.
- robots.txt fetched per host, cached 24h (`temoto/robotstxt`). A 404 means
  allow all, which is SSC's case today. Disallowed URLs are skipped and logged.
- Per-host token bucket (`golang.org/x/time/rate`), default 1 request / 5s,
  burst 1. All workers share the limiter, so concurrency never means hammering
  one host.
- Conditional GET (`If-None-Match`, `If-Modified-Since`) on every request.
- Timeouts: 30s per request; body size caps (2 MB JSON/HTML, 20 MB PDF).

**Failures** (no user-facing alerts for any of these)

| Situation | Behavior |
|---|---|
| Timeout, 5xx, connection error | Temporary. Exponential backoff on `next_check_at` (×2, capped at 6h), `consecutive_failures++` |
| 429 / 503 with `Retry-After` | Honor `Retry-After` (capped) and slow that host's limiter |
| 403 / suspected block | Treat as temporary with long backoff. Log loudly |
| 404 / 410 | Permanent after K consecutive failures → disable source, log |
| Body is an error page / empty / JSON `statusCode != "200"` | **Not a change.** Counts as a failed check. Never snapshot it (otherwise "page → error → page" alerts twice) |
| `pdftotext` fails / no text (scanned PDF) | Mark source as `needs-ocr`, skip. No alert |
| LLM failure | Change stays `pending`, retried. Fallback template after N attempts (§10) |
| Telegram failure | Delivery is **marked, then sent** (decided 2026-11-02): no user ever gets an alert twice, at the cost of possibly losing one. `Send` retries flood control (429, honoring `retry_after`, capped at 1 min) and network/5xx errors up to 4 attempts; 400s aren't retried. 403 marks the subscriber inactive |
| Bot off for a while | Changes stay `pending`. On restart, ones older than 72h are closed without alerting, so users aren't flooded with stale news |

Operator visibility: structured logs via `log/slog`. Later, an optional
`ADMIN_CHAT_ID` gets a daily health digest (sources failing, changes sent).

---

## 14. Configuration and secrets

All secrets are env vars, loaded once in `config`, never logged, and never committed (`.env` is gitignored). `pehredaar run` also reads a `.env` file in the working directory (`KEY=VALUE` lines, `#` comments, optional quotes); a variable already set in the real environment wins over the file. Only the names of loaded variables are logged.

| Var | Purpose | Default |
|---|---|---|
| `PEHREDAAR_DB` | SQLite path | `./data/pehredaar.db` |
| `PEHREDAAR_SOURCES` | Sources file | `./sources.yaml` |
| `TELEGRAM_BOT_TOKEN` | Bot token. Unset: `run` warns and runs without the bot and outbox (changes are still detected, and stay pending) | unset |
| `ANTHROPIC_API_KEY` | Claude summaries (§10). Unset: template alerts | unset |
| `PEHREDAAR_LLM_MODEL` | Claude model for summaries | `claude-haiku-5-5` |
| `PDFTOTEXT_PATH` | Override binary | `pdftotext` on PATH |
| `PEHREDAAR_WORKERS` | Pool size | `4` |
| `PEHREDAAR_HTTP` | Dashboard address (loopback only) | `127.0.0.1:8787` |

SQLite opened with WAL mode and `busy_timeout`, since the bot and the
workers write concurrently. Driver: `modernc.org/sqlite` (pure Go, no cgo).

---

## 15. Testing strategy

- **normalize:** table-driven tests over `testdata/` fixtures (saved SSC JSON
  pages, sample PDFs) with **golden files** (`*.golden.txt`, regenerated with
  `go test -update`). Includes "same content, different noise → same hash" cases,
  and a Hindi feed + Hindi PDF fixture (garbled-but-stable extraction).
- **differ:** pairs of fixtures, e.g. feed with one item added, PDF with a
  date changed, pagination churn, whitespace-only edit.
- **fetcher:** `httptest.Server` for 304s, Retry-After, robots.txt, timeouts,
  size caps. No live network in `go test`.
- **store:** tests against a temp-file SQLite. The same test suite will later
  run against Postgres to prove the interface holds.
- **scheduler:** end-to-end with a fake fetcher and a temp-file store.
- **outbox:** real store, fake notifier and summarizer. Asserts "one change →
  one alert per subscriber", even across a simulated crash; language routing;
  blocked users; summarizer fallback; cosmetic suppression.
- **notifier:** command logic against a real store, `Send` against a fake
  Bot API. No test talks to Telegram.
- **summarizer:** template against diff fixtures; Claude against a fake
  Messages API (`httptest`): request shape, number check, cosmetic rule,
  Hindi PDF skip, every error path. No test talks to Anthropic.
- PDF tests need `pdftotext` and are skipped (`t.Skip`) if it's missing.

---

## 16. Open questions

Decided (2026-10-31): feed-level subscriptions in v1 (per-exam in v1.1);
the six feeds in §6; English **and** Hindi.

1. **In-place PDF edits vs new notices**: measure with the probe before
   tuning PDF poll frequency.
2. **Hindi PDF text**: does poppler extract it better than xpdf? Is OCR worth it in v1.1?
3. ~~LLM provider and model~~: decided 2026-11-02, Claude Haiku 5.5 (§10).
4. **Hosting**: laptop for development. A small Linux VPS later (Go cross-compiles from Windows).

---

## 17. Build milestones

| # | Milestone | Done when |
|---|---|---|
| 1 | `store` + migrations | Interface + SQLite impl, tests green |
| 2 | `fetcher` + `normalize` + `probe` | Probe shows stable hashes on live SSC feeds/PDFs across fetches. Fixtures saved |
| 3 | `differ` + noise layers | Fixture diffs are correct. Pagination churn and whitespace produce no change |
| 4 | `notifier` + bot | `/watch` `/list` `/stop` work. Templated alerts sent end-to-end |
| 5 | `summarizer` | LLM summaries + importance. Cosmetic suppressed. Fallback path tested |
| 6 | `scheduler` hardening | Backoff, discovery/retirement, crash-safety test |

---

## 18. Local operator dashboard

Not part of the user product (users only ever see Telegram). It exists so the
operator can watch the pipeline work while it is being built and run.

- Served by `pehredaar run` next to the scheduler (one process, `errgroup`),
  at `http://127.0.0.1:8787`. It has no login, so `run` refuses any
  non-loopback address. Cross-site POSTs are rejected
  (`http.CrossOriginProtection`).
- Pages: overview (feeds, failing sources, recent changes, activity log,
  discovered PDFs; auto-reloads every 30s), source detail (metadata, "check
  now", snapshots, latest canonical text, child PDFs), change (coloured
  diff), snapshot (raw canonical text).
- Reads through a narrow `dashboard.Store` interface (no writes); the
  activity log is the scheduler's in-memory ring of the last 300 checks,
  so it resets on restart.
- Server-rendered `html/template` with embedded templates, no JS framework;
  all stored text is auto-escaped.
