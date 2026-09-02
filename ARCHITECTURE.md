# CrashCart — Architecture

Sentry-SDK-compatible crash tracking for self-hosters. One Go binary, any
Postgres; nothing else required.

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
(`internal/db/schema.sql`), the queries and the packages — and by the
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
single transaction so a failure (including the daily quota) leaves
nothing behind; no aggregate row is written at ingest. The one row every
envelope of a project touches — the quota counter — is bumped last, so
its lock is held only from that statement to the commit. Order and
details: `ingest.Ingest` (`internal/ingest/ingest.go`).

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
`crashcart_*` functions in `schema.sql`; `retention.Rollup`;
`internal/store` for the dynamic breakdowns.

**Enumerations are Postgres types.** Levels, statuses and kinds are
`CREATE TYPE … AS ENUM` in `schema.sql`; sqlc generates the Go constants,
so the allowed values have one definition and the database rejects a bad
one.

**Issues are the one stateful table.** Sentry's statuses (unresolved →
resolved → regression / ignored), no substatuses and no "triaged"; exact
counts even for events that were not stored; the releases an issue was
seen on live on the row so that "resolve in next release" regression can
be decided without the events. Only ingest may flip an issue to
regression — symbolication moving an old event between issues is not new
evidence. An ignored issue carries its own lifting condition (Sentry's
"Archive until …") and a scheduler lifts it (`alerts.CheckIgnored`). An
issue outlives its events and is expired by `retention` only once nothing
could re-create it. Definitions: `issues` in `schema.sql`,
`UpsertIssue` (`internal/db/queries/issues.sql`), `retention.ExpireIssues`.

**Sampling is per issue, counts stay exact — and it is what bounds the
database.** The first N events of each issue are always stored (more for
unhandled ones), then a project's `sample_rate` of them; dropped events
still count. The default stores everything; a project that outgrows its
machine lowers the rate, and what is stored then grows with the number of
*issues*, not events — the ten-thousandth copy of the same
NullPointerException adds nothing the issue row does not already say.
That is why payloads can simply live in Postgres and one setting lets a
single machine cover a project of any volume. Knobs and the decision:
`projects.sample_keep_first` / `sample_rate`, `ingest.UnhandledKeepFactor`,
`ingest.Ingest`.

**The payload lives with the row.** `events.payload` is the event JSON as
the SDK sent it, gzipped once at ingest and never rewritten; everything
filterable is a column or a `tags` key extracted at ingest, so nothing
queries inside it. Symbol files, sentry-cli upload chunks and envelope
attachments (screenshots …) are `BYTEA` rows too — attachments keyed by
their event and partitioned with it, kept only when the event is stored,
bounded at ingest (`sentry.MaxAttachments`, `MaxAttachmentSize`).
Everything is in the one database: one backup, one retention mechanism,
nothing to keep consistent with anything else. An object store for
payloads was considered and rejected: sampling already bounds the volume,
and a second store buys only a second thing to run, back up and keep
consistent.

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

**Rate limiting is in memory, the daily quota is in Postgres.** A rate
limit is a per-process, per-credential window (each replica enforces it
on its own share — cheap, and good enough against a burst). The daily
quota is exact because it is the ingest transaction's last statement and
rolls it back; the process then refuses that project before doing any
work until the next UTC day, and the SDKs are told to back off through
the standard header. `auth.RateLimit`, `ingest.Ingest` / `quotaExhausted`.

**Viewer is server-rendered.** templ + htmx, all state in the URL
(`web.ViewState`), charts as inline SVG from the server; `app.js` only
adds keyboard triage, theme and the SSE banner. Issue-centric: overview →
issues → issue → event; releases with crash-free rate; settings.

**No migrations, but a version.** `schema.sql` is the whole schema,
created on the first start against an empty database (under an advisory
lock so replicas can start together) and carrying its version; a later
start refuses a database of another version at startup, with
instructions, not at the first query. A schema change is an edit to that
file plus a version bump; a database moves between versions with
`export` / `import` (`internal/export`, format spec in
`docs/reference/export-format.md`).

**Why plain Postgres, and only Postgres.** Postgres without extensions
runs anywhere — a container, a package, RDS, Cloud SQL, Neon, Supabase —
and `pg_dump` / `pg_upgrade` stay ordinary. What an extension would have
added (compression, chunk-drop retention, continuous aggregates) is
covered by gzipped payloads, weekly partitions and the dirty-key rollups
(which are exact for late data by construction — a policy that only
refreshes a recent window is not).

## Plans (not implemented)

None recorded. A plan goes here until it is built; once built, its
definition is the code and the entry is deleted.
