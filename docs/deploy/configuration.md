# Configuration

Everything is set with environment variables.

## Required

| Variable | Meaning |
|---|---|
| `DATABASE_URL` | Postgres connection URL |

## Recommended

| Variable | Default | Meaning |
|---|---|---|
| `PUBLIC_URL` | derived from the request | The address your apps use. Shown in DSNs, used in alert links and by `sentry-cli`. Set it when CrashCart is behind a proxy or domain |
| `API_KEYS` | empty (open) | Comma-separated keys accepted as `Authorization: Bearer …` on the API and by `sentry-cli` |
| `VIEWER_PASSWORD` | empty (open) | Password for the web viewer (any username) |
| `CORS_ORIGIN` | `*` | Which web origins may send events. Set to your site for browser SDKs |

## Data

| Variable | Default | Meaning |
|---|---|---|
| `RETENTION_DAYS` | `30` | Days to keep raw events and sessions. Issues and statistics are kept longer |
| `PII_REDACT` | `false` | Scrub emails, phone numbers, tokens and user ids before storing events |
| `COMPRESS_AFTER` | `48h` | How long events stay editable. Symbol uploads re-symbolicate events newer than this; with TimescaleDB, older data is compressed |

Changing a data setting and restarting is enough — it applies to existing
data too.

## Optional features

| Variable | Default | Meaning |
|---|---|---|
| `SYMBOLICATE_URL` | off | Address of the dSYM sidecar, e.g. `http://symbolicate:8080` |
| `TELEGRAM_BOT_TOKEN` | — | Bot token for Telegram alerts |
| `CUSTOM_TAGS` | — | Comma-separated tag keys to offer as filters in the viewer, e.g. `tenant,feature_flag` |

## Tuning

| Variable | Default | Meaning |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | Port to listen on |
| `RATE_LIMIT` | `600` | Requests per minute allowed per DSN key or API key. `0` disables |
| `ALERT_INTERVAL` | `10m` | How often to check for crash spikes |
| `WORKERS` | `4` | Parallelism for symbolication and alert delivery |
| `TIMESCALE` | `auto` | `on` / `off` to force TimescaleDB or plain Postgres. See [Postgres options](./postgres) |

## Per project

Sampling and daily quota are set per project in the viewer
(Settings → Sampling). See [Projects & DSNs](/guide/projects#sampling-and-daily-quota).
