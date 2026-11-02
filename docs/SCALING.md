# Scaling Pehredaar to thousands of users

This is a plan, not a description of the running system. Today Pehredaar is
one Go process with SQLite, built and tested with a handful of users. This
document says what breaks first as users grow, how to tell, and what to do at
each stage. Each step is only worth doing once its trigger is reached.

## What grows with users, and what doesn't

Most of the system does **not** get more expensive as users join:

| Part | Grows with | Cost at 100 users vs 100,000 |
|---|---|---|
| Fetching SSC | Number of *sources* (9 feeds + discovered PDFs) | Identical. A notice is fetched once, however many people watch it |
| Diffing, storing snapshots | Number of sources and changes | Identical |
| LLM summaries | Number of *changes* × languages in use (at most 2) | Identical: one summary per change per language, cached |
| Bot commands (`/watch`, …) | Users, but each is one tiny DB read/write | Negligible until very large |
| **Sending alerts** | **Subscribers × changes** | **This is the bottleneck** |
| `deliveries` table | Subscribers × changes | Grows without bound today |

So scaling Pehredaar is mostly about **fan-out**: getting one change to many
chats quickly, within Telegram's limits.

## The hard limit: Telegram

A bot may send roughly **30 messages per second** in total and about **1 per
second to one chat**. Pehredaar stays a little under: 25/s globally, 1/s per
chat (`internal/notifier/bot.go`). That makes fan-out time:

| Subscribers to one change | Time until the last one gets it, at 25/s |
|---|---|
| 1,000 | 40 seconds |
| 10,000 | ~7 minutes |
| 50,000 | ~33 minutes |
| 100,000 | ~67 minutes |

Adding servers does not help: the limit is per bot token, not per machine.
For a "the last date moved" alert, half an hour late is still useful but
not good. This table is what drives the stages below.

## Stage 1 — first real users (up to ~5,000)

Everything still fits in one process and SQLite. The work is about running
it reliably, not about scale.

1. **Move off the laptop.** A small Linux VPS (1 vCPU, 1 GB RAM is plenty),
   running the single binary under systemd with `Restart=always`. Install
   `poppler-utils` for `pdftotext`. Cross-compile from Windows with
   `GOOS=linux GOARCH=amd64 go build`. Keep the dashboard on localhost and
   reach it through an SSH tunnel; it has no login.
2. **Back up the database.** Everything is in one SQLite file. Take a nightly
   online backup (`sqlite3 pehredaar.db ".backup …"`) and copy it off the
   machine, or stream it continuously with Litestream.
3. **Operator alerts.** The planned `ADMIN_CHAT_ID` daily digest: sources
   failing, changes found, alerts sent and lost, LLM fallbacks. Without it,
   a broken source fails silently.
4. **Measure fan-out latency.** Log, per change, the time from detection to the
   last delivery. This number tells you when Stage 2 is due.
5. **Small fixes that matter at this size:**
   - The per-chat rate limiters in `notifier.Bot` are kept in a map that is
     never pruned: one entry per chat ever messaged. Evict limiters idle for
     more than a minute.
   - Prune `deliveries`: a row is only needed while its change is unfinished
     (it prevents a double send after a crash). Delete rows for changes
     notified more than 7 days ago.
   - Command abuse: rate-limit commands per user (e.g. 1/s), so one person
     spamming `/watch` can't slow the bot for everyone.

**Trigger for Stage 2:** fan-out latency regularly over ~5 minutes, or one
feed passing ~5,000 subscribers.

## Stage 2 — thousands to tens of thousands (~5,000 to ~50,000)

Fan-out time is now the problem. Options, in the order I'd try them:

### 2a. Telegram channels for feed alerts (recommended)

In v1, subscriptions are per *feed*, so every subscriber of a feed in one
language receives the *identical* message. That is exactly what a Telegram
channel is for: the bot posts once, and Telegram delivers it to every
member, with no per-message limit and at no cost.

- One channel per (feed, language): at most 9 today (e.g. "Pehredaar: SSC
  Notice Board", "पहरेदार: SSC सूचना पट्ट").
- The bot is an admin of each channel and posts the alert there; `/watch`
  replies with the channel link instead of (or as well as) a DM subscription.
- Delivery time stops depending on the number of users.
- Trade-offs: Pehredaar no longer knows who receives what (Telegram shows
  only member counts), and per-user settings can't apply to a channel post.
  That's acceptable for feed-wide alerts.
- When per-exam subscriptions arrive (v1.1, e.g. "only CGL 2026"), those
  stay as DMs: they are personalized, and each exam has far fewer
  subscribers than a whole feed. Feed-wide → channel, per-exam → DM.

### 2b. Send the most important alerts first

If DMs remain, order the send queue by importance (`date`, `eligibility`,
`fee` before `other`) instead of one change at a time in arrival order. Today
`outbox` finishes one change for all subscribers before starting the next,
oldest subscriber first (`ORDER BY s.id`). A date change shouldn't wait
behind a 30-minute "result published" broadcast, and the same people
shouldn't always be last: rotate the starting point between changes.

### 2c. Paid broadcasts

The Bot API lets a bot pay (in Telegram Stars, per message) to send
broadcasts far faster than the free limit (the `allow_paid_broadcast`
option, up to about 1,000 messages/s). It has eligibility conditions and a
running cost. Check the current Bot API docs and pricing before relying on
it. Worth considering only if channels don't fit.

### Still not needed at this stage

- **Postgres.** One process writing to SQLite in WAL mode handles this
  comfortably: the writes are one row per delivery and a few per check.
- **More servers.** The bottleneck is Telegram's limit, which more machines
  don't raise.
- **A message queue.** The `changes` + `deliveries` tables already are a
  durable queue (the outbox pattern).

**Trigger for Stage 3:** more exam families (UPSC, IBPS, state PSCs),
per-exam subscriptions with many DMs, a need for zero downtime during
deploys, or SQLite write contention showing up as `database is locked`
errors in the logs.

## Stage 3 — many exam families, ~50,000+ users

Now it's worth splitting the process up. The code is already organized for it:
the stages talk through the database, and the database sits behind the
`store.Store` interface.

1. **Postgres instead of SQLite.** Implement `store.Store` for Postgres. The
   same storage test suite (`storetest`) proves the new implementation
   behaves identically. Use a managed Postgres with point-in-time recovery.
2. **Split into three roles**, each its own process, one binary with
   subcommands:
   - `scheduler`: fetch, normalize, diff, write changes. One instance is
     enough for hundreds of sources (per-host politeness limits fetching
     anyway, not CPU).
   - `sender`: the outbox. Claims deliveries with
     `SELECT … FOR UPDATE SKIP LOCKED` so several instances never send the
     same alert. Since the Telegram limit is per bot, the global rate limit
     must be shared between instances (a token bucket in Postgres or
     Redis), or keep a single sender.
   - `bot`: command handling via a **webhook** instead of long polling, so
     it can run behind a load balancer with more than one instance and
     deploy without gaps.
3. **Per-exam subscriptions** need an exam index. SSC items carry `examId`;
   store it per item so a change can be routed to people watching that exam.
4. **More sources** (other exam bodies) mean more per-host rate limiters and
   more `sources.yaml` entries. Each new site needs its own investigation
   (see the SSC notes in `CLAUDE.md`); the fetch side scales with sources,
   not users.
5. **Observability:** metrics (Prometheus or similar) for checks per minute,
   failures per source, changes, LLM latency and fallbacks, delivery
   latency, and blocked users. Alert the operator on stuck changes (pending
   for over 30 minutes).

## Costs, roughly

| Users | Hosting | LLM | Telegram |
|---|---|---|---|
| ≤ 5,000 | One small VPS (a few dollars a month) | Pennies a month: a few changes a day, Claude Haiku | Free |
| ≤ 50,000 | Same VPS; channels for feed alerts | Same: LLM cost depends on changes, not users | Free with channels; paid broadcasts cost per message |
| 50,000+ | A few small servers + managed Postgres | Grows only with exam families and languages | Free with channels |

## Things that must stay true at any size

- **An alert is never sent twice** (mark-then-send, the `deliveries` key or
  its Postgres equivalent).
- **A source being down never alerts anyone**, and an error page never
  becomes a snapshot.
- **Politeness toward the sites:** one fetch per source per interval, no
  matter how many users watch it. Growth in users must never mean more load
  on ssc.gov.in.
- **No invented numbers in alerts** (the summarizer's number check).
