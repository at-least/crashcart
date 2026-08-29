# CrashCart

Open-source error tracking for mobile and web apps. Works with the Sentry
SDK: point any Sentry SDK at CrashCart's DSN and it receives crashes, errors,
messages and release-health sessions. CrashCart is *Sentry SDK compatible*;
it does not imitate the Sentry product — the viewer, API and data model are
its own (see [GLOSSARY.md](GLOSSARY.md) for the vocabulary).

One Go binary, one Postgres with TimescaleDB, nothing else required.

```
Sentry SDK ──POST /api/{id}/envelope/──▶ crashcart ──▶ Postgres + TimescaleDB
Browser    ──GET  /p/{slug}/…  (htmx) ──▶    │
Scripts    ──GET  /api/projects/… ──────▶    │
sentry-cli ──POST /api/0/…/files/dsyms/ ─▶   └──▶ symbolicate sidecar (dSYM only, optional)
```

Design notes live in [ARCHITECTURE.md](ARCHITECTURE.md).

## Quick start

```sh
docker compose up -d                       # TimescaleDB + crashcart on :8080
docker compose exec crashcart /crashcart project shop "Shop app" ios
# project shop (id 1)
# DSN: http://<key>@localhost:8080/1
```

Paste the DSN into the SDK:

```swift
SentrySDK.start { $0.dsn = "http://<key>@localhost:8080/1" }
```

A project is one DSN. Any SDK holding that DSN reports into it — the
`platform` argument is a label, not a filter. Use one project per platform
(`shop-ios`, `shop-android`): issues never merge across platforms, but the
overview's "latest release" and the crash-spike baseline are computed per
project, so mixing platforms in one project blends those two numbers.

Open <http://localhost:8080> for the viewer. `docker compose exec crashcart
/crashcart seed` writes a week of demo data into a project called `demo`.

Without Docker: `make build`, then `DATABASE_URL=postgres://… bin/crashcart
serve`. Migrations run at startup; the database role needs `CREATE` on the
database (or a superuser must `CREATE EXTENSION timescaledb` first).

## Configuration

All configuration is by environment variable (`internal/config/config.go`).

| Variable | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | — (required) | Postgres URL; the database must allow the `timescaledb` extension |
| `LISTEN_ADDR` | `:8080` | HTTP listen address |
| `PUBLIC_URL` | derived from the request | Externally visible base URL, used when printing DSNs |
| `API_KEYS` | empty (open) | Comma-separated Bearer tokens for `/api/*` |
| `VIEWER_PASSWORD` | empty (open) | HTTP basic-auth password for the viewer (any username) |
| `CORS_ORIGIN` | `*` | `Access-Control-Allow-Origin` for ingest and API |
| `RATE_LIMIT` | `600` | Requests per minute per credential (DSN key / API key); `0` disables |
| `RETENTION_DAYS` | `30` | Raw events and sessions are dropped after this many days (whole chunks) |
| `COMPRESS_AFTER` | `48h` | Chunks older than this are compressed; symbol uploads re-symbolicate only newer events |
| `ALERT_INTERVAL` | `10m` | How often the crash-spike detector runs |
| `WORKERS` | `4` | Job worker goroutines (symbolication, alert delivery) |
| `SYMBOLICATE_URL` | empty (off) | Base URL of the dSYM symbolication sidecar |
| `TELEGRAM_BOT_TOKEN` | empty | Bot token for Telegram alert channels |
| `PII_REDACT` | `false` | Scrub emails, phone numbers, tokens and user ids from stored events |
| `CUSTOM_TAGS` | empty | Comma-separated tag keys the viewer offers as filters |

## Commands

```
crashcart serve                  HTTP server + job worker + schedulers (default)
crashcart migrate                apply pending migrations and exit
crashcart retention              reconcile Timescale policies and run one sweep
crashcart alerts                 run one crash-spike check
crashcart seed [slug]            write a week of demo data (default project "demo")
crashcart export [slug]          stream NDJSON to stdout (all projects, or one)
crashcart import                 load NDJSON from stdin (idempotent)
crashcart project <slug> <name> [platform]   create a project and print its DSN
```

## Export / import

`crashcart export > backup.ndjson` streams every table as newline-delimited
JSON, one object per row, `"t"` naming the table. The first line is
`{"t":"_meta","format":1,"exported_at":<unix ms>,"app":"crashcart"}`; then
the tables in this order: `projects`, `issues`, `events`, `sessions`,
`symbol_files`, `alert_rules`, `alert_channels`.

- Rows refer to their project by `"project": "<slug>"`, never by id, so a
  dump loads into any database.
- Time-series ids (`events.id`, `sessions.id`) are integers and are kept —
  they encode the event time (`internal/pk`). Identity ids of projects,
  symbol files and alert channels are not exported; their natural keys are.
- `TIMESTAMPTZ` columns are unix milliseconds, JSON columns are embedded
  JSON, `BYTEA` is base64, `NULL` columns are omitted.
- Aggregates, jobs and rate limits are not exported: aggregates recompute
  from the raw tables, the others expire.

`crashcart import < backup.ndjson` upserts: events and sessions with
`ON CONFLICT DO NOTHING`, issues / symbol files / alert rules on their
natural key (issue counts are replaced, not added), alert channels only when
no identical `(project, kind, config)` row exists. Unknown project slugs are
created. Importing twice, or onto a live database, is safe. Lines with an
unknown `"t"` are counted under `skipped` and ignored, so newer exports
still load on older builds. Per-table row counts are printed to stderr.

## Symbolication

| Symbol kind | Platforms | How |
|---|---|---|
| ProGuard / R8 mapping | Android | In-process; resolved inline at ingest when the mapping is cached, otherwise by the job worker |
| Source maps | JavaScript, React Native | In-process, matched by release |
| dSYM | iOS, macOS | Sidecar container (`container/symbolicate`, `llvm-symbolizer`); enable with `SYMBOLICATE_URL` |

Upload symbol files through the API:

```sh
curl -H "Authorization: Bearer $API_KEY" -F kind=proguard -F release=2.4.1 \
     -F file=@mapping.txt http://localhost:8080/api/projects/shop/symbols
```

or with sentry-cli, which speaks the Sentry chunked-upload protocol
(`/api/0/organizations/<org>/chunk-upload/` + `…/files/difs/assemble/`;
sentry-cli 2 may also use the legacy `…/files/dsyms/` upload). The
organization segment is ignored; the project segment is the CrashCart slug
or numeric id:

```sh
export SENTRY_URL=http://localhost:8080 SENTRY_AUTH_TOKEN=$API_KEY SENTRY_ORG=any SENTRY_PROJECT=shop
sentry-cli debug-files upload path/to/App.dSYM        # dSYM: debug_id = LC_UUID (arm64 slice of a fat binary)
sentry-cli upload-proguard --write-properties sentry-debug-meta.properties mapping.txt
```

Files uploaded this way carry no release; they are matched by `debug_id`
(`debug_meta.images` in the event). Set `PUBLIC_URL` so the chunk-upload
URL handed to sentry-cli is reachable from where it runs.

Uploading a symbol file re-queues the release's unsymbolicated events from
the last `COMPRESS_AFTER`. Symbolicated frames are stored beside the
original payload, which is never rewritten.

## API

JSON, snake_case, RFC3339 UTC times, integer ids. Bearer auth with one of
`API_KEYS` when set. Handlers are in `internal/api`.

```
GET    /api/projects                          POST /api/projects
GET    /api/projects/{slug}                   PATCH | DELETE /api/projects/{slug}
GET    /api/projects/{slug}/overview          totals, timeline, top issues
GET    /api/projects/{slug}/issues            ?status=&release=&q=&from=&to=&before=
GET    /api/projects/{slug}/issues/{fingerprint}
PATCH  /api/projects/{slug}/issues/{fingerprint}   {"status": "resolved"}
POST   /api/projects/{slug}/issues/bulk       {"fingerprints": [...], "status": "..."}
GET    /api/projects/{slug}/events            filters: level, release, environment, platform, user_id, tags…
GET    /api/projects/{slug}/events/{id}       full payload, breadcrumbs, symbolicated frames
GET    /api/projects/{slug}/releases          crash-free rate per release
GET    /api/projects/{slug}/releases/{version}
GET    /api/projects/{slug}/alerts            rules + channels
PATCH  /api/projects/{slug}/alerts/{type}     new_issue | regression | crash_spike
POST   /api/projects/{slug}/alerts/channels   {"kind":"webhook","config":{"url":…}} | {"kind":"telegram","config":{"chat_id":…}}
DELETE /api/projects/{slug}/alerts/channels/{id}
GET    /api/projects/{slug}/symbols           POST (multipart: kind, release, file)   DELETE …/symbols/{id}
GET    /health

POST   /api/{project_id}/envelope/            Sentry envelope ingest (authenticated by the DSN key)
POST   /api/{project_id}/store/               legacy single-event ingest
GET|POST /api/0/projects/{org}/{slug}/files/dsyms/   sentry-cli legacy debug-file upload / lookup by debug_id
GET|POST /api/0/organizations/{org}/chunk-upload/     sentry-cli chunked upload (options / chunks)
POST     /api/0/projects/{org}/{slug}/files/difs/assemble/   sentry-cli assemble
```

Transactions, profiles, replays and client reports in envelopes are
accepted and dropped.

## Verified against

Exercised end to end with real clients (not hand-built envelopes), each
sending a message, a handled exception with user/tag/breadcrumb, and an
unhandled crash through the SDK's own crash path:

| SDK | Version | Notes |
|---|---|---|
| sentry-python | 2.68 | gzip-compressed envelopes |
| @sentry/node | 10.72 | |
| @sentry/browser | 10.72 | real Chromium, CORS + preflight |
| @sentry/bun | 10.72 | |
| sentry-go | 0.49 | bare-array `exception`/`threads`, panics as message events |
| sentry-rust | 0.49 | `sentry_panic` frames filtered out of the location |
| sentry-java | 8.54 | plain JVM, chained exceptions |
| sentry-android-core | 8.14 | API 35 emulator, crash-cache resend, R8 mapping with inlined frames |
| Sentry Android Gradle plugin | 4.14 | automatic ProGuard mapping upload |
| sentry-dotnet | 6.9 | exception without frames → thread stack |
| sentry-dart | 9.28 | async placeholders and SDK frames excluded from grouping |
| sentry-native | master (inproc) | address-only frames grouped by image+offset |
| sentry-cli | 3.7 | `debug-files upload`, `upload-proguard`, chunked upload |

Not yet: Cocoa/iOS (needs macOS for the dSYM sidecar), Unity/Unreal/Godot,
Ruby, PHP, Elixir, React Native, Flutter.

## Operations

- **TimescaleDB is required.** `events` and `sessions` are hypertables keyed
  by the microsecond id; hourly/daily statistics are continuous aggregates,
  so nothing is kept in sync by hand.
- **Policies come from the environment.** At startup (and on `crashcart
  retention`) compression (`COMPRESS_AFTER`) and retention
  (`RETENTION_DAYS`) policies are reconciled against the live database;
  changing an env var and restarting is enough. Aggregates keep 400 days.
- **Backups** are `crashcart export > backup.ndjson`, restorable with
  `crashcart import` into any CrashCart database. `pg_dump` works too.
- **Jobs** (symbolication, alert delivery) are rows in Postgres; the worker
  retries with backoff and drops a job after 8 attempts. `crashcart alerts`
  and `crashcart retention` run one pass each for cron-style operation.
- **Scaling.** The binary is stateless: run several behind a load balancer
  against one database. Ingest is one transaction per envelope; sampling
  (`sample_keep_first`, `sample_rate` per project) bounds storage per issue
  while issue counts stay exact.

## Development

```
make generate      sqlc generate + templ generate (generated files are committed)
make build         → bin/crashcart
make test          go vet + unit tests
make test-db       DB-backed tests; needs TEST_DATABASE_URL (see Makefile for a docker run one-liner)
make css           rebuild internal/web/assets/app.css (needs npm install)
make docker        build the image
```

## License

MIT
