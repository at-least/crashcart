# CrashCart — Architecture

Sentry-SDK-compatible crash tracking for self-hosters. One Go binary, one
Postgres (with TimescaleDB), nothing else required.

```
Sentry SDK ──POST /api/{id}/envelope/──▶ crashcart ──▶ Postgres + TimescaleDB
Browser    ──GET  /p/{slug}/…  (htmx) ──▶    │        (events, sessions: hypertables
Scripts    ──GET  /api/projects/… ──────▶    │         issues: stateful table
sentry-cli ──POST /api/0/…/files/dsyms/ ─▶   │         stats: continuous aggregates
                                             │         jobs: SKIP LOCKED queue)
                                             └──▶ symbolicate sidecar (dSYM only, optional)
```

## Decisions

**Compatibility scope.** Envelope ingest (`event`, `session`, `sessions`)
and the debug-file upload endpoint sentry-cli uses. Transactions, profiles,
replays and client reports are accepted and dropped. The Sentry Web API and
UI are not imitated; the viewer is our own.

**Time is a TIMESTAMPTZ column, keys are natural.** `events.occurred_at` /
`sessions.started_at` are the TimescaleDB time dimensions; a time window
is a range on them, buckets are `time_bucket(INTERVAL …)`, policies take
intervals. The primary keys are `(project_id, event_id, occurred_at)` and
`(project_id, sid, started_at)` — the SDK's own ids, so a resent envelope
lands on the same row (`ON CONFLICT DO NOTHING`) and a session's status
updates hit one row. Every per-project table references `projects` with
`ON DELETE CASCADE`: deleting a project deletes its data. Events are addressed by `event_id` everywhere (URLs,
API, jobs); list pagination is a keyset cursor `(occurred_at, event_id)`.

**Ingest is one transaction, no hot rows.** Per envelope: fold events by
fingerprint → one `issues` upsert per distinct fingerprint → sampling
decision → pipelined `INSERT … ON CONFLICT DO NOTHING` for events → sessions
→ jobs. Aggregates are *not* touched at ingest.

**Aggregates are continuous aggregates.** `event_stats_hourly`,
`issue_stats_hourly`, `release_health_daily` are TimescaleDB continuous
aggregates with real-time aggregation on. They are functions of the raw
tables: nothing to keep consistent, adding a dimension is a new view with
history, import never recomputes them. The chart queries fold them into
buckets of any width (`crashcart_bucket`), gap-fill (`crashcart_buckets`),
rank the top releases and fold the rest into "other" — all in SQL, so the
API and the viewer share one query per chart and the Go side only maps
rows. Issue breakdowns (release / OS / device / environment / `tags.<key>`)
are one scan with a `LATERAL VALUES` unpivot and a window function. Tag
filters are containment (`tags @> {k: v}`) on a GIN index.

**Enumerations are Postgres types.** `event_level`, `session_status`,
`issue_status`, `symbol_kind`, `job_kind`, `alert_type`, `channel_kind` are
`CREATE TYPE … AS ENUM`; sqlc generates the Go constants, so the allowed
values have one definition and a bad value is rejected by the database.

**Issues are the one stateful table.** Status lifecycle (unresolved → triaged
→ resolved → regression / ignored), first/last release, exact `event_count`
(counts sampled-out events) and `stored_count`. Regression = a resolved
issue seen on a release different from `resolved_release`.

**Sampling is per issue, counts stay exact.** `projects.sample_keep_first`
events of each issue are always stored; after that `sample_rate` of them;
`fatal` always. Dropped events still increment `event_count`.

**Symbolicate at ingest, store beside the payload.** `payload` is never
rewritten (it is TOASTed; rewriting it doubles the write). Symbolicated
frames go in `events.symbols`; `fingerprint` and `error_location` are
computed on the resolved frames, so the issue is right from the first
event. ProGuard and source maps resolve in-process (mappings are loaded
from the database on first use and cached); dSYM goes through the sidecar
with a per-envelope time budget (`ingest.SymbolicateBudget`) — only when
the sidecar fails or runs out of time is a `symbolicate` job queued. An
event with no mapping yet is stored as-is, without a job: uploading a
symbol file re-queues the release's unsymbolicated events from the last
`COMPRESS_AFTER` (`resymbolicate` fans out to one `symbolicate` job per
event in a single `INSERT … SELECT`), which may move them to a new issue.
`symbol_files.release` is NULL for a mapping matched by debug id only
(`UNIQUE NULLS NOT DISTINCT` keeps one row per project/kind/release/filename).

**Compression + retention are policies.** Chunks older than `COMPRESS_AFTER`
(48 h) are compressed (`segmentby project_id, fingerprint`; typically
10–20× on Sentry payloads). Retention drops whole chunks after
`RETENTION_DAYS`; hourly/daily aggregates keep 400 days. Policies are
reconciled from the environment at startup (`internal/retention`).

**Jobs live in Postgres.** `jobs` + `UPDATE … SKIP LOCKED RETURNING`: a
worker leases a batch (`locked_until`, attempt counted) in one short
transaction, runs the handlers with nothing held open (they make HTTP
calls), then deletes or reschedules each job. An expired lease — the worker
died — makes the job claimable again. Kinds: `symbolicate {event}`,
`resymbolicate {release}`, `alert {type, fingerprint}`. Retries with
backoff; after 8 attempts a job is dead: never claimed again, kept with its
`last_error` for a week (`DeadJobs`), then dropped. A partial unique index
(`jobs_pending`) keeps one pending job per `(kind, project, args)`, so
enqueues are `ON CONFLICT DO NOTHING`. Workers wake on
`NOTIFY crashcart_jobs` (a trigger on insert, fired at commit; one LISTEN
connection per process — `store.Listener`) and poll every 30 s as the
fallback; the SSE "new issues" stream wakes the same way on
`crashcart_issues` (new issue / regression, payload = project id).

**Scheduled work runs on one replica.** The spike check and the retention
sweep tick in every process, but each tick
takes a Postgres advisory lock (`store.RunAsLeader`) and skips when another
replica holds it.

**Rate limiting is in memory, the daily quota is in Postgres.** Rate
limits are fixed 60 s windows per credential, per process; with several
replicas each enforces `RATE_LIMIT` on its own share. The daily quota is
exact: the ingest transaction bumps `project_usage (project_id, day)` and
rolls back when the total crosses `projects.daily_quota`.

**Viewer is server-rendered.** templ + htmx; all state in the URL. Issue-
centric: overview → issues → issue (stack, breakdown, events) → event;
releases with crash-free rate; settings (alerts, symbols, DSN). Charts are
inline SVG rendered by the server; `app.js` only adds keyboard triage,
theme, and the SSE "new issues" banner.

## Storage

| table | kind | key | notes |
|---|---|---|---|
| projects | table | id (identity), slug, public_key | DSN key = `public_key` |
| events | hypertable (1 day chunks) | (project_id, event_id, occurred_at) | compressed after 48 h |
| sessions | hypertable | (project_id, sid, started_at) | release-health inputs, `count` for aggregates |
| issues | table | (project_id, fingerprint) | stateful |
| symbol_files | table | (project_id, kind, release, filename) | BYTEA in Postgres |
| project_usage | table | (project_id, day) | events received per UTC day; daily quota |
| jobs | table | id | queue |
| alert_rules / alert_channels | table | | per project |
| event_stats_hourly | cagg | bucket, project, release, platform, level | events / crashes / errors |
| issue_stats_hourly | cagg | bucket, project, fingerprint | sparklines |
| release_health_daily | cagg | bucket, project, release | total / crashed / errored sessions |

## Write cost per event

Non-exception event: 1 insert. Exception event: 1 insert + 1 issue upsert
(+1 `stored_count` bump) + 0–1 job row; symbolication via the worker adds one
UPDATE of the small columns (the TOASTed payload is not rewritten). Aggregate
refresh is per bucket, compression per chunk.

## Export / import

Spec: `docs/reference/export-format.md` (change it before the code).

`crashcart export` streams NDJSON: `{"t":"<table>", ...columns}`. Rows refer
to projects by `project` slug (never by id), timestamps are RFC3339 UTC,
events / sessions carry their natural keys, JSON columns are embedded,
bytes are base64. `crashcart import` upserts (events/sessions
`ON CONFLICT DO NOTHING`, everything else on its key) inside one
transaction, so importing twice or onto live data is safe and a failed
import changes nothing. Aggregates are not exported; they recompute.

## Why TimescaleDB is required

The schema depends on the Community (TSL) half of TimescaleDB: compression
(`segmentby project_id, fingerprint`), chunk-drop retention and continuous
aggregates with real-time aggregation. The migrator (`internal/db`) creates
the extension and checks `timescaledb.license = 'timescale'`; a database
with the Apache-2 build (what most managed hosts ship) or without the
extension is refused at startup with `ErrNoTimescale`. There is one schema
(`0001_init.sql`), one stats path and one test run.
