# CrashCart — Architecture

Sentry-SDK-compatible crash tracking for self-hosters. One Go binary, any
Postgres; nothing else required.

```
Sentry SDK ──POST /api/{id}/envelope/──▶ crashcart ──▶ Postgres 14+ (no extensions)
Browser    ──GET  /p/{slug}/…  (htmx) ──▶    │        (events, sessions: weekly partitions
Scripts    ──GET  /api/projects/… ──────▶    │         issues: stateful table
sentry-cli ──POST /api/0/…/files/dsyms/ ─▶   │         stats: rollup tables + dirty keys
                                             │         jobs: SKIP LOCKED queue)
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

**Ingest is one transaction, no hot rows.** Per envelope: `releases`
upsert (a no-op unless a new release / platform appears) → fold events by
fingerprint → one `issues` upsert per distinct fingerprint → sampling
decision → pipelined `INSERT … ON CONFLICT DO NOTHING` for events (payload
gzipped) → sessions → dirty-hour marks → jobs → quota bump. No aggregate
row is written at ingest. The quota row (`project_usage`, one per project
and day) is the only row every envelope of a project touches, so it is
bumped last: its lock is held from that statement to the commit, and
envelopes of one project overlap for the rest of the write.

**Statistics are rollups with dirty keys.** `event_stats_hourly_rolled`,
`issue_stats_hourly_rolled` and `release_health_hourly_rolled` hold one
row per project, hour and dimension. Every write to `events` / `sessions`
marks its `(project, hour)` in `event_stats_dirty` / `session_stats_dirty`
in the same transaction (one small upsert per hour touched; the hot row is
per project and hour, taken right before the quota row). The views the queries read — `event_stats_hourly`,
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
just mark what they wrote; a dirty hour older than `RETENTION_DAYS` is
cleared without recomputing — its raw rows are gone (or a lone event
with a clock far in the past is all there is), and the rolled row is the
record. The chart queries read
`crashcart_event_stats(project, from, to)` — an inlined SQL function
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
Only ingest may flip that status (`UpsertIssue` with `regress`);
symbolication moving an old event between issues is not new evidence.
An issue outlives its events: resolved / ignored ones are deleted once
the partition holding their last event is gone (the week boundary, so
an issue is never deleted while its events still list and re-created
as new by the next one), unresolved ones after `retention.StaleIssueFactor`
retentions; their per-issue rollup rows go with them.

**Sampling is per issue, counts stay exact — and it is what bounds the
database.** The first `projects.sample_keep_first` events of each issue
are always stored (`ingest.FatalKeepFactor` × that for crashes); after
that `sample_rate` of them; ungrouped events (nothing to fingerprint)
take the rate from the start. Dropped events still increment
`event_count`, so the numbers stay exact. The default rate is 1 — store
everything — and a project that outgrows its machine lowers it: what is
stored then grows with the number of *issues*, not the number of events,
because the ten-thousandth copy of the same NullPointerException adds
nothing the issue row does not already say. That is why payloads can
simply live in Postgres, and why one setting lets a single machine cover
a project of any event volume.

**The payload lives with the row.** `events.payload` is the event's JSON
exactly as the SDK sent it, gzipped once at ingest (`STORAGE EXTERNAL`,
so TOAST keeps it out of the main heap without compressing it again — a
row stays a few hundred bytes for every scan that does not need it);
everything filterable is a column or a `tags` key, extracted at ingest,
so nothing ever queries inside it, and it is never rewritten. The readers
are the event page, the JSON event endpoint, the symbolication job and
export. Symbol files and sentry-cli's upload chunks are `BYTEA` rows too.
Everything is in the one database: one backup, one retention mechanism,
nothing to keep consistent with anything else.

**Symbolicate at ingest.** ProGuard and source maps resolve in-process (mappings are loaded
from the database on first use and cached); dSYM goes through the sidecar
with a per-envelope time budget (`ingest.SymbolicateBudget`) — only when
the sidecar fails, runs out of time or does not have the dSYM yet is a
`symbolicate` job queued. An event with no mapping yet is stored as-is, without a job: uploading a
symbol file re-queues the release's unsymbolicated events (the newest
2000, anywhere in the retention window — `resymbolicate` fans out to one
`symbolicate` job per event in a single `INSERT … SELECT`), which may move
them to a new issue.
`symbol_files.release` is NULL for a mapping matched by debug id only
(`UNIQUE NULLS NOT DISTINCT` keeps one row per project/kind/release/filename).

**The dSYM sidecar is the same binary with LLVM next to it.** `crashcart
symbolicate` (`internal/symbolicate.Sidecar`) is an HTTP server around
`llvm-symbolizer` in its own container (`container/symbolicate`), the
only image that carries LLVM; it has no database. It keeps the dSYMs it
has seen on disk (least recently used, `SYMBOLICATE_CACHE_MAX_MB`) under
a key made of the `symbol_files` row's id and upload time, so a re-upload
is a new key. A request names the key and the addresses; a miss is a
404, and only then are the bytes read from the database and sent once
(`PUT /symbols/{key}`). At ingest a miss is not filled — the event is
stored as-is and its `symbolicate` job sends the file — so an SDK request
never waits on a few hundred MB leaving Postgres, and a dSYM crosses the
wire once per sidecar, not once per crash.

**Retention is a DROP TABLE.** `internal/retention` keeps weekly
partitions (`events_pYYYYMMDD`, Monday-aligned) from one week before the
retention window to two weeks ahead — at startup and on the hourly sweep
— and drops a partition once it ends before `now − RETENTION_DAYS` (rows
live up to a week longer; the default partition is swept row by row, it
only ever holds clock outliers). A week that already has rows in the
default partition when its partition is created gets them moved in the
same transaction (standalone table → move → `ATTACH PARTITION`). The
payloads go with their rows. Symbol files expire after twice the
retention, upload chunks after a day, the rollups keep 400 days.

**Jobs live in Postgres.** `jobs` + `UPDATE … SKIP LOCKED RETURNING`: a
worker leases a batch (`locked_until`, attempt counted) in one short
transaction, runs the handlers with nothing held open (they make HTTP
calls), then deletes or reschedules each job. An expired lease — the worker
died — makes the job claimable again. Kinds: `symbolicate {event, at}` (the time keeps the lookup to one partition),
`resymbolicate {release}`, `alert {type, fingerprint}`. Retries with
backoff; after 8 attempts a job is dead: never claimed again, kept with its
`last_error` for a week (`DeadJobs`), then dropped. A partial unique index
(`jobs_pending`) keeps one live job per `(kind, project, args)` —
pending, leased or backing off — so enqueues are `ON CONFLICT DO UPDATE`
that only pull `run_after` forward: a repeat while the job is running
cannot leave a second row for the retry to collide with, and an enqueue
during a backoff makes the job due now. A handler's deadline is the
lease itself, so a job never runs on past the moment another worker may
claim it. Workers wake on
`NOTIFY crashcart_jobs` (a trigger on insert, fired at commit; one LISTEN
connection per process — `store.Listener`) and poll every 30 s as the
fallback; the SSE "new issues" stream wakes the same way on
`crashcart_issues` (new issue / regression, payload = project id).

**Scheduled work runs on one replica.** The stats rollup (every minute),
the spike check and the retention sweep (hourly) tick in every process,
but each tick takes a Postgres advisory lock (`store.RunAsLeader`) and
skips when another replica holds it. Partition creation (at startup on
every replica, and in the sweep) serializes on a transaction-scoped
advisory lock and re-checks under it, so replicas starting together do
not race on `CREATE TABLE`.

**Shutdown drains in order.** On SIGTERM the server stops accepting and
drains the HTTP handlers (an ingest write may take `ingest.WriteTimeout`;
the SSE streams are cut at once, they reconnect); only then are the
workers and schedulers stopped, and the process waits for the job in
hand to record its outcome. A second signal exits immediately.

**Alerts never reach inward.** A webhook URL is checked as entered and
again when the connection is made (after DNS, so a name resolving to
169.254.169.254 is caught): loopback, link-local, unspecified and
multicast targets are refused always, private ranges unless
`WEBHOOK_ALLOW_PRIVATE`, and redirects are never followed.

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
replicas each enforces `RATE_LIMIT` on its own share (6000/min by
default — a burst of cached crashes after an outage must fit). The daily
quota is off by default (sampling bounds the database; a quota is a cost
cap a project sets) and exact: the ingest transaction bumps `project_usage (project_id, day)` as
its last statement and rolls back when the total crosses
`projects.daily_quota`. The process then remembers that the project is
exhausted until the next UTC day (or until its quota is changed) and
refuses its envelopes before doing any work; the SDKs get the
`X-Sentry-Rate-Limits` header and stop sending on their own.

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
| events | weekly partitions + default | (project_id, event_id, occurred_at) | columns + gzipped `payload` (TOASTed, uncompressed by TOAST) |
| sessions | weekly partitions + default | (project_id, sid, started_at) | release-health inputs, `count` for aggregates |
| releases | table | (project_id, release) | every release seen, platforms, first_seen |
| issues | table | (project_id, fingerprint) | stateful |
| symbol_files | table | (project_id, kind, release, filename) | BYTEA in Postgres |
| upload_chunks | table | sha1 | sentry-cli chunks until assembled (a day at most) |
| project_usage | table | (project_id, day) | events received per UTC day; daily quota |
| jobs | table | id | queue |
| alert_rules / alert_channels | table | | per project |
| event_stats_dirty / session_stats_dirty | table | (project_id, bucket) | hours awaiting rollup, with `gen` |
| event_stats_hourly_rolled → event_stats_hourly | table → view | bucket, project, release, platform, level | events / crashes / errors; `crashcart_event_stats(…)` reads the view |
| issue_stats_hourly_rolled → issue_stats_hourly | table → view | bucket, project, fingerprint | sparklines |
| release_health_hourly_rolled → release_health_hourly | table → view | bucket, project, release | total / crashed / errored sessions |

## Write cost per event

Non-exception event: 1 insert (row + TOASTed payload). Exception event: 1
insert + 1 issue upsert (+1 `stored_count` bump) + 0–1 job row; one
dirty-hour upsert per hour an envelope touches; a sampled-out event is
the issue upsert alone. Symbolication via the worker adds one UPDATE of
the small columns (the TOASTed payload is not rewritten) and re-marks the
hour. Rollup is per dirty hour, retention per partition.

## Export / import

Spec: `docs/reference/export-format.md` (change it before the code).

`crashcart export` streams NDJSON: `{"t":"<table>", ...columns}`. Rows refer
to projects by `project` slug (never by id), timestamps are RFC3339 UTC,
events / sessions carry their natural keys, JSON columns are embedded,
bytes (payloads decoded, symbol files) are base64. `crashcart import`
upserts (events/sessions `ON CONFLICT DO NOTHING`, issue counts never go
down, everything else on its key), committing every `export.CommitEvery`
lines — a month of events is tens of millions of rows, too many for one
transaction — so importing twice or onto live data is safe, and a failed
import keeps the chunks before the bad line and is re-run. Rollups are
not exported; the imported hours are marked dirty and recomputed.

## Why plain Postgres, and only Postgres

Postgres without extensions runs anywhere — a container, a package, RDS,
Cloud SQL, Neon, Supabase — and `pg_dump` / `pg_upgrade` stay ordinary.
What an extension would have added (compression, chunk-drop retention,
continuous aggregates) is covered by gzipping payloads at ingest, weekly
partitions (retention is a `DROP TABLE`) and the dirty-key rollups (exact
for late data by construction — a policy that only refreshes a recent
window is not). An object store was considered for the payloads and
rejected: per-issue sampling already keeps the stored volume proportional
to the number of issues, so the bytes fit in the database of any single
machine, and a second store would have bought nothing but a second thing
to run, back up and keep consistent.

**No migrations, but a version.** `internal/db/schema.sql` is the whole
schema; `db.Init` creates it on the first start against an empty database
(under an advisory lock, so replicas can start together). The schema
carries its version in `crashcart_schema` (one row, written by
`schema.sql`; `db.SchemaVersion` is read from the same statement), and on
every later start `Init` compares the two and refuses to run against a
database of another version — a newer binary on an older database fails
at startup with instructions, not at the first query. A schema change is
an edit to that file plus a bump of the version, and a database moves
between versions with `export` / `import`.
