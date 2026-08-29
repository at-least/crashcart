# Configuration

All configuration is by environment variable. Nothing is read from files.

## Database

| Variable | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | — **required** | Postgres URL |
| `TIMESCALE` | `auto` | `auto` probes for the extension; `on` requires it; `off` forces plain Postgres. Fixed on first migration — see [TimescaleDB or plain Postgres](./postgres#timescaledb-or-plain-postgres) |

## HTTP

| Variable | Default | Meaning |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | Listen address |
| `PUBLIC_URL` | derived from the request | Externally visible base URL. Used in printed DSNs, the chunk-upload URL returned to `sentry-cli`, and alert links. Set it whenever CrashCart is behind a proxy or a custom domain |
| `CORS_ORIGIN` | `*` | `Access-Control-Allow-Origin` for ingest and the API. Set to your web app's origin for browser SDKs |
| `RATE_LIMIT` | `600` | Requests per minute per credential (DSN key or API key), 60-second fixed windows. `0` disables |

## Authentication

| Variable | Default | Meaning |
|---|---|---|
| `API_KEYS` | empty (open) | Comma-separated Bearer tokens accepted on `/api/projects/…`, `/api/export`, `/api/import` and the `sentry-cli` routes |
| `VIEWER_PASSWORD` | empty (open) | HTTP basic-auth password for the viewer; any username |

Ingest (`/api/{id}/envelope/`, `/api/{id}/store/`) is always authenticated
by the project's DSN key, never by `API_KEYS`.

## Data lifecycle

| Variable | Default | Meaning |
|---|---|---|
| `RETENTION_DAYS` | `30` | Raw events and sessions are dropped after this many days. Issues and aggregates are kept (aggregates for 400 days) |
| `COMPRESS_AFTER` | `48h` | TimescaleDB: chunks older than this are compressed. Also the window in which a symbol upload re-symbolicates existing events |
| `PII_REDACT` | `false` | Scrub emails, phone numbers, tokens and user ids from events before storing |

Policies are reconciled against the live database on every start and by
`crashcart retention`, so changing a value and restarting is enough.

## Background work

| Variable | Default | Meaning |
|---|---|---|
| `WORKERS` | `4` | Job worker goroutines (symbolication, alert delivery) |
| `ALERT_INTERVAL` | `10m` | How often the crash-spike detector runs |
| `SYMBOLICATE_URL` | empty (off) | Base URL of the dSYM sidecar, e.g. `http://symbolicate:8080` |
| `TELEGRAM_BOT_TOKEN` | empty | Bot token used by Telegram alert channels |

## Viewer

| Variable | Default | Meaning |
|---|---|---|
| `CUSTOM_TAGS` | empty | Comma-separated tag keys offered as filters in the viewer, e.g. `tenant,feature_flag` |

## Per-project settings

Sampling (`sample_keep_first`, `sample_rate`) and `daily_quota` are
per-project, not environment variables. See
[Projects & DSNs](/guide/projects#sampling-and-quota).
