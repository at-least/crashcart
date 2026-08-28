# CrashCart

Open-source error tracking, Sentry SDK compatible. One Go binary + one Postgres.

## What

Point any Sentry SDK at CrashCart instead of sentry.io. Get crash tracking, stack trace
analysis, issue grouping, release health and alerting — on your own server.

- **Sentry-compatible** — change the DSN, done. The standard `/api/{project}/envelope/`
  endpoint and `X-Sentry-Auth` work as-is; iOS, Android, Flutter, JS, Python SDKs all work.
- **Stack trace analysis** — root cause (deepest in-app frame), app vs framework frames,
  breadcrumb user journey.
- **Issues** — fingerprint grouping with lifecycle (unresolved → triaged → resolved →
  regression, auto-detected when a resolved issue reappears in another release).
- **Alerting** — crash spike (3× weekly baseline), new error, regression; Telegram,
  Slack/Discord webhooks, SMTP email.
- **Release health** — crash-free sessions per release from Sentry session items.
- **Symbolication** — ProGuard/R8 mappings and JS source maps in-process; dSYM via a
  sidecar container.
- **Viewer** — server-rendered dashboard (templ + htmx + shadless/Tailwind): stat cards,
  charts, issue list, dense event table with click-to-filter, detail sheet, dark mode.
- **PII redaction + sampling** — for compliance and cost.

Storage is write-cost first: every table has only its primary key, and the event time is
encoded in `events.id`, so an event costs one row + one index entry and every read is a
bounded primary-key range scan. If a filter gets slow at your volume, add the specific
index you need from `sql/optional_indexes.sql` (never applied automatically). See
[ARCHITECTURE.md](ARCHITECTURE.md).

## Quick start

```bash
docker compose up -d            # Postgres + CrashCart on http://localhost:8080
docker compose exec crashcart crashcart seed   # optional demo data
```

Or run from source:

```bash
docker run -d --name crashcart-pg -e POSTGRES_PASSWORD=crashcart -e POSTGRES_USER=crashcart \
  -e POSTGRES_DB=crashcart -p 5432:5432 postgres:16-alpine
cp .env.example .env && set -a && . ./.env && set +a
go run ./cmd/crashcart seed     # migrations run automatically
go run ./cmd/crashcart          # serve
```

Open http://localhost:8080.

Point your app at it — either DSN form works:

```swift
// Native Sentry DSN: token as the DSN key, any numeric project id
options.dsn = "http://INGEST_TOKEN@crashcart.example.com/1"
// or the explicit endpoint
options.dsn = "http://x@crashcart.example.com/ingest?token=INGEST_TOKEN"
```

## Configuration

| Var | Default | Description |
|---|---|---|
| `DATABASE_URL` | — | Postgres connection string (required) |
| `LISTEN_ADDR` | `:8080` | HTTP listen address |
| `API_KEYS` | — | Comma-separated Bearer keys for `/api/*` (unset = open, dev only) |
| `INGEST_TOKEN` | — | Token for ingest (unset = open, dev only) |
| `RATE_LIMIT` | `100` | Requests per minute per key/IP; `0` disables |
| `CORS_ORIGIN` | `*` | Allowed origin for `/ingest` and `/api/*` |
| `PII_REDACT` | `false` | Scrub emails/phones/cards from messages + tags, mask user ids |
| `SAMPLE_RATE` | `1.0` | Keep ratio for info/debug (warning ≥ 50%, error/fatal always) |
| `RETENTION_DAYS` | `30` | Delete data older than this |
| `RETENTION_INTERVAL` | `1h` | How often retention runs |
| `ALERT_INTERVAL` | `10m` | How often alert detectors run |
| `CUSTOM_TAGS` | — | Extra searchable viewer columns: `build:Build,region:Region` |
| `DEPLOYMENTS` | — | Multi-instance portal: `iOS\|https://ios.example.com\|key,Android\|…` |
| `SYMBOLICATE_URL` | — | dSYM sidecar base URL (`container/symbolicate`) |
| `TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHAT_IDS` | — | Telegram alerts |
| `ALERT_WEBHOOKS` | — | Comma-separated Slack/Discord-compatible webhook URLs |
| `ALERT_EMAILS`, `EMAIL_FROM`, `SMTP_ADDR`, `SMTP_USER`, `SMTP_PASSWORD` | — | Email alerts (STARTTLS, or implicit TLS on :465) |

## Commands

```
crashcart serve       HTTP server + in-process schedulers (default)
crashcart migrate     apply pending migrations and exit
crashcart retention   one retention pass
crashcart alerts      one alert check
crashcart seed        write a week of demo events
crashcart export      stream every table as NDJSON to stdout (backup / migration)
```

### Export format

`crashcart export > dump.ndjson` writes one JSON object per row, `t` = table name, columns
in snake_case, in primary-key order. The encoding is storage-neutral so another CrashCart
edition (e.g. SQLite/D1) can load it without a Postgres dump: timestamps are unix
milliseconds, dates `YYYY-MM-DD`, JSON columns are embedded values, booleans `true/false`,
binary base64. Aggregate tables (`hourly_stats`, `issues`, `releases`, `release_health`)
are included so an importer never recomputes them from events — on a per-row-billed store
that recompute would cost ~0.5 extra writes per event.

Migrations run automatically on every start (`internal/db/migrations`, tracked in
`schema_migrations`, advisory-locked so replicas can start together).

## API

All `/api/*` routes need `Authorization: Bearer <key>` when `API_KEYS` is set. Times are
RFC3339 UTC; windows are `?days=N` or `?from=…&to=…` (exclusive `to`).

| Route | Description |
|---|---|
| `POST /ingest`, `POST /api/{project}/envelope/` | Sentry envelope ingest (gzip/deflate ok) |
| `GET /api/events` | List events. Filters: `level` (csv), `q`, `crash=1`, `release`, `error_type`, `fingerprint`, `user_id`, `device_id`, `device_model`, `os_version`, `error_location`, `tag.<key>=value`, `limit`, `offset` |
| `GET /api/events/{id}` | Full event incl. payload + breadcrumbs |
| `GET /api/stats` | `{fatal, crash, error, levels}` |
| `GET /api/stats/timeline` | Crashes per day (`hourly=1` → 24 hourly buckets) |
| `GET /api/stats/volume` | Fatal + error per bucket |
| `GET /api/stats/releases` | Per-release counters |
| `GET /api/stats/release_versions` | Versions active in the window |
| `GET /api/stats/release_health` | Crash-free sessions per release |
| `GET /api/issues`, `GET /api/issues/{fp}` | Issues (filters: `error_type`, `status`, `release`, `user_id`, `device_id`, `device_model`, `os_version`) |
| `PATCH /api/issues/{fp}` | `{"status": "resolved"}` |
| `GET /api/alerts`, `PATCH /api/alerts/{type}` | Detector toggles (`{"enabled": true}`) |
| `GET /api/alerts/channels` | Configured channels (masked) |
| `POST /api/symbols?platform=&release=&file=` | Upload a mapping file (raw body, ≤50 MB) |
| `GET /api/symbols` | List uploaded symbol files |
| `POST /api/symbolicate` | `{platform, release, frames}` → resolved frames |
| `GET /health` | `ok` when Postgres answers |

## Development

```bash
make generate      # sqlc + templ
make test          # unit tests
TEST_DATABASE_URL=postgres://crashcart:crashcart@localhost:5432/crashcart?sslmode=disable make test   # + integration
make css           # rebuild internal/web/assets/app.css from internal/web/styles/app.css (needs npm install)
```

The viewer stylesheet is Tailwind v4 + [shadless](../shadless); the compiled artifact is
committed so `go build` needs no Node.

## Layout

```
cmd/crashcart/         main: serve | migrate | retention | alerts | seed
internal/
  api/                 JSON API + ingest HTTP handlers
  alerts/              detectors + Telegram/webhook/SMTP channels
  auth/                bearer / ingest-token / rate-limit / CORS middleware
  config/              env parsing
  db/                  migrations (embedded) + runner; sqlc/ generated queries
  ingest/              envelope → COPY events + aggregate upserts (one tx); PII; sampling
  retention/           bounded deletes per table
  sentry/              envelope parser, fingerprint, stack-trace analysis
  server/              handler assembly (shared by main + tests)
  store/               read/update layer used by API and viewer
  symbolicate/         ProGuard, source map, dSYM client
  timerange/           from/to/days window parsing, zero-fill slots
  web/                 templ views, htmx routes, embedded assets, styles/
container/symbolicate/ dSYM sidecar (llvm-symbolizer)
```

## License

MIT. CrashCart is not affiliated with or endorsed by Sentry (Functional Software, Inc.);
"Sentry" is their trademark. CrashCart is compatible with the MIT-licensed Sentry SDKs.
