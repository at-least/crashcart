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
history, import never recomputes them.

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
backoff, dropped after 8 attempts. Workers wake on
`NOTIFY crashcart_jobs` (a trigger on insert, fired at commit; one LISTEN
connection per process — `store.Listener`) and poll every 30 s as the
fallback; the SSE "new issues" stream wakes the same way on
`crashcart_issues` (new issue / regression, payload = project id).

**Rate limiting is in memory.** Fixed 60 s windows per credential, per
process; with several replicas each enforces `RATE_LIMIT` on its own
share.

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
`ON CONFLICT DO NOTHING`, everything else on its key), so importing twice or
onto live data is safe. Aggregates are not exported; they recompute.

## Plain-Postgres mode (Neon, Supabase, RDS, …)

Managed Postgres cannot run the Community (TSL) half of TimescaleDB (Neon ships
the Apache-2 edition only; Supabase removed the extension on PG17), so the
schema has two variants chosen by the migrator (`internal/db`): `0001_init.sql`
is common, then exactly one of `0002_timescale.sql` (hypertables, compression,
continuous aggregates) or `0002_plain.sql`. `TIMESCALE=auto` (default) probes
`CREATE EXTENSION`; `on` / `off` force a variant. A database keeps the variant
it was created with.

On plain Postgres the stats live in `*_rolled` tables and the names the
queries use (`event_stats_hourly`, `issue_stats_hourly`,
`release_health_daily`) are views: the rolled rows `UNION ALL` everything at
or after the rollup watermark (`stats_rollup.watermark`, the end of the last
rolled hour) computed live from `events` / `sessions` — so every query, sqlc
model and the API work unchanged, and nothing goes missing between an hour
rolling over and the next rollup. `retention.RollupRecent` (scheduler, every
10 minutes) re-rolls the last 3 complete hours and advances the watermark;
`RollupAll` rebuilds from the oldest row after `import` / `seed`; the sweep
deletes `events` / `sessions` by time range in 5000-row batches.
Trade-offs: no compression (budget 5–10× the storage), no chunk exclusion
(the `(project_id, id)` indexes carry the range scans).

`TEST_PLAIN=1` runs the whole test suite on the plain variant.
