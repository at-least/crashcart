# CLAUDE.md — CrashCart (Go + Postgres/TimescaleDB)

Sentry-SDK-compatible crash tracking backend + viewer. Read ARCHITECTURE.md
for the design; GLOSSARY.md for terminology (Event / Issue / Release /
Platform — never "log entry", "error group", "app version", "OS").

## Stack

Go 1.24+ (std `net/http` mux, pgx/v5, sqlc, templ), Postgres 16 +
TimescaleDB (optional — used when available, plain Postgres otherwise), htmx + Tailwind v4 + shadless for the viewer.
Optional: dSYM symbolication sidecar (`container/symbolicate`).

## Commands

```
make generate      sqlc generate + templ generate (generated files are committed)
make build         → bin/crashcart
make test          go vet + unit tests
TEST_DATABASE_URL=postgres://crashcart:crashcart@127.0.0.1:55432/crashcart?sslmode=disable make test-db
make css           rebuild internal/web/assets/app.css (needs npm install; artifact is committed)
crashcart serve | migrate | retention | export | import | seed | rebuild-symbols
```

Local TimescaleDB for tests: `docker run -d --name crashcart-test-pg -e POSTGRES_PASSWORD=crashcart -e POSTGRES_USER=crashcart -e POSTGRES_DB=crashcart -p 127.0.0.1:55432:5432 timescale/timescaledb:latest-pg16`.

## Layout

```
cmd/crashcart/        main.go: subcommands
internal/
  config/             env → Config
  sentry/             envelope parser, Frame, Fingerprint, ErrorLocation
  db/                 migrations/*.sql (0001 common; 0002_timescale / 0002_plain variants), sqlc_schema.sql (mirror for sqlc),
                      queries/*.sql → sqlc/ (generated), migrate.go
  store/              Store = pool + sqlc.Queries; dynamic event listing/breakdown (only hand-written SQL);
                      Cursor (keyset paging), Listener (LISTEN/NOTIFY fan-out)
  auth/               Bearer, Basic, CORS, RateLimit (in-memory), SentryKey
  ingest/             POST /api/{id}/envelope|store; Ingest(); PII redaction
  symbolicate/        proguard / sourcemap (in-process), dsym (sidecar client), Service (cache + Resolve at ingest + job handlers)
  jobs/               worker loop (SKIP LOCKED), handlers by kind
  alerts/             notifier (webhook, telegram), crash-spike scheduler
  retention/          Timescale policy reconcile + sweeps (issues, jobs, upload chunks, symbol files); plain-Postgres rollup
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
  `templ generate` (`.templ`). `TEST_PLAIN=1 make test-db` runs the suite on plain Postgres (no
  TimescaleDB; `TIMESCALE=off`). Keep `internal/db/sqlc_schema.sql` in sync with
  the migrations (it is the plain-SQL mirror sqlc parses; caggs appear as tables).
- Hand-written SQL only in: migrations, `internal/store` (dynamic filters),
  `internal/export`, `internal/retention` (policy calls).
- Time is `TIMESTAMPTZ` everywhere (`events.occurred_at`, `sessions.started_at`,
  `issues.first_seen/last_seen`, aggregate `bucket`); windows are `[from, to)`
  on those columns, buckets are UTC-aligned (`t.Truncate(width)` in Go matches
  `time_bucket` / `date_trunc(…, 'UTC')`). Events are addressed by `event_id`;
  event lists page with `store.Cursor` (`occurred_at` + `event_id`).
- Nullable text columns are `*string` (sqlc `emit_pointers_for_null_types`);
  `TIMESTAMPTZ` → `time.Time`; `JSONB` → `json.RawMessage`. Level / status /
  kind columns are Postgres enum types → `sqlc.EventLevel`, `sqlc.IssueStatus`,
  `sqlc.JobKind`, … (convert with `string(v)` / `sqlc.X(s)` at the edges).
- Every per-project table has a FK to `projects` (`ON DELETE CASCADE`);
  DB tests that insert rows for a made-up project id call `testdb.Projects`.
- API JSON is snake_case; times RFC3339 UTC; identity ids (projects, symbol
  files, channels) are integers, events are keyed by `event_id`.
- Viewer URL state lives in `web/state.go` (`ViewState.With*` return copies);
  HTML mutations require the `HX-Request` header (CSRF guard); `/api/*` needs
  Bearer auth when `API_KEYS` is set (CORS on it only with `API_CORS_ORIGIN`;
  `CORS_ORIGIN` is for the SDK ingest endpoints); the viewer needs basic auth when
  `VIEWER_PASSWORD` is set; ingest is authenticated by the DSN key.
- Never rewrite `events.payload`. Symbolication writes `symbols`,
  `fingerprint`, `error_location`, `symbolicated` only.
- Tests: unit tests next to the code; DB-backed tests use `internal/testdb`
  and skip without `TEST_DATABASE_URL`; `internal/server/server_test.go` is
  the end-to-end suite (ingest → API → viewer).
