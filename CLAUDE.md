# CLAUDE.md — CrashCart (Go + Postgres)

Sentry-SDK-compatible crash tracking backend + viewer. ARCHITECTURE.md
holds the decisions and why; GLOSSARY.md the vocabulary — Sentry's own
(Event / Issue / Release / Transaction / Culprit / Unhandled; "crash" only
for sessions) — never "log entry", "error group", "app version", "screen",
"location".

## Code is the source of truth

Markdown never defines implementation. The schema, the queries, the Go
packages and their tests are the only definition of what the system
does; an `.md` file holds decisions and their rationale, and plans that
are *not yet* implemented. When a plan is built, its `.md` definition is
deleted (DRY); a doc that restates code will drift, and drift is a bug.
User-facing reference pages (`docs/reference/*`) are derived from the
code, not hand-copied. When asked to change behaviour: change code and
tests, do not "update the docs" with a second copy.

## Stack

Go 1.25+ (std `net/http` mux, pgx/v5, sqlc, templ), plain Postgres 15+
(no extensions; the one store — payloads, symbol files included), htmx +
Tailwind v4 + shadless for the viewer. Optional: dSYM symbolication
sidecar (`container/symbolicate`).

## Commands

```
make generate      sqlc generate + templ generate + gendocs (generated files are committed)
make build         → bin/crashcart
make test          go vet + gendocs -check + unit tests
make test-db       DB-backed tests too; provisions a disposable Postgres via Docker
                    (cmd/testpg) when TEST_DATABASE_URL is unset
make css           rebuild internal/web/assets/app.css (needs npm install; artifact is committed)
crashcart          subcommands: the `usage` text in cmd/crashcart/main.go
```

## Layout

```
cmd/crashcart/        main.go: subcommands, the `serve` wiring (schedulers, shutdown order)
internal/
  config/             env → Config
  sentry/             envelope parser, Frame, Fingerprint, Culprit, ID
  db/                 schema.sql (the whole schema, created on first start — no migrations; carries a version Init checks), sqlc_schema.sql
                      (mirror for sqlc; the stats views appear as tables), queries/*.sql → sqlc/ (generated), db.go (Init)
  store/              Store = pool + sqlc.Queries; dynamic event listing/breakdown (only hand-written SQL);
                      Cursor (keyset paging), Listener (LISTEN/NOTIFY fan-out), RunAsLeader
  auth/               Access (API keys, user sessions), CORS, RateLimit, SentryKey
  ingest/             POST /api/{id}/envelope|store; Ingest(); PII redaction
  symbolicate/        proguard / sourcemap (in-process), dsym (sidecar client), sidecar (the dSYM server),
                      Service (cache + Resolve at ingest + job handlers)
  jobs/               worker loop (SKIP LOCKED), handlers by kind
  alerts/             notifier (webhook, telegram), spike check, ignored-issue check
  retention/          partitions (ensure / drop), stats rollup, sweeps
  api/                /api/projects/… JSON handlers, /api/0/… sentry-cli compat
  web/                templ views, handlers, state.go (URL ↔ ViewState), svg charts, assets/, styles/
  export/             NDJSON export / import
  seed/               demo data
  server/             mux wiring
  testdb/             a real, separate database per test (pgtestdb template clone);
                      skip without TEST_DATABASE_URL
container/symbolicate/  Dockerfile: the same binary (`crashcart symbolicate`) + llvm-symbolizer
```

## Conventions (rules for changes — not a description of the code)

- Regenerate after editing: `sqlc generate` (queries or `sqlc_schema.sql`),
  `templ generate` (`.templ`), `go run ./cmd/gendocs` (`config.Vars`,
  `cli.Commands` — rewrites `docs/deploy/configuration.md` /
  `docs/reference/cli.md`; also checks `docs/reference/api.md`,
  `docs/reference/export-format.md` and `GLOSSARY.md` against the code and
  fails if they drifted). Keep `internal/db/sqlc_schema.sql` in sync with
  `schema.sql` (it is the plain-SQL mirror sqlc parses; the stats views appear as tables).
- Hand-written SQL only in: `schema.sql`, `internal/store` (dynamic filters),
  `internal/export`, `internal/retention` (partitions, rollup). Everything
  else goes through sqlc queries.
- A schema change is an edit to `schema.sql` + a bump of its version (read
  by `db.Init`); an export-format change bumps the format in
  `internal/export` and its spec `docs/reference/export-format.md` together.
- There is one store — Postgres. Do not add an object store, a cache
  server or a second queue; per-issue sampling is what bounds the database
  (ARCHITECTURE.md).
- Never rewrite `events.payload`. Symbolication writes the small columns only.
- Never write the `*_rolled` stats tables from anywhere but
  `retention.Rollup`; every write to `events` / `sessions` (and every
  fingerprint change) marks its hour dirty in the same transaction —
  use the existing `Mark*StatsDirty` queries.
- Time is `TIMESTAMPTZ`, windows are `[from, to)`, buckets UTC-aligned
  (`crashcart_bucket` in `schema.sql`; it agrees with Go `t.Truncate`
  only for widths that divide a day — do not use wider ones). Events are
  addressed by `event_id`; lists page with `store.Cursor`, never offset.
- `sentry.ID` for event ids / fingerprints; parse untrusted input with
  `sentry.ParseID`, make test ids with `sentry.DerivedID([]byte("name"))`
  (an `event_id` that is not 32-hex is replaced by an id derived from the
  event body — don't look such an event up by `DerivedID("name")`).
- sqlc conventions: nullable text → `*string`, `TIMESTAMPTZ` → `time.Time`,
  `JSONB` → `json.RawMessage`, Postgres enums → `sqlc.X` types (convert
  with `string(v)` / `sqlc.X(s)` at the edges). Every per-project table has
  a FK to `projects` `ON DELETE CASCADE` (a test enumerates them).
- API JSON is snake_case, times RFC3339 UTC, identity ids are integers,
  events are keyed by `event_id`.
- Security boundaries to preserve: HTML mutations require `HX-Request`
  (CSRF guard); the public sign-in / setup posts refuse cross-site
  requests; `/api/*` needs an API key; the viewer a signed-in user;
  ingest the DSN key. `auth.ActorFrom(ctx)` is who acts. `API_CORS_ORIGIN`
  is for `/api/*`, `CORS_ORIGIN` for the SDK ingest endpoints.
- Vocabulary: GLOSSARY.md. Issue statuses and ignore conditions are the
  `issue_status` enum and the `ignore_*` columns; the viewer's form
  values live in `web.parseStatus`, the API's fields in `internal/api`.
- Tests with every change, and tests that can *fail*: assert the specific
  behaviour (rows, values, statuses), not `err == nil`; a claim about how
  the code behaves is confirmed by a test, not by reading. Unit tests next
  to the code; DB-backed tests use `internal/testdb` (a real, separate
  database per test, cloned from a migrated template; skip without
  `TEST_DATABASE_URL`; `testdb.Projects` for made-up project ids);
  `internal/server/server_test.go` is the end-to-end suite.
- Test-DB pitfalls: prefer `pg_catalog` over `information_schema` in
  tests. The test Postgres is the `crashcart-testpg` container
  `cmd/testpg` manages (reused across runs); `docker rm -f` it to reset.
