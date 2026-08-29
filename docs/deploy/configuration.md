# Configuration

Everything is set with environment variables.

## Required

| Variable | Meaning |
|---|---|
| `DATABASE_URL` | Postgres connection URL |

Access is not configured here: the viewer uses user accounts and the API
uses API keys, both managed in the viewer (**Account**) or with
`crashcart user` / `crashcart apikey` — see [Security](./security).

## Recommended

| Variable | Default | Meaning |
|---|---|---|
| `PUBLIC_URL` | derived from the request | The address your apps use. Shown in DSNs, used in alert links and by `sentry-cli`. Set it when CrashCart is behind a proxy or domain |
| `CORS_ORIGIN` | `*` | Which web origins may send events (the SDK endpoints). Set to your site for browser SDKs |
| `API_CORS_ORIGIN` | empty (no CORS) | Web origin allowed to call `/api/*` from a browser. Leave empty unless a browser app talks to the JSON API |

## Data

| Variable | Default | Meaning |
|---|---|---|
| `RETENTION_DAYS` | `30` | Days to keep raw events and sessions. Issues and statistics are kept longer |
| `PII_REDACT` | `false` | Scrub emails, phone numbers, tokens and user ids before storing events |
| `COMPRESS_AFTER` | `48h` | How long events stay editable. Symbol uploads re-symbolicate events newer than this; with TimescaleDB, older data is compressed |
| `CHUNK_INTERVAL` | `168h` (7 days) | Width of the TimescaleDB chunks that hold events and sessions. Keep a chunk (with its indexes) within about a quarter of the database's memory: `168h` suits up to a few tens of thousands of events a day, `24h` a few hundred thousand. Applies to new chunks; every query pays a little per chunk in the window, so do not go narrower than the volume needs |

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
| `RATE_LIMIT` | `600` | Requests per minute allowed per DSN key or API key, counted in memory per process (each replica enforces it on its own traffic). `0` disables |
| `ALERT_INTERVAL` | `10m` | How often to check for crash spikes |
| `WORKERS` | `4` | Parallelism for symbolication and alert delivery |

## Per project

Sampling and daily quota are set per project in the viewer
(Settings → Sampling). See [Projects & DSNs](/guide/projects#sampling-and-daily-quota).
