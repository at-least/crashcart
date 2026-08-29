# CLAUDE.md — CrashCart (Go + Postgres)

Sentry-SDK-compatible crash tracking backend + viewer. Read ARCHITECTURE.md
for the design; GLOSSARY.md for terminology (Event / Issue / Release /
Platform — never "log entry", "error group", "app version", "OS").

## Stack

Go 1.24+ (std `net/http` mux, pgx/v5, sqlc, templ), plain Postgres 14+
(no extensions; the one store — payloads, symbol files included), htmx +
Tailwind v4 + shadless for the viewer. Optional: dSYM symbolication
sidecar (`container/symbolicate`).

## Commands

```
make generate      sqlc generate + templ generate (generated files are committed)
make build         → bin/crashcart
make test          go vet + unit tests
TEST_DATABASE_URL=postgres://crashcart:crashcart@127.0.0.1:55432/crashcart?sslmode=disable make test-db
make css           rebuild internal/web/assets/app.css (needs npm install; artifact is committed)
crashcart serve | init | retention | export | import | seed | rebuild-symbols
```

Local Postgres for tests: `docker run -d --name crashcart-test-pg -e POSTGRES_PASSWORD=crashcart -e POSTGRES_USER=crashcart -e POSTGRES_DB=crashcart -p 127.0.0.1:55432:5432 postgres:16-alpine`.

## Layout

```
cmd/crashcart/        main.go: subcommands
internal/
  config/             env → Config
  sentry/             envelope parser, Frame, Fingerprint, ErrorLocation
  db/                 schema.sql (the whole schema, created on first start — no migrations), sqlc_schema.sql
                      (mirror for sqlc; the stats views appear as tables), queries/*.sql → sqlc/ (generated), db.go (Init)
  store/              Store = pool + Blobs + sqlc.Queries; dynamic event listing/breakdown (only hand-written SQL);
                      Cursor (keyset paging), Listener (LISTEN/NOTIFY fan-out)
  auth/               Access (API keys, user sessions, bcrypt), CORS, RateLimit (in-memory), SentryKey
  ingest/             POST /api/{id}/envelope|store; Ingest(); PII redaction
  symbolicate/        proguard / sourcemap (in-process), dsym (sidecar client), Service (cache + Resolve at ingest + job handlers)
  jobs/               worker loop (SKIP LOCKED), handlers by kind
  alerts/             notifier (webhook, telegram), crash-spike scheduler
  retention/          weekly partitions (ensure / drop), stats rollup (dirty keys), sweeps (issues, jobs, chunks, symbol files)
  api/                /api/projects/… JSON handlers, /api/0/… sentry-cli compat
  web/                templ views, handlers, state.go (URL ↔ ViewState), svg charts, assets/, styles/
  export/             NDJSON export / import (format: docs/reference/export-format.md)
  seed/               demo data
  server/             mux wiring
  testdb/             TEST_DATABASE_URL helper: fresh schema per test
container/symbolicate/  Python + llvm-symbolizer sidecar
```

## Conventions

- Regenerate after editing: `sqlc generate` (queries or `sqlc_schema.sql`),
  `templ generate` (`.templ`). Keep `internal/db/sqlc_schema.sql` in sync with
  `schema.sql` (it is the plain-SQL mirror sqlc parses; the stats views appear as tables).
- Hand-written SQL only in: `schema.sql`, `internal/store` (dynamic filters),
  `internal/export`, `internal/retention` (partitions, rollup).
- `events.payload` is the raw event gzipped at ingest (`store.Gzip`;
  `STORAGE EXTERNAL`); read it with `store.Payload(ctx, event)` — nil when
  the row was imported without one. Symbol files and sentry-cli chunks are
  BYTEA rows. What bounds the database is per-issue sampling
  (`sample_keep_first`, `ingest.FatalKeepFactor` for crashes, `sample_rate`
  default 0.01): stored events grow with issues, not events. There is no
  second store; do not add one.
- Statistics: every write to `events` / `sessions` marks its (project, hour)
  in `event_stats_dirty` / `session_stats_dirty` in the same transaction
  (`MarkEventStatsDirty`, also after a fingerprint change); the
  `event_stats_hourly` / `issue_stats_hourly` / `release_health_hourly`
  views read the `*_rolled` tables for clean hours and compute dirty hours
  live, `retention.Rollup` (every minute, leader) recomputes and clears
  them. Never write the `*_rolled` tables from elsewhere.
- Time is `TIMESTAMPTZ` everywhere (`events.occurred_at`, `sessions.started_at`,
  `issues.first_seen/last_seen`, stats `bucket`); windows are `[from, to)`
  on those columns, buckets are UTC-aligned (`t.Truncate(width)` in Go matches
  `crashcart_bucket` / `date_trunc(…, 'UTC')`). `events` / `sessions` are
  partitioned by week on that column (`events_pYYYYMMDD`, plus `events_default`);
  a query that carries a time range touches only its partitions. Events are
  addressed by `event_id`; event lists page with `store.Cursor` (`occurred_at` + `event_id`).
- `event_id` / `fingerprint` are Postgres `UUID` ↔ `sentry.ID` (32-hex string with
  pgx UUID scanner/valuer); parse untrusted input with `sentry.ParseID`, make test
  ids with `sentry.DerivedID([]byte("name"))`.
- Nullable text columns are `*string` (sqlc `emit_pointers_for_null_types`);
  `TIMESTAMPTZ` → `time.Time`; `JSONB` → `json.RawMessage`. Level / status /
  kind columns are Postgres enum types → `sqlc.EventLevel`, `sqlc.IssueStatus`,
  `sqlc.JobKind`, … (convert with `string(v)` / `sqlc.X(s)` at the edges).
- Every per-project table has a FK to `projects` (`ON DELETE CASCADE`);
  DB tests that insert rows for a made-up project id call `testdb.Projects`.
- API JSON is snake_case; times RFC3339 UTC; identity ids (projects, symbol
  files, channels) are integers, events are keyed by `event_id`.
- Viewer URL state lives in `web/state.go` (`ViewState.With*` return copies);
  HTML mutations require the `HX-Request` header (CSRF guard); `/api/*` needs an
  API key (`api_keys`, hashed; `auth.Access.APIKey`; CORS on it only with
  `API_CORS_ORIGIN` — `CORS_ORIGIN` is for the SDK ingest endpoints); the viewer
  needs a signed-in user (`users` + `user_sessions` cookie; `auth.Access.Session`;
  `/login`, `/setup`, `/account`); ingest is authenticated by the DSN key.
  `auth.ActorFrom(ctx)` is who acts (recorded in `issues.status_by`).
- Never rewrite `events.payload`. Symbolication writes `symbols`,
  `fingerprint`, `error_location`, `symbolicated` only.
- Tests: unit tests next to the code; DB-backed tests use `internal/testdb`
  (fresh schema per test) and skip without `TEST_DATABASE_URL`;
  `internal/server/server_test.go` is the end-to-end suite (ingest → API → viewer).
