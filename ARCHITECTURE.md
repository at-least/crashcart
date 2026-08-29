# CrashCart — Architecture

Sentry-SDK-compatible crash tracking for self-hosters. One Go binary, any
Postgres, one S3-compatible bucket; nothing else required.

```
Sentry SDK ──POST /api/{id}/envelope/──▶ crashcart ──▶ Postgres 14+ (no extensions)
Browser    ──GET  /p/{slug}/…  (htmx) ──▶    │        (events, sessions: weekly partitions
Scripts    ──GET  /api/projects/… ──────▶    │         issues: stateful table
sentry-cli ──POST /api/0/…/files/dsyms/ ─▶   │         stats: rollup tables + dirty keys
                                             │         jobs: SKIP LOCKED queue)
                                             ├──▶ S3 bucket (payload packs, symbol files, upload chunks)
                                             └──▶ symbolicate sidecar (dSYM only, optional)
```

## Decisions

**Compatibility scope.** Envelope ingest (`event`, `session`, `sessions`)
and the debug-file upload endpoint sentry-cli uses. Transactions, profiles,
replays and client reports are accepted and dropped. The Sentry Web API and
UI are not imitated; the viewer is our own.

**Time is a TIMESTAMPTZ column, keys are natural.** `events.occurred_at` /
`sessions.started_at` are the partition keys (`PARTITION BY RANGE`, one
partition per week, plus a DEFAULT partition so an insert never fails for
want of one — a device with a wrong clock lands there and is moved into a
real partition when it is created); a time window is a range on them and
prunes to its partitions, buckets are `crashcart_bucket(…)`. The primary
keys are `(project_id, event_id, occurred_at)` and
`(project_id, sid, started_at)` — the SDK's own ids, so a resent envelope
lands on the same row (`ON CONFLICT DO NOTHING`) and a session's status
updates hit one row. `event_id` and `fingerprint` are `UUID` columns (16
bytes in every key and index); in Go they are
`sentry.ID`, a 32-hex string that encodes/decodes itself as a Postgres
UUID, so URLs, JSON and the SDK protocol never see dashes. An SDK event
without a proper id gets one derived from the event body (sha256), so a
resend still dedupes. Every per-project table references `projects` with
`ON DELETE CASCADE`: deleting a project deletes its data. Events are addressed by `event_id` everywhere (URLs,
API, jobs); list pagination is a keyset cursor `(occurred_at, event_id)`.

**Ingest is one transaction, then the payloads.** Per envelope: quota bump →
`releases` upsert (a no-op unless a new release / platform appears) → fold
events by fingerprint → one `issues` upsert per distinct fingerprint →
sampling decision → pipelined `INSERT … ON CONFLICT DO NOTHING` for events →
payloads → events → sessions → dirty-hour marks → jobs. The payloads
(gzipped once, here) go into `payload_spool` in the same transaction —
as durable as the event row, nothing is ever lost — at the place they
will have in a *pack* object: the transaction claims the fullest open
pack no other transaction is writing to (`packs`, `FOR UPDATE SKIP
LOCKED`; none free → it opens one), advances the pack's byte counter by
the envelope's total — the offsets — and closes the pack when that
reaches 8 MB. So `events.payload_ref` (`<pack>#<offset>#<length>`) is
written with the row, once; a rollback returns the bytes, so packs have
no gaps; concurrent envelopes take different packs and never wait on
each other; no pack belongs to a process, so a process dying leaves
nothing to recover. Every process looks for closed packs every 5 s
(`retention.PackPayloads`): lock the pack row (`SKIP LOCKED`), lay the
spool rows out by offset, PUT the object, delete the rows and the pack,
commit — a failed PUT rolls back for the next run. Nothing but size
closes a pack: a quiet server keeps its payloads in the spool until 8 MB
have gathered (the sweep drops the ones whose events retention already
took), so the object store sees one PUT per 8 MB whatever the events'
size, and no upload is bigger — PUT requests are what an S3 bill is made
of. A failed PUT rolls the batch
back for the next run; reads come from the spool until then, so the
object store being down is invisible except for a growing spool. No
aggregate row is written at ingest.

**Statistics are rollups with dirty keys.** `event_stats_hourly_rolled`,
`issue_stats_hourly_rolled` and `release_health_hourly_rolled` hold one
row per project, hour and dimension. Every write to `events` / `sessions`
marks its `(project, hour)` in `event_stats_dirty` / `session_stats_dirty`
in the same transaction (one small upsert per hour touched; the hot row is
per project and hour, and ingest already serializes per project on the
quota row). The views the queries read — `event_stats_hourly`,
`issue_stats_hourly`, `release_health_hourly` — take the rollup for clean
hours and aggregate dirty hours live from the raw table, so they are exact
at every instant: an event that arrives days after it occurred (a crash
sent on the next app launch — the normal case for mobile) counts in its
own hour the moment it is committed, and so does a session whose status
changes or an event whose fingerprint moves after symbolication. Every
minute, on one replica, `retention.Rollup` reads up to 500 dirty keys with
their `gen`, recomputes those hours from the raw rows in one transaction,
and deletes each key only if its `gen` is unchanged — a mark that landed
meanwhile keeps it; no ingest transaction ever waits on the rollup. The
current hour stays dirty by construction and is always computed live.
The rollups keep 400 days, longer than the raw rows, and import / seed
just mark what they wrote. The chart queries read
`crashcart_event_stats(project, from, to, width)` — an inlined SQL function
over the hourly view — fold into buckets of any width (`crashcart_bucket`),
gap-fill (`crashcart_buckets`), rank the top releases and fold the rest
into "other" — all in SQL, so the API and the viewer share one query per
chart and the Go side only maps rows. Issue breakdowns (release / OS /
device / environment / `tags.<key>`) are one scan with a `LATERAL VALUES`
unpivot and a window function. Tag filters are containment
(`tags @> {k: v}`) on a GIN index.

**Enumerations are Postgres types.** `event_level`, `session_status`,
`issue_status`, `symbol_kind`, `job_kind`, `alert_type`, `channel_kind` are
`CREATE TYPE … AS ENUM`; sqlc generates the Go constants, so the allowed
values have one definition and a bad value is rejected by the database.

**Issues are the one stateful table.** Status lifecycle (unresolved → triaged
→ resolved → regression / ignored), first/last release, exact `event_count`
(counts sampled-out events) and `stored_count`. `releases` is every
release the issue was seen on (kept on the row: sampled-out and expired
events count too); resolving copies it to `resolved_releases`, and a later
event on a release outside that set is a regression — old builds still
crashing are inside the set, the release that carries the fix is not.

**Sampling is per issue, counts stay exact.** `projects.sample_keep_first`
events of each issue are always stored; after that `sample_rate` of them;
`fatal` always. Dropped events still increment `event_count`.

**Bytes live in the bucket, rows in Postgres.** The event payload — the
JSON exactly as the SDK sent it — is gzipped on its own and packed with
its neighbours into `events/<day>/<pack id>`; `events.payload_ref` is
`<key>#<offset>#<length>`, and a read is one ranged GET (`store.Payload`
tries the spool first — a primary-key lookup on a small table — which
also means "not in the spool" implies "already uploaded": no window in
which a payload is in neither place). Everything
filterable is a column or a `tags` key, extracted at ingest, so nothing
ever queries inside a payload and it is never rewritten. Symbol files are
at `symbols/<project>/<symbol_files.id>`, sentry-cli's upload chunks at
`chunks/<sha1>` until assembled — keys derived from the row. An object
that outlives its rows (a quota-rejected envelope's bytes in a pack, a
deleted symbol file) needs no garbage collection: the bucket's lifecycle
rules expire each prefix on the retention schedule. A row is a few
hundred bytes, so the database stays small whatever the payload volume;
the only readers of a payload are the event page, the JSON event
endpoint, the symbolication job and export. The S3 client is minio-go
(put, ranged get, delete, bucket lifecycle), which knows the S3-compatible
providers' quirks. Symbolicated frames go in `events.symbols`; `fingerprint` and
`error_location` are computed on the resolved frames, so the issue is
right from the first event.

**Symbolicate at ingest.** ProGuard and source maps resolve in-process (mappings are loaded
from the database on first use and cached); dSYM goes through the sidecar
with a per-envelope time budget (`ingest.SymbolicateBudget`) — only when
the sidecar fails or runs out of time is a `symbolicate` job queued. An
event with no mapping yet is stored as-is, without a job: uploading a
symbol file re-queues the release's unsymbolicated events (the newest
2000, anywhere in the retention window — `resymbolicate` fans out to one
`symbolicate` job per event in a single `INSERT … SELECT`), which may move
them to a new issue.
`symbol_files.release` is NULL for a mapping matched by debug id only
(`UNIQUE NULLS NOT DISTINCT` keeps one row per project/kind/release/filename).

**Retention is a DROP TABLE and a lifecycle rule.** `internal/retention`
keeps weekly partitions (`events_pYYYYMMDD`, Monday-aligned) from one week
before the retention window to two weeks ahead — at startup and on the
hourly sweep — and drops a partition once it ends before
`now − RETENTION_DAYS` (rows live up to a week longer; the default
partition is swept row by row, it only ever holds clock outliers). A week
that already has rows in the default partition when its partition is
created gets them moved in the same transaction (standalone table → move →
`ATTACH PARTITION`). The bucket's lifecycle rules, set at startup, expire
`events/` after `RETENTION_DAYS + 7` days, `symbols/` after
`2 × RETENTION_DAYS` (when `ExpireSymbolFiles` drops the rows), `chunks/`
after a day; nothing lists or deletes objects one by one. The rollups keep
400 days.

**Jobs live in Postgres.** `jobs` + `UPDATE … SKIP LOCKED RETURNING`: a
worker leases a batch (`locked_until`, attempt counted) in one short
transaction, runs the handlers with nothing held open (they make HTTP
calls), then deletes or reschedules each job. An expired lease — the worker
died — makes the job claimable again. Kinds: `symbolicate {event, at}` (the time keeps the lookup to one partition),
`resymbolicate {release}`, `alert {type, fingerprint}`. Retries with
backoff; after 8 attempts a job is dead: never claimed again, kept with its
`last_error` for a week (`DeadJobs`), then dropped. A partial unique index
(`jobs_pending`) keeps one pending job per `(kind, project, args)`, so
enqueues are `ON CONFLICT DO NOTHING`. Workers wake on
`NOTIFY crashcart_jobs` (a trigger on insert, fired at commit; one LISTEN
connection per process — `store.Listener`) and poll every 30 s as the
fallback; the SSE "new issues" stream wakes the same way on
`crashcart_issues` (new issue / regression, payload = project id).

**Scheduled work runs on one replica.** The stats rollup (every minute),
the spike check and the retention sweep (hourly) tick in every process,
but each tick takes a Postgres advisory lock (`store.RunAsLeader`) and
skips when another replica holds it.

**Access is users and API keys, in Postgres.** The viewer needs a signed-in
user: `users` (email, bcrypt hash) and `user_sessions` (the cookie token
stored as sha256, 30-day expiry, swept by retention). The JSON API and
sentry-cli need an API key: `api_keys` holds the secret's sha256, a display
prefix, `last_used_at` (written at most once a minute) and `revoked_at`.
The first user is created on `/setup` (only while there are none) or by
`crashcart user add`; users and keys are managed on `/account` or the
CLI. No roles. `auth.ActorFrom` names who acts; issue status changes
record it in `issues.status_by`.

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
| users / user_sessions / api_keys | table | id / token_hash / id | viewer accounts, session cookies (hashed), API keys (hashed) |
| projects | table | id (identity), slug, public_key | DSN key = `public_key` |
| events | weekly partitions + default | (project_id, event_id, occurred_at) | columns + `payload_ref` into a pack in the bucket |
| packs | table | pack_key | packs being filled (`next_offset`) or waiting for upload (`closed`) |
| payload_spool | table | (pack_key, offset) | payloads of those packs, at their offsets |
| sessions | weekly partitions + default | (project_id, sid, started_at) | release-health inputs, `count` for aggregates |
| releases | table | (project_id, release) | every release seen, platforms, first_seen |
| issues | table | (project_id, fingerprint) | stateful |
| symbol_files | table | (project_id, kind, release, filename) | metadata; the bytes are `symbols/<project>/<id>` in the bucket |
| project_usage | table | (project_id, day) | events received per UTC day; daily quota |
| jobs | table | id | queue |
| alert_rules / alert_channels | table | | per project |
| event_stats_dirty / session_stats_dirty | table | (project_id, bucket) | hours awaiting rollup, with `gen` |
| event_stats_hourly_rolled → event_stats_hourly | table → view | bucket, project, release, platform, level | events / crashes / errors; `crashcart_event_stats(…)` reads the view |
| issue_stats_hourly_rolled → issue_stats_hourly | table → view | bucket, project, fingerprint | sparklines |
| release_health_hourly_rolled → release_health_hourly | table → view | bucket, project, release | total / crashed / errored sessions |
| *(bucket)* `events/<day>/<pack>`, `symbols/`, `chunks/` | objects | pack: `payload_ref`; others derived from the row | expired by lifecycle rule |

## Write cost per event

Non-exception event: 1 insert. Exception event: 1 insert + 1 issue upsert
(+1 `stored_count` bump) + 0–1 job row; plus 1 spool insert per stored
event (deleted when its pack is uploaded), one `packs` update and one
dirty-hour upsert per hour an envelope touches; one object PUT per 8 MB
of payloads.
Symbolication via the worker adds one UPDATE of the small columns (the
payload is never rewritten) and re-marks the hour. Rollup is per dirty
hour, retention per partition.

## Export / import

Spec: `docs/reference/export-format.md` (change it before the code).

`crashcart export` streams NDJSON: `{"t":"<table>", ...columns}`. Rows refer
to projects by `project` slug (never by id), timestamps are RFC3339 UTC,
events / sessions carry their natural keys, JSON columns are embedded,
bytes are base64; payloads and symbol files are read from the bucket and
embedded (a row whose object is gone is exported without it).
`crashcart import` upserts (events/sessions `ON CONFLICT DO NOTHING`,
everything else on its key) inside one transaction and writes the objects
as it goes, so importing twice or onto live data is safe and a failed
import changes nothing in the database (objects it wrote expire by
lifecycle). Rollups are not exported; the imported hours are marked dirty
and recomputed.

## Why plain Postgres and a bucket

Postgres without extensions runs anywhere — a container, a package, RDS,
Cloud SQL, Neon, Supabase — and `pg_dump` / `pg_upgrade` stay ordinary.
What an extension would have added (compression, chunk-drop retention,
continuous aggregates) is covered by keeping the bytes out of the database
(the rows are small enough not to need compressing), weekly partitions
(retention is a `DROP TABLE`) and the dirty-key rollups (exact for late
data by construction — a policy that only refreshes a recent window is
not). The bucket is the one dependency added: it is what makes the bytes
cheap, expirable without code and shared between replicas, and every
environment from a laptop (MinIO) to any cloud has one.

**No migrations.** `internal/db/schema.sql` is the whole schema; `db.Init`
creates it on the first start against an empty database (under an advisory
lock, so replicas can start together) and does nothing afterwards. Until
there are deployed databases to carry forward, a schema change is an edit
to that file and a fresh database (`export` / `import` moves data).
