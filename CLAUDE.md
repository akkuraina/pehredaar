# Pehredaar

"The one who keeps watch." A notice change-tracker for Indian exam aspirants.
Tagline: *They changed it quietly. Pehredaar caught it.*

Government exam notifications, recruitment PDFs and scholarship pages get edited
after publication (last dates, eligibility, age limits, fees, corrigenda).
Pehredaar watches curated pages/PDFs and sends a plain-language Telegram alert
describing exactly what changed, e.g. "Last date moved from 12 Nov to 20 Nov;
age limit changed from 27 to 25."

## Users and interface

Exam aspirants. The Telegram bot is the entire UI in v1 (no web frontend).
Commands: `/watch`, `/list`, `/stop`.

## Pipeline

1. Scheduler fetches each watched URL every N minutes (goroutines, concurrent).
2. Normalize: strip noise (visitor counters, timestamps, ads, nav chrome) and
   extract text. **Noise filtering is the hardest quality problem**; otherwise
   every fetch looks like a change.
3. Hash normalized content; if unchanged vs. last snapshot, stop (common case).
4. If changed, diff against the previous snapshot.
5. LLM turns the diff into a 1–2 sentence summary and classifies importance:
   `date`, `eligibility`, `fee`, `cosmetic`. Cosmetic changes do not alert.
6. Notify subscribers via Telegram.

## MVP scope

- One exam family: **SSC** (branch per additional exam family later).
- 10–20 hand-curated source URLs. No "add any URL" yet; curation allows
  per-site noise rules.
- Politeness: respect robots.txt, per-host rate limiting, clear User-Agent,
  tolerate sites being down/blocking without crashing or spamming alerts.

## Stack

- Go for all backend code. The author is learning Go: write idiomatic,
  well-structured Go and briefly explain non-obvious choices.
- `net/http` + `goquery` for HTML. Add `chromedp` only if a specific source
  needs JS rendering.
- PDFs: shell out to `pdftotext`. Note: the dev machine (Windows) has the
  **xpdf** build (v4.00), not poppler. Only rely on flags common to both
  (`-layout`, `-enc UTF-8`).
- `sergi/go-diff` for diffing (line-level mode).
- Storage: SQLite via `modernc.org/sqlite` (pure Go, no cgo) behind a `Store`
  interface so Postgres can be swapped in later.
- Telegram: `github.com/go-telegram/bot` (zero deps, current Bot API, context-based).
- LLM behind an interface so the provider is swappable.
- Secrets (bot token, LLM keys) come from environment variables. Never commit them.

## Layout

```
cmd/pehredaar/main.go
internal/
  fetcher/      # HTTP + PDF fetching, robots.txt, rate limiting
  normalize/    # strip noise, extract text
  differ/       # compare snapshots
  summarizer/   # LLM calls
  notifier/     # Telegram
  store/        # DB access (interface + SQLite impl)
  scheduler/    # polling loop
```

## Core tables

- `sources` (id, url, type, category, check_interval)
- `snapshots` (id, source_id, content_hash, text, fetched_at)
- `changes` (id, source_id, old_snapshot_id, new_snapshot_id, summary, importance)
- `subscribers`, `subscriptions` (telegram_id; exactly one of source or category)

## Build order

1. Data model and store.
2. Fetcher + normalizer, tested against real exam sites.
3. Differ with noise filtering.
4. Telegram bot flow.
5. LLM summaries and importance classification.

## Verified source notes (SSC, checked 2026-10-30)

- `ssc.gov.in` is an Angular SPA: the HTML is an empty shell, so scraping it
  with goquery yields nothing. `robots.txt` returns 404 (no restrictions).
- Content comes from a JSON API, e.g.
  `GET https://ssc.gov.in/api/general-website/portal/notice-boards?page=1&limit=10&contentType=notice-boards&key=createdAt&order=DESC&isAttachment=true&language=english&attributes=id,headline,examId,contentType,redirectUrl,startDate,endDate,language,createdAt`
  Other `contentType` values seen in the app bundle: `admit-card`, `answer-key`,
  `results`, `ssc-calendar`, `browse-exam`, `tentative-vacancy`, `syllabus`.
  Requests without query params return `{"statusCode":"203","error":"Query params are missing"}`.
- Responses carry a weak `ETag`; `Cache-Control: no-cache`.
- Attachments: `https://ssc.gov.in/api/attachment/<path>` (path from JSON,
  backslashes → `/`). PDFs send `Last-Modified` and `ETag`; `pdftotext -layout`
  extracts them cleanly (corrigenda are text PDFs, not scans, in samples seen).

## Conventions

- Propose a plan and get a go-ahead before large changes.
- Tests for normalizer and differ use saved HTML/PDF fixtures in `testdata/`;
  tests never hit live sites.
- Small, focused commits on `main`.
- Do not invent facts about government sites (URL structure, update patterns).
  Fetch the page and show what it actually looks like.
