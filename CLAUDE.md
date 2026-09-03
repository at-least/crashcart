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

Go 1.27+ (std `net/http` mux, pgx/v5 with hand-written queries, templ,
tern for migrations), plain Postgres 15+ (no extensions; the one store —
payloads, symbol files included by default), htmx + Tailwind v4 +
shadless for the viewer. Optional: symbol files and event payloads in an
S3-compatible bucket (`BLOB_STORE=s3`, minio-go), dSYM symbolication
sidecar (`container/symbolicate`).

## Commands

```
make generate      templ generate + gendocs (generated files are committed)
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
  blob/               optional object store (BLOB_STORE=s3): Store (Memory for tests, S3 via minio-go)
  config/             env → Config
  sentry/             envelope parser, Frame, Fingerprint, Culprit, ID
  db/                 migrations/*.sql (tern, applied on every start; 00001_baseline.sql is
                      the whole schema), db.go (Init)
  store/              Store = pool + Blobs; one file per domain, each a package-level
                      function per query (`func X(ctx, db DB, args...) (Row, error)`) plus
                      that domain's row structs; enums.go (the Postgres enums, as plain
                      Go string types); packs.go (payload spool → packs, Payload read path);
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

- Regenerate after editing: `templ generate` (`.templ`), `go run
  ./cmd/gendocs` (`config.Vars`, `cli.Commands` — rewrites
  `docs/deploy/configuration.md` / `docs/reference/cli.md`; also checks
  `docs/reference/api.md`, `docs/reference/export-format.md` and
  `GLOSSARY.md` against the code and fails if they drifted).
- Queries are hand-written pgx, one file per domain in `internal/store`
  (`events.go`, `issues.go`, …): a package-level function per query,
  `func X(ctx context.Context, db DB, args...) (Row, error)`, taking `DB`
  (pool or transaction) explicitly rather than a `*Store` method — a call
  site inside a `Tx` callback then reads as a visible, greppable choice
  between the pool and the transaction (`X(ctx, s.Pool, ...)` vs `X(ctx,
  tx, ...)`) instead of an implicit one via method receiver. `*Store`
  methods remain only for composite/dynamic operations that pick
  pool-vs-tx internally (`ListEvents`, `RotateProjectKey`, `PackWeek`,
  `InsertEvents`). Row structs and the enum types (`enums.go`) live in
  `internal/store` — no separate models package, since every consumer
  already needs the pool/`Tx` to do anything with them. Scan with
  `pgx.CollectRows`/`pgx.RowToStructByName[T]` for a query's own multi-row
  shape, or a hand-written `scanX(rows pgx.Rows, err error) (X, error)`
  (`pgx.CollectOneRow` under the hood — `QueryRow` has no by-name
  equivalent) when the shape is reused across queries. By-name matching
  (case/underscore-folded, no `db` tag needed when the struct's field names
  mirror the SELECT list) errors loudly on a column ↔ field mismatch
  instead of silently rebinding two same-typed columns that got reordered.
  A multi-argument INSERT/UPDATE with several same-typed params (adjacent
  `*string`/`*time.Time` fields, a placeholder reused for two columns)
  binds by name instead of position too: `@Field` placeholders with
  `pgx.StrictStructArgs(&p)` when every field of `p` is used by the query,
  `pgx.StrictNamedArgs{...}` when the values come from more than one
  source (`SetIssuesStatus`). Plain `$1, $2` stays fine for one or two
  differently-typed params (`GetIssue`'s project_id/fingerprint) — the
  point is removing the silent-swap class of bug, not banning `$N`.
  `internal/store/tx_scope_test.go` greps every
  `Tx(func(ctx, tx) {...})` callback in the repo for a `.Pool` reference
  and fails the build if it finds one — the mechanical check that
  replaces sqlc's inability to compile a query against the wrong
  connection.
- SQL only lives in: `internal/db/migrations/`, `internal/store` (every
  query, plus its dynamic filters), `internal/export`, `internal/retention`
  (partitions, rollup). A handler package (`internal/api`, `internal/web`,
  …) never writes a query string itself — it calls a `store` function.
- A schema change is a new file in `internal/db/migrations/` (never an
  edit to an existing one) — sequential digit prefix (`\d+_name.sql`),
  plain SQL with no per-statement markers (tern runs a migration's whole
  file as one multi-statement `Exec`, so a plpgsql function body with a
  `;` inside needs no special handling). Forward-only: omit the
  `---- create above / drop below ----` separator so the migration is
  irreversible; this project restores from `crashcart export` instead of
  rolling back. An export-format change bumps the format
  in `internal/export` and its spec `docs/reference/export-format.md`
  together — independent of schema versioning.
- There is one store by default — Postgres. Symbol files and event
  payloads may live in object storage (`BLOB_STORE=s3`, `internal/blob`),
  nothing else does: no cache server, no second queue, no other table in
  a bucket; per-issue sampling is what bounds the database
  (ARCHITECTURE.md). A row's location is its own — `symbol_files.data`
  xor `blob_key`; `events.payload`, else `payload_spool`, else a pack via
  `event_packs` — so read rows the way they were written
  (`store.Payload`, `symbolicate.Service`'s `files.go` helpers), never
  by the process's mode. Write objects before rows, delete them after
  rows, in `packs.go` / `files.go`; ingest never talks to the bucket
  (the spool is written in its transaction); never rely on bucket
  lifecycle rules.
- Never rewrite `events.payload`. Symbolication writes the small columns only.
- Never write the `*_rolled` stats tables from anywhere but
  `retention.Rollup`; every write to `events` / `sessions` (and every
  fingerprint change) marks its hour dirty in the same transaction —
  use the existing `Mark*StatsDirty` queries.
- Time is `TIMESTAMPTZ`, windows are `[from, to)`, buckets UTC-aligned
  (`crashcart_bucket` in `internal/db/migrations/`; it agrees with Go `t.Truncate`
  only for widths that divide a day — do not use wider ones). Events are
  addressed by `event_id`; lists page with `store.Cursor`, never offset.
- `sentry.ID` for event ids / fingerprints; parse untrusted input with
  `sentry.ParseID`, make test ids with `sentry.DerivedID([]byte("name"))`
  (an `event_id` that is not 32-hex is replaced by an id derived from the
  event body — don't look such an event up by `DerivedID("name")`).
- Row-struct conventions: nullable text → `*string`, `TIMESTAMPTZ` →
  `time.Time`, `JSONB` → `json.RawMessage`, Postgres enums → `store.X`
  types — a plain `type X string` needs no `Scan`/`Value` methods for pgx
  to decode/encode an enum column. A struct that crosses the HTTP API
  boundary keeps its exact `json:"snake_case"` tags. Every per-project
  table has a FK to `projects` `ON DELETE CASCADE` (a test enumerates
  them).
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
