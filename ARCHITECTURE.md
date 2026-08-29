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

**Time is in the primary key.** `events.id = unix_ms × 1000 + rand(0..999)`
(`internal/pk`). It is the TimescaleDB time dimension, so a time window is
an id range, ordering by id is chronological, and chunk exclusion works on
the PK alone. Never compare `to_timestamp(id/1e6)` in a WHERE clause.

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

**Symbolicate once, store beside the payload.** `payload` is never rewritten
(it is TOASTed; rewriting it doubles the write). Symbolicated frames go in
`events.symbols`; the fingerprint and `error_location` are updated. ProGuard
and source maps resolve in-process and inline at ingest when the mapping is
cached; dSYM goes through the sidecar via the job worker. Uploading a symbol
file re-queues the release's unsymbolicated events from the last
`COMPRESS_AFTER` (updates must land before the chunk is compressed).

**Compression + retention are policies.** Chunks older than `COMPRESS_AFTER`
(48 h) are compressed (`segmentby project_id, fingerprint`; typically
10–20× on Sentry payloads). Retention drops whole chunks after
`RETENTION_DAYS`; hourly/daily aggregates keep 400 days. Policies are
reconciled from the environment at startup (`internal/retention`).

**Jobs live in Postgres.** `jobs` + `SELECT … FOR UPDATE SKIP LOCKED`. Kinds:
`symbolicate {event}`, `resymbolicate {release}`, `alert {type, fingerprint}`.
Retries with backoff, dropped after 8 attempts.

**Viewer is server-rendered.** templ + htmx; all state in the URL. Issue-
centric: overview → issues → issue (stack, breakdown, events) → event;
releases with crash-free rate; settings (alerts, symbols, DSN). Charts are
inline SVG rendered by the server; `app.js` only adds keyboard triage,
theme, and the SSE "new issues" banner.

## Storage

| table | kind | key | notes |
|---|---|---|---|
| projects | table | id (identity), slug, public_key | DSN key = `public_key` |
| events | hypertable (1 day chunks) | id (µs) | compressed after 48 h |
| sessions | hypertable | id (µs) | release-health inputs, `count` for aggregates |
| issues | table | (project_id, fingerprint) | stateful |
| symbol_files | table | (project_id, kind, release, filename) | BYTEA in Postgres |
| jobs | table | id | queue |
| alert_rules / alert_channels | table | | per project |
| rate_limits | table | (key, window) | 60 s fixed windows |
| event_stats_hourly | cagg | bucket, project, release, platform, level | events / crashes / errors |
| issue_stats_hourly | cagg | bucket, project, fingerprint | sparklines |
| release_health_daily | cagg | bucket, project, release | total / crashed / errored sessions |

## Write cost per event

Non-exception event: 1 insert. Exception event: 1 insert + 1 issue upsert
(+1 `stored_count` bump) + 0–1 job row; symbolication via the worker adds one
UPDATE of the small columns (the TOASTed payload is not rewritten). Aggregate
refresh is per bucket, compression per chunk.

## Export / import

Spec: `docs/export-format.md` (shared contract; change it before the code).

`crashcart export` streams NDJSON: `{"t":"<table>", ...columns}`. Rows refer
to projects by `project` slug (never by id), timestamps are unix ms, ids are
integers, JSON columns are embedded, bytes are base64. `crashcart import`
upserts (events/sessions `ON CONFLICT DO NOTHING`, everything else on its key),
so importing twice or onto live data is safe. Aggregates are not exported;
they recompute.

## To do: plain-Postgres mode (Neon / Supabase)

Managed Postgres cannot run the Community (TSL) half of TimescaleDB: Neon ships
the Apache-2 edition only (hypertables, but no continuous aggregates,
compression or retention policies); Supabase has removed the extension on
PG17. Supporting them means a mode without Timescale, chosen by the migrator
when the extension is unavailable:

- `events` / `sessions` as plain tables (pg_partman optional later); the
  `(project_id, id)` indexes carry the time-range queries.
- `event_stats_hourly` / `issue_stats_hourly` / `release_health_daily` as
  plain tables, refreshed by the scheduler once per hour for the previous
  bucket (idempotent: delete bucket + insert … select); queries union the
  current hour live from the raw tables. Same design as the serverless
  implementation's rollup cron — port its bucket semantics verbatim so both
  implementations' stats tables stay column-identical.
- Retention: batched `DELETE … WHERE id < cutoff` instead of drop_chunks.
- No compression: budget 5–10× the storage.

Do this after the serverless rollup lands (its spec is the reference).
Deployment recipe to document alongside: Cloud Run / Fly (scale to zero) +
Neon; Timescale Cloud stays the full-featured managed option.
