# CrashCart — Architecture

Sentry-SDK-compatible crash tracking for self-hosters. One Go binary, any
Postgres; nothing else required — an S3-compatible bucket for the big
bytes (symbol files, raw payloads) is optional (`BLOB_STORE`), not assumed.

```
Sentry SDK ──POST /api/{id}/envelope/──▶ crashcart ──▶ Postgres 15+ (no extensions)
Browser    ──GET  /p/{slug}/…  (htmx) ──▶    │        (events, sessions: weekly partitions
Scripts    ──GET  /api/projects/… ──────▶    │         issues: stateful table
sentry-cli ──POST /api/0/…/files/dsyms/ ─▶   │         stats: rollup tables + dirty keys
                                             │         jobs: SKIP LOCKED queue)
                                             └──▶ symbolicate sidecar (dSYM only, optional)
```

This file records the *decisions* and why they were taken. What the
system actually does is defined by the code it points at — the schema
(`internal/db/migrations/`), the queries and the packages — and by the
tests; nothing here is a second definition of it. When a pointer and the
code disagree, the code is right and this file is wrong.

## Decisions

**Compatibility scope — the SDK wire protocol, not Sentry's web API.**
We speak what the SDKs and sentry-cli send (envelopes, debug-file
upload) so any Sentry SDK works unchanged; the Sentry Web API and UI are
not imitated — the viewer is our own, which keeps the product small.
Which envelope item types are stored and which are accepted-and-dropped:
`internal/sentry/envelope.go`.

**Time is a `TIMESTAMPTZ` column, keys are natural.** Events and sessions
are partitioned by their own time (weekly), so a time window prunes to
its partitions and retention is a `DROP TABLE`; a DEFAULT partition means
an insert never fails for want of one (a wrong device clock lands there
and is moved when its week is created). Primary keys are the SDK's own
ids plus that time, so a resent envelope lands on the same row and a
session's status updates hit one row. Ids are `UUID` in Postgres and a
dash-less hex string in Go (`sentry.ID`), so URLs, JSON and the protocol
never see dashes; an event without a proper id gets a stable one derived
from its body so a resend still dedupes (`sentry.DerivedID`). Every
per-project table cascades from `projects`: deleting a project deletes
its data. Lists page by keyset (`store.Cursor`), never by offset.

**Ingest is one transaction, no hot rows.** An envelope is written in a
single transaction so a failure leaves nothing behind; no aggregate row
is written at ingest, and no row is bumped on every envelope of a
project — the only per-project row-level contention in the write path
is `issues`, one row per fingerprint, not per envelope. The store
decision for each event is a plain probability computed before that
upsert, not derived from its result, so the row lock covers exactly one
statement per fingerprint per envelope, nothing more. Order and details:
`ingest.Ingest` (`internal/ingest/ingest.go`).

**Statistics are rollups with dirty keys, exact for late data.** Hourly
rollup tables are read through views that take the rolled row for clean
hours and compute dirty hours live from the raw rows, so a crash that
arrives days after it happened (the normal mobile case), a session whose
status changes, or an event whose fingerprint moves after symbolication
is counted correctly the moment it is committed. Every write to
events/sessions marks its (project, hour) dirty in the same transaction;
a scheduler recomputes dirty hours and clears a key only if no mark
landed meanwhile, so ingest never waits on the rollup. The chart queries
fold, gap-fill and rank in SQL so the API and the viewer share one query
per chart. Definitions: the `*_dirty` / `*_rolled` tables, views and
`crashcart_*` functions in `internal/db/migrations/`; `retention.Rollup`;
`internal/store` for the dynamic breakdowns.

**Enumerations are Postgres types.** Levels, statuses and kinds are
`CREATE TYPE … AS ENUM` in `internal/db/migrations/`, mirrored by a plain
`type X string` with its constants in `internal/store/enums.go`, so the
allowed values have one definition and the database rejects a bad one.

**Issues are the one stateful table.** Sentry's statuses (unresolved →
resolved → regression / ignored), no substatuses and no "triaged"; exact
counts even for events that were not stored; the releases an issue was
seen on live on the row so that "resolve in next release" regression can
be decided without the events. Only ingest may flip an issue to
regression — symbolication moving an old event between issues is not new
evidence. An ignored issue carries its own lifting condition (Sentry's
"Archive until …") and a scheduler lifts it (`alerts.CheckIgnored`). An
issue outlives its events and is expired by `retention` only once nothing
could re-create it. Definitions: `issues` in `internal/db/migrations/`,
`UpsertIssue` (`internal/db/queries/issues.sql`), `retention.ExpireIssues`.

**Sampling is per event, counts stay exact — and it is what bounds the
database.** Every event is an independent `sample_rate` coin flip
(unhandled ones — crashes, uncaught exceptions — at `UnhandledKeepFactor`×
that, capped at 1); dropped events still count on their issue. There is
no per-issue "first N always kept" guarantee: the goal is debugging, not
an audit trail, so a real, recurring issue surfaces through volume
instead of a stored sequence number — and removing that guarantee also
means the store decision no longer needs the issue row's post-upsert
count, so it is made before the upsert, not after (see "no hot rows"
above). The default stores everything; a project that outgrows its
machine lowers the rate, and what is stored then grows with the number of
*issues*, not events — the ten-thousandth copy of the same
NullPointerException adds nothing the issue row does not already say.
That is why payloads can simply live in Postgres and one setting lets a
single machine cover a project of any volume. Knobs and the decision:
`projects.sample_rate`, `ingest.UnhandledKeepFactor`, `ingest.sampleStore`.

**The payload lives with the row.** `events.payload` is the event JSON as
the SDK sent it, gzipped once at ingest and never rewritten; everything
filterable is a column or a `tags` key extracted at ingest, so nothing
queries inside it. sentry-cli upload chunks and envelope attachments
(screenshots …) are `BYTEA` rows too — attachments keyed by their event
and partitioned with it, kept only when the event is stored, bounded at
ingest (`sentry.MaxAttachments`, `MaxAttachmentSize`). Everything is in
the one database by default: one backup, one retention mechanism, nothing
to keep consistent with anything else. An object store for payloads was
considered and rejected: sampling already bounds the volume, and a second
store buys only a second thing to run, back up and keep consistent.

**The big bytes may go to a bucket — only when asked, and per row.**
Measured: 30,000 events with 30 KB gzipped payloads are 1,015 MB in
Postgres and 20 MB without them — payload bytes *are* the database, and
backup/restore time, WAL, replication and `crashcart export` all scale
with them; a ProGuard mapping from a large Android app runs to hundreds
of MB on its own (`symbolicate.MaxUpload` is 500 MB). Both are written
once, read rarely and never queried by content — the shape object storage
exists for, and what Sentry does with its debug files. So `BLOB_STORE=s3`
puts symbol files and event payloads in an S3-compatible bucket; the
default stays `postgres` (nothing else to run). The choice is **per
row**, never per process, so nothing is migrated when the backend
changes and a database legitimately holds both kinds: `export` inlines
the bytes whichever way they are held and `import` writes them the
destination's way — the export file is also the move between backends.

Symbol files: `symbol_files` carries either `data` or `blob_key`; the
object is written before its row and deleted after it under a per-row
advisory lock, so a failed upload leaves no row and a re-upload frees
exactly the object it replaced (`internal/symbolicate/files.go`).

Payloads: ingest never touches the bucket. With a store, `InsertEvents`
writes the gzipped payload into `payload_spool` **in the ingest
transaction** — durable in Postgres before any object exists, the bucket
being down only a growing spool — and a leader tick packs the spool per
(project, week) into ~8 MB objects (`events/<project>/<week>/<id>`;
one PUT per pack, because PUT requests, not bytes, are what an
object-store bill is made of: per-event objects at 1M events/month would
cost more in PUTs than in storage), recording each event's place in
`event_packs`. A payload is read from the column, else the spool, else a
ranged GET — the last two in one statement, so an event packed between
two lookups cannot read as one without a payload; an export walks
events in pack order through a small cache of whole packs. A week's
packs are deleted with its partition, by the `packs` table — never by
bucket lifecycle rules, which the previous object store used and which
expired objects rows still pointed at (`internal/store/packs.go`,
`internal/blob`).

**Symbolicate at ingest, fall back to a job.** ProGuard and source maps
resolve in-process (mappings cached from the database); dSYM goes through
the sidecar under a per-envelope time budget, and only when the sidecar
fails, is slow or lacks the file is a job queued — an SDK request never
waits on a large file leaving Postgres. An event with no mapping yet is
stored as-is without a job; uploading a mapping later re-queues the
release's unsymbolicated events, which may move them to another issue.
Symbolication writes only the small columns, never the payload.
`internal/symbolicate/service.go` (`SymbolicateBudget` is in `ingest`).

**The dSYM sidecar is the same binary with LLVM next to it.** `crashcart
symbolicate` is an HTTP server around `llvm-symbolizer` in its own
container (`container/symbolicate`) — the only image carrying LLVM — with
no database and a disk LRU cache keyed so that a re-upload is a new key.
A miss is answered as such and the file is sent once, so a dSYM crosses
the wire once per sidecar, not once per crash. `internal/symbolicate/sidecar.go`,
`dsym.go`.

**Retention is a `DROP TABLE`.** Weekly partitions are kept ahead of
time and dropped once wholly past the retention window (rows live up to
a week longer); the default partition, which only ever holds clock
outliers, is swept row by row. Rollups outlive raw rows; symbol files,
upload chunks, dead jobs and stale issues have their own expiries.
`internal/retention/retention.go`.

**Jobs live in Postgres.** A `SKIP LOCKED` lease queue: a worker leases a
batch in one short transaction, runs handlers with nothing held open (they
make HTTP calls), then records the outcome; an expired lease makes the
job claimable again, and the handler's deadline is the lease itself so a
job never runs past the moment another worker may claim it. One live job
per (kind, project, args) — a repeat enqueue only pulls the due time
forward. Workers wake on `NOTIFY` with a slow poll as fallback; the SSE
"new issues" stream wakes the same way. `internal/jobs/worker.go`,
`internal/db/queries/jobs.sql`, `store.Listener`.

**Scheduled work runs on one replica.** Every process ticks, but each
tick takes an advisory lock (`store.RunAsLeader`) and skips when another
replica holds it; partition creation serializes the same way so replicas
starting together do not race. Which jobs tick at which cadence:
`cmd/crashcart/main.go` (`serve`).

**Shutdown drains in order.** Stop accepting, drain HTTP (an ingest write
may finish), then stop workers and schedulers and wait for the job in
hand to record its outcome; a second signal exits immediately.
`cmd/crashcart/main.go`.

**Alerts never reach inward.** A webhook target is checked as entered
and again after DNS (so a name resolving to a metadata address is
caught); loopback, link-local, unspecified and multicast are always
refused, private ranges unless configured, and redirects are never
followed. `internal/alerts/alerts.go` (`ValidateWebhookURL`, the dialer).

**Access is users and API keys, in Postgres.** The viewer needs a
signed-in user, the JSON API and sentry-cli an API key; secrets are
stored hashed, never plain. No roles; who acts is recorded on issue
status changes. `internal/auth`.

**Rate limiting is in memory — the one ingest guard, approximate on
purpose.** A rate limit is a per-process, per-credential 60 s window
(each replica enforces it on its own share, so the effective ceiling
scales with replica count); the SDK is told to back off through the
standard header. There is no daily quota: sampling already bounds
storage, and an exact, Postgres-backed cost cap would need a row every
envelope of a project touches — the "no hot rows" cost this design
avoids — for a guarantee (a total daily budget, not just a burst limit)
most deployments do not need. `auth.RateLimit`, `ingest.Ingest`.

**Viewer is server-rendered.** templ + htmx, all state in the URL
(`web.ViewState`), charts as inline SVG from the server; `app.js` only
adds keyboard triage, theme and the SSE banner. Issue-centric: overview →
issues → issue → event; releases with crash-free rate; settings.

**Versioned migrations (tern).** `internal/db/migrations/` is applied on
every start; `db.Init` hijacks a single pool connection (`pool.Acquire` +
`Hijack`, the same pattern `store.Listener` uses for LISTEN) and hands it
to tern's `migrate.Migrator`, which runs its own `pg_advisory_lock` and
every migration statement on that one connection — serializing replicas
starting together without ever needing a second connection of its own. An
earlier hand-rolled advisory lock held a *second* connection around the
migration run and could self-deadlock a pool with no headroom to spare
(`MaxConns=1`; the same shape as the RunAsLeader fix below); pinning the
lock and the migrations to one connection is what closes that (`db.Init`,
`TestInitSingleConnection`).
`00001_baseline.sql` is the whole schema, and a schema change from here on
is a new migration file, not an edit to an existing one. Chosen over an
earlier "one schema.sql + a version, moved with `crashcart export`/
`import`" model for two costs that model had: a routine schema change
forced operators through a full dump/reload instead of pull-and-restart,
and — because the domain here is stored events/payloads — that reload's
cost scales with data volume, so a deployment's upgrade cost only grew
over its life instead of staying flat. `db.Init` still refuses to start
rather than silently doing the wrong thing: a database ahead of the
binary's known migrations refuses with instructions. `export` / `import`
(`internal/export`, format spec in `docs/reference/export-format.md`)
remain for backup and moving a database between environments — that is
decoupled from schema versioning.

**Why plain Postgres, and only Postgres (plus, optionally, a bucket).**
Postgres without extensions runs anywhere — a container, a package, RDS,
Cloud SQL, Neon, Supabase — and `pg_dump` / `pg_upgrade` stay ordinary. What an extension would have
added (compression, chunk-drop retention, continuous aggregates) is
covered by gzipped payloads, weekly partitions and the dirty-key rollups
(which are exact for late data by construction — a policy that only
refreshes a recent window is not).

## Plans (not implemented)

A plan goes here until it is built; once built, its definition is the
code and the entry is deleted.

**`crashcart blob-gc`.** The blob store accepts bounded orphan windows
rather than risking a row without its bytes: a crash between a symbol
row's commit and the post-commit delete of the object it replaced, an
`import` into a `postgres`-mode instance over rows that held a
`blob_key`, and a pack whose object was written but whose flush
transaction failed (the spool rows are repacked; the object and its
`packs` row wait for the week sweep). A `List(prefix)` on `blob.Store`
plus a command that removes objects under `symbols/` and `events/` no
row references would close them all. Not built: none has been seen in
practice, and the previous object store's lifecycle-rule shortcut is the
thing this design exists to avoid.

**`crashcart blob-migrate`.** Switching `BLOB_STORE` to `s3` leaves the
rows written before it where they are (by design). For a large existing
instance the honest way to move *them* is a background job that reads
`events.payload` rows a partition at a time, spools and packs them, and
NULLs the column — not `export`/`import` of a 300 GB database. Not built.

**`crashcart push-relay`.** The iOS/Android companion apps (planned,
`crashcart-ios` / `crashcart-android`, separate repos) are one published
App Store / Play Store binary, so every install shares the one Firebase
project baked into that binary — an instance's own
`FCM_SERVICE_ACCOUNT_JSON` (`internal/alerts/push.go`) cannot send to
those devices (FCM refuses a token from a different project), and
handing every self-hosted instance the maintainer's real service account
so it *could* would let any one of them push to any other instance's
users. The fix is a small relay subcommand the maintainer alone runs,
holding the one real `FCM_SERVICE_ACCOUNT_JSON`; `sendPush` calls it
instead of FCM directly by default. It needs no billing or entitlement
check of its own — the paid part is the App Store/Play Store
subscription, not the relay — only a rate limit (per device token or
source IP, `internal/auth.RateLimit`'s pattern) to bound abuse and FCM
quota. `FCM_SERVICE_ACCOUNT_JSON` stays as a direct-to-FCM override for
anyone who builds the app themselves against their own Firebase project.
Not built: the apps themselves don't exist yet.
