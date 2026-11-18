# Pehredaar (पहरेदार)

*"The one who keeps watch."* **They changed it quietly. Pehredaar caught it.**

Government exam notices get edited after they're published: last dates move,
age limits change, fees are revised, corrigenda appear. Aspirants usually
find out late, or not at all. Pehredaar watches the official SSC notice feeds
and tells you on Telegram or by email exactly what changed, for example:

> 🔔 **SSC Calendar**
> The CGL 2026 exam start date has moved from 1 May 2026 to 1 Jul 2026.
>
> Notice updated: Combined Graduate Level Examination, 2026
> • Start date: 1 May 2026 → 1 Jul 2026

Pehredaar has a persona: a sharp-eyed owl. *"I don't blink. You don't have
to."* People join through a short survey on the website, pick their exams and
how to be told (Telegram, email or both), and get a live feed of what the owl
has caught.

## What it does today

Status: milestones 1–6 of the [build plan](docs/ARCHITECTURE.md#17-build-milestones)
are done, plus the public website and email alerts
([architecture §20](docs/ARCHITECTURE.md#20-public-website-and-email-built-2026-11-17)).
It is deployed at https://pehredaar.in (Render).

**Watches 9 SSC feeds** (from `ssc.gov.in`'s own JSON API):

| Feed | English | Hindi | Checked every |
|---|---|---|---|
| Notice Board | ✅ | ✅ | 15 min |
| Exam Calendar | ✅ | (SSC's Hindi feed is empty) | 1 h |
| Admit Cards | ✅ | (empty) | 1 h |
| Answer Keys | ✅ | ✅ | 30 min |
| Results | ✅ | ✅ | 30 min |
| Tentative Vacancies | ✅ | (empty) | 1 h |

**And 4 notice pages from other exam bodies** (read by the page reader,
[architecture §8](docs/ARCHITECTURE.md#8-normalization-and-noise-filtering)):

| Exam | Source | Notes |
|---|---|---|
| JEE Main | nta.ac.in, "Latest @ NTA" box | jeemain.nta.nic.in blocks bots; filtered to JEE, newest 15 |
| NEET UG | nta.ac.in, same box | neet.nta.nic.in blocks bots; filtered to NEET, newest 15 |
| IBPS | ibps.in, CRP updates | PO, SO, Clerk, RRB |
| ICSE / ISC | cisce.org, notifications archive | tenders filtered out |

All English, checked every 30 min. UPSC (and NDA, CDS) and CBSE block
Pehredaar's User-Agent, and we don't disguise it
([§19](docs/ARCHITECTURE.md#19-beyond-ssc-decided-2026-11-17-not-built-yet)).

It also watches the **PDFs attached** to items in those feeds (checked every
6 h), so an edited corrigendum PDF is caught even when the feed itself
doesn't change.

**For each check:**

1. **Fetch** politely: a clear User-Agent, robots.txt respected, at most one
   request every 5 s per host, and conditional requests (so an unchanged
   feed costs SSC a ~60 ms "304 Not Modified").
2. **Normalize**: reduce the JSON or PDF (via `pdftotext`) to canonical text,
   dropping noise like pagination and volatile fields, so only real changes
   count.
3. **Compare**: if the text's hash is unchanged, stop (the usual case).
   Otherwise diff it against the last snapshot. Feed diffs are per notice
   (new / edited / removed); PDF diffs are by line.
4. **Store** the change. Then, as a separate stage:
5. **Summarize**: if an Anthropic key is set, Claude Haiku writes a
   one- or two-sentence summary and classifies the change as `date`,
   `eligibility`, `fee`, `other` or `cosmetic`. Cosmetic changes are not
   sent. The exact old → new lines are always shown under the summary.
   Without a key, the alert is just those lines.
6. **Send** the alert to everyone watching that feed, in their language
   (English or Hindi).

Safety rules built in:

- **No invented numbers.** If Claude's summary mentions a number that isn't
  in the change, the summary is thrown away and the plain lines are sent.
- **No double alerts.** A delivery is recorded before it is sent, so a crash
  or restart never sends the same alert twice.
- **No false alarms.** A site being down, an error page or an empty response
  is never treated as a change. The first time a source is seen is a
  baseline, not a change.
- **No stale floods.** Changes older than 72 h when the bot comes back online
  are not sent.
- **Dead links stop costing requests.** A source that returns 404 five times
  in a row, or a scanned PDF with no text, is switched off (the dashboard
  says why). PDFs found in feeds are retired after 120 days.
- **Never silent.** If the Claude API fails, the plain alert is sent after 3
  attempts.

### The website

Run `pehredaar run` and open http://127.0.0.1:8788.

| Page | What it does |
|---|---|
| Home (`/`) | The owl (its eyes follow your pointer), a live "watching now" pulse, a real old → new catch animated, and a ticker of caught changes that updates by itself |
| Join (`/join`) | Four questions, one at a time: which exams (SSC, JEE, NEET, IBPS and ICSE/ISC are live; the rest are "coming soon"), which kinds of notices, Telegram / email / both, English or Hindi |
| Your watch (`/feed`) | Changes caught for your feeds (new ones appear live), what the official notice board lists right now, "coming soon" exams, your channels, and "delete everything about me" |
| Sign in (`/login`) | No passwords: an emailed link, or `/feed` in the Telegram bot |
| A change (`/c/<id>`) | One change in full, with the official PDF; linked from alert emails |

Everything on the site is real data from the database; nothing is made up.
Pages work without JavaScript, and motion is off for people who ask their
device for reduced motion.

### Email

Email alerts go out through [Resend](https://resend.com) once
`RESEND_API_KEY` is set. An address gets no alerts until its owner clicks the
confirmation link. Every alert has a one-click unsubscribe (Gmail and Yahoo
require it). Until you verify a domain in Resend, it only delivers to your
own Resend account address.

### The bot

| Command | What it does |
|---|---|
| `/start` | Choose English or Hindi, then see help. From the website's "Open Telegram" button, it links your account to your web sign-up |
| `/watch` | Tap buttons to pick feeds to watch (✅ marks ones you already watch) |
| `/list` | Show what you're watching |
| `/stop` | Stop watching a feed; `/stop all` stops everything |
| `/language` | Switch between English and Hindi |
| `/feed` | Get a one-time link that signs you in to the website |
| `/help` | Show help |

English subscribers get alerts from the English feeds. Hindi subscribers get
alerts from both, with summaries written in Hindi.

### Not done yet

- Hosting on a server (it runs only while `pehredaar run` is running).
- A sending domain for email, and handling bounced addresses (needs a public URL).
- The website's own pages in Hindi (alerts and the bot already are).
- Reading the PDF attached to a *new* notice to summarize it; today a new
  notice's summary is based on its headline and fields.
- Per-exam subscriptions (e.g. only "CGL 2026"); v1 is per feed.
- Exam bodies beyond SSC: UPSC/NDA, IBPS, SSB, NTA (JEE, NEET), CBSE,
  CISCE, CLAT, CAT, GRE, GMAT are planned; state boards and PSCs later
  ([architecture §19](docs/ARCHITECTURE.md#19-beyond-ssc-decided-2026-11-17-not-built-yet)).

## Setup

### Requirements

- **Go 1.27** or newer.
- **`pdftotext`** on your `PATH` (or set `PDFTOTEXT_PATH`):
  - Windows: the Xpdf command-line tools from xpdfreader.com.
  - Linux: `sudo apt install poppler-utils`.
  - macOS: `brew install poppler`.
- **A Telegram bot token** (optional): without it, no Telegram alerts.
- **A Resend API key** (optional): without it, no email sign-ups or alerts.
- **An Anthropic API key** (optional): without it, alerts don't get a summary.

### 1. Build

```sh
git clone https://github.com/akkuraina/pehredaar.git
cd pehredaar
go build -o bin/pehredaar.exe ./cmd/pehredaar   # on Linux/macOS: -o bin/pehredaar
```

### 2. Create the Telegram bot

1. In Telegram, open **@BotFather** (blue verified tick) and send `/newbot`.
2. Choose a display name (e.g. `Pehredaar`) and a username ending in `bot`.
3. Copy the token it gives you. Treat it like a password; if it leaks,
   send `/revoke` to BotFather for a new one.
4. Optional: `/setdescription`, `/setuserpic`, and `/setjoingroups` →
   Disable. Don't bother with `/setcommands`: Pehredaar sets its own command
   menu (in English and Hindi) at startup.

### 3. Add your secrets to `.env`

Create a file named `.env` in the project folder (it is gitignored, never
commit it):

```
TELEGRAM_BOT_TOKEN=123456789:AA...
ANTHROPIC_API_KEY=sk-ant-...
RESEND_API_KEY=re_...
```

Get an Anthropic key at console.anthropic.com and a Resend key at
resend.com/api-keys. All three lines are optional.
Values already set in your shell's environment take priority over `.env`.

### 4. Run

```sh
bin/pehredaar.exe run
```

You should see, in the log:

```
loaded .env vars=[TELEGRAM_BOT_TOKEN ANTHROPIC_API_KEY]
sources synced ... curated=9
email via Resend from="Pehredaar <onboarding@resend.dev>"
summaries by Claude model=claude-haiku-5-5
dashboard listening url=http://127.0.0.1:8787
website listening url=http://127.0.0.1:8788
telegram bot running
```

Then open the website at http://127.0.0.1:8788 and join, or message your bot
`/start` and `/watch`. Stop with Ctrl+C.

- **Website:** http://127.0.0.1:8788, the public site (§ "The website" above).
  By default it only listens on this machine; set `PEHREDAAR_PUBLIC` and
  `PEHREDAAR_BASE_URL` when deploying.

- **Dashboard:** http://127.0.0.1:8787 shows every source, its last checks,
  snapshots, changes (as coloured diffs) and an activity log, with a "check
  now" button. It's for the operator only, has no login, and only listens on
  localhost.
- **State** lives in `data/pehredaar.db` (SQLite). Delete it to start fresh.
- The first check of each source is a baseline, so expect your first alert
  only after SSC actually changes something.

### Developer tools

```sh
bin/pehredaar.exe probe <url>        # fetch + normalize one URL, show what Pehredaar sees (no DB)
bin/pehredaar.exe summarize          # list recent changes
bin/pehredaar.exe summarize <id>     # show the alert for one change (add -lang hi); sends nothing
go test ./...                        # all tests; none touch SSC, Telegram or Anthropic
```

### Configuration

| Variable | Purpose | Default |
|---|---|---|
| `TELEGRAM_BOT_TOKEN` | Bot token. Unset: no bot, no alerts (changes are still detected) | unset |
| `ANTHROPIC_API_KEY` | Claude summaries. Unset: plain alerts | unset |
| `PEHREDAAR_LLM_MODEL` | Claude model | `claude-haiku-5-5` |
| `PEHREDAAR_DB` | SQLite file | `data/pehredaar.db` |
| `PEHREDAAR_SOURCES` | Sources file | `sources.yaml` |
| `PEHREDAAR_WORKERS` | Concurrent checks | `4` |
| `PEHREDAAR_HTTP` | Dashboard address (localhost only) | `127.0.0.1:8787` |
| `PEHREDAAR_PUBLIC` | Website address | `0.0.0.0:$PORT` if `PORT` is set, else `127.0.0.1:8788` |
| `PEHREDAAR_TRUSTED_PROXIES` | Reverse proxies whose `X-Forwarded-For` is believed (IPs, CIDRs, or `private`). Needed behind any proxy, or all visitors share one rate limit | unset |
| `PEHREDAAR_BASE_URL` | The website's public URL, used in email and Telegram links | `http://` + `PEHREDAAR_PUBLIC` |
| `RESEND_API_KEY` | Email via Resend. Unset: no email sign-ups or alerts | unset |
| `PEHREDAAR_EMAIL_FROM` | Email sender | `Pehredaar <onboarding@resend.dev>` |
| `PDFTOTEXT_PATH` | Path to `pdftotext` | found on `PATH` |

Sources are curated by hand in [`sources.yaml`](sources.yaml) and synced into
the database at startup.

## Deploying

Pehredaar is one always-on process with a SQLite file, so it needs a host
that keeps a process running and a disk that survives deploys, and **exactly
one instance** (two would both poll the Telegram bot and fight over the
database). Serverless platforms like Vercel don't fit. Before choosing a
region, check that the government sites answer from it:
`pehredaar probe <feed URL>` from a trial machine there.

Whichever host:

- Set `PEHREDAAR_BASE_URL` to the public `https://` address (used in email
  and Telegram links; it also turns on `Secure` cookies).
- Set `PEHREDAAR_TRUSTED_PROXIES`, since every option below puts a proxy in
  front: `127.0.0.1` behind Caddy on the same machine, `private` on Railway
  or Render.
- Point the platform's health check at `/healthz` (200 while the database
  answers).
- The operator dashboard only listens on localhost inside the machine or
  container. On a VPS, reach it with `ssh -L 8787:127.0.0.1:8787 <server>`.

**Docker (Railway, Render, or any container host).** The `Dockerfile`
builds a static binary and adds `pdftotext` (poppler). Mount a persistent
volume at `/data`; the site listens on `$PORT`, which these platforms set.

```sh
docker build -t pehredaar .
docker run -p 8788:8788 -v pehredaar-data:/data --env-file .env pehredaar
```

The container runs as a non-root user. If your platform mounts the volume
owned by root and the database can't be created, check its docs for running
as root or fixing volume permissions.

**A Linux VPS.** Build on Windows with
`$env:GOOS="linux"; $env:GOARCH="amd64"; go build -o pehredaar ./cmd/pehredaar`,
copy it to the server with `sources.yaml` and `.env`, install
`poppler-utils`, and use [`deploy/pehredaar.service`](deploy/pehredaar.service)
(systemd, with setup steps in the file) and
[`deploy/Caddyfile`](deploy/Caddyfile) (HTTPS and the reverse proxy).

## Project layout

```
cmd/pehredaar/     CLI: run, probe, summarize
internal/
  config/          sources.yaml loading and sync, .env loading
  fetcher/         HTTP + PDF fetching, robots.txt, per-host rate limits
  normalize/       raw JSON/PDF → canonical text, noise removal
  differ/          compare snapshots
  scheduler/       polling loop and worker pool
  outbox/          pending changes → summaries → alerts
  summarizer/      change → alert text (Claude, with a template fallback)
  notifier/        Telegram bot: commands and sending
  email/           Resend client and email templates
  auth/            one-time sign-in tokens, signed cookies and unsubscribe links
  exams/           the exams the survey offers
  web/             the public website (templates, CSS, JS embedded in the binary)
  store/           Store interface + SQLite implementation
  dashboard/       local operator web UI
deploy/            systemd unit and Caddyfile for a VPS
Dockerfile         container image (binary + pdftotext)
docs/
  ARCHITECTURE.md  full design and decisions
  SCALING.md       plan for thousands of users
```

## Scaling to thousands of users

Most of Pehredaar costs the same at 100 users as at 100,000: SSC is fetched
once per source however many people watch it, and Claude writes one summary
per change per language. The one thing that grows with users is **sending**,
and Telegram limits a bot to about 30 messages per second. At that rate,
10,000 subscribers take ~7 minutes to all receive one alert, and 50,000
take ~33 minutes.

The plan, in short:

1. **Up to ~5,000 users:** move to a small Linux VPS with nightly database
   backups, a daily health report to the operator, and a few small fixes
   (prune old delivery records and idle rate limiters, rate-limit commands
   per user). One process and SQLite are still plenty.
2. **~5,000–50,000:** post feed-wide alerts to **Telegram channels** (one per
   feed and language), which reach any number of members at once, for free.
   Keep DMs for personalized alerts, and send date changes before less
   urgent ones.
3. **50,000+, more exam bodies:** move to Postgres (the `Store` interface
   allows it), split into scheduler / sender / bot processes, use a webhook
   for the bot, and add metrics.

Each step has a measurable trigger. Details, numbers and trade-offs are in
[docs/SCALING.md](docs/SCALING.md).
