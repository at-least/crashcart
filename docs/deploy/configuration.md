# Configuration

Everything is set with environment variables.

## Required

| Variable | Meaning |
|---|---|
| `DATABASE_URL` | Postgres connection URL |
| `S3_BUCKET` | The bucket for event payloads and symbol files (dedicated to CrashCart: it sets the bucket's lifecycle rules) |
| `S3_ACCESS_KEY`, `S3_SECRET_KEY` | Credentials for it |
| `S3_ENDPOINT` | The S3-compatible endpoint, e.g. `http://minio:9000`, `https://<account>.r2.cloudflarestorage.com`. Leave empty for AWS S3 |
| `S3_REGION` | AWS region (`us-east-1` when empty); other providers usually ignore it |
| `S3_PREFIX` | optional key prefix inside the bucket |

See [The database and the object store](./postgres).

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
| `RETENTION_DAYS` | `30` | Days to keep raw events and sessions (whole weekly partitions are dropped, so up to a week longer). Payloads in the bucket expire a week after that, symbol files after twice it. Issues and statistics are kept longer |
| `PII_REDACT` | `false` | Scrub emails, phone numbers, tokens and user ids before storing events |

Changing a data setting and restarting is enough — it applies to existing
data too (the bucket's lifecycle rules are reset at startup).

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
