# CLAUDE.md — CrashCart (Go + Postgres/TimescaleDB)

Sentry-SDK-compatible crash tracking backend + viewer. Read ARCHITECTURE.md
for the design; GLOSSARY.md for terminology (Event / Issue / Release /
Platform — never "log entry", "error group", "app version", "OS").

## Stack

Go 1.24+ (std `net/http` mux, pgx/v5, sqlc, templ), Postgres 16 +
TimescaleDB (required), htmx + Tailwind v4 + shadless for the viewer.
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
  pk/                 µs primary key ↔ time; bucket arithmetic
  sentry/             envelope parser, Frame, Fingerprint, ErrorLocation
  db/                 migrations/*.sql (Timescale DDL), sqlc_schema.sql (plain mirror for sqlc),
                      queries/*.sql → sqlc/ (generated), migrate.go
  store/              Store = pool + sqlc.Queries; dynamic event listing/breakdown (only hand-written SQL)
  auth/               Bearer, Basic, CORS, RateLimit, SentryKey
  ingest/             POST /api/{id}/envelope|store; Ingest(); PII redaction
  symbolicate/        proguard / sourcemap (in-process), dsym (sidecar client), Service (cache + Inline + job handler)
  jobs/               worker loop (SKIP LOCKED), handlers by kind
  alerts/             notifier (webhook, telegram), crash-spike scheduler
  retention/          Timescale policy reconcile + sweeps (issues, jobs, rate_limits, symbol files)
  api/                /api/projects/… JSON handlers, /api/0/… sentry-cli compat
  web/                templ views, handlers, state.go (URL ↔ ViewState), svg charts, assets/, styles/
  export/             NDJSON export / import
  seed/               demo data
  server/             mux wiring
  testdb/             TEST_DATABASE_URL helper: fresh schema per test
container/symbolicate/  Python + llvm-symbolizer sidecar
```

## Conventions

- Regenerate after editing: `sqlc generate` (queries or `sqlc_schema.sql`),
  `templ generate` (`.templ`). Keep `internal/db/sqlc_schema.sql` in sync with
  the migrations (it is the plain-SQL mirror sqlc parses; caggs appear as tables).
- Hand-written SQL only in: migrations, `internal/store` (dynamic filters),
  `internal/export`, `internal/retention` (policy calls).
- Time windows on hypertables are id ranges: `pk.Lower(from)` / `pk.Upper(to)`.
- Nullable text columns are `*string` (sqlc `emit_pointers_for_null_types`);
  `TIMESTAMPTZ` → `time.Time`; `JSONB` → `json.RawMessage`.
- API JSON is snake_case; times RFC3339 UTC; ids are integers (< 2^53).
- Viewer URL state lives in `web/state.go` (`ViewState.With*` return copies);
  HTML mutations require the `HX-Request` header (CSRF guard); `/api/*` needs
  Bearer auth when `API_KEYS` is set; the viewer needs basic auth when
  `VIEWER_PASSWORD` is set; ingest is authenticated by the DSN key.
- Never rewrite `events.payload`. Symbolication writes `symbols`,
  `fingerprint`, `error_location`, `symbolicated` only.
- Tests: unit tests next to the code; DB-backed tests use `internal/testdb`
  and skip without `TEST_DATABASE_URL`; `internal/server/server_test.go` is
  the end-to-end suite (ingest → API → viewer).
