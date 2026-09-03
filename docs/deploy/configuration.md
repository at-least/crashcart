# Configuration

Everything is set with environment variables.

## Required

| Variable | Meaning |
|---|---|
| `DATABASE_URL` | Postgres connection URL |

See [The database](./postgres).

Access is not configured here: the viewer uses user accounts and the API
uses API keys, both managed in the viewer (**Account**) or with `crashcart
user` / `crashcart apikey` — see [Security](./security).

## Recommended

| Variable | Default | Meaning |
|---|---|---|
| `PUBLIC_URL` | derived from the request | The address your apps use. Shown in DSNs, used in alert links and by `sentry-cli`. Set it when CrashCart is behind a proxy or domain |
| `CORS_ORIGIN` | `*` | Which web origins may send events (the SDK endpoints). Set to your site for browser SDKs |
| `API_CORS_ORIGIN` | empty (no CORS) | Web origin allowed to call `/api/*` from a browser. Leave empty unless a browser app talks to the JSON API |

## Data

| Variable | Default | Meaning |
|---|---|---|
| `RETENTION_DAYS` | `30` | Days to keep raw events and sessions (whole weekly partitions are dropped, so up to a week longer). Symbol files are kept twice as long; issues and statistics longer still |
| `PII_REDACT` | `false` | Scrub emails, phone numbers, tokens and user ids before storing events |

Changing a data setting and restarting is enough — it applies to existing
data too.

## Optional features

| Variable | Default | Meaning |
|---|---|---|
| `SYMBOLICATE_URL` | off | Address of the dSYM sidecar, e.g. `http://symbolicate:8080` |
| `SYMBOLICATE_CACHE_DIR` | `$TMPDIR/crashcart-symbols` | `crashcart symbolicate` only: where the sidecar keeps the dSYMs it has used |
| `SYMBOLICATE_CACHE_MAX_MB` | `4096` | `crashcart symbolicate` only: bound of that cache; least recently used dSYMs are dropped |
| `TELEGRAM_BOT_TOKEN` | — | Bot token for Telegram alerts |
| `WEBHOOK_ALLOW_PRIVATE` | `false` | Let webhooks target private addresses (10/8, 172.16/12, 192.168/16, fc00::/7) — a service on your LAN. Loopback, link-local (cloud metadata) and redirects are always refused |
| `CUSTOM_TAGS` | — | Comma-separated tag keys to offer as filters in the viewer, e.g. `tenant,feature_flag` |
| `BLOB_STORE` | `postgres` | Where the big bytes go — uploaded symbol files (ProGuard mappings, dSYMs, source maps) and raw event payloads: `postgres` — in the database, nothing else to run; `s3` — an S3-compatible bucket, which keeps the database to metadata (a fiftieth of the size) so backups, replication and exports stay small. Rows already written stay where they are |
| `S3_BUCKET` | — | `BLOB_STORE=s3`: the bucket (must exist) |
| `S3_ENDPOINT` | AWS | `BLOB_STORE=s3`: host[:port] of an S3-compatible store — MinIO, R2, Backblaze, Ceph — e.g. `minio:9000` or `https://<account>.r2.cloudflarestorage.com` (`http://` for a plain-HTTP MinIO on the LAN). Empty means AWS S3 |
| `S3_REGION` | `us-east-1` | `BLOB_STORE=s3`: the bucket's region (AWS); other stores ignore it |
| `S3_ACCESS_KEY` | — | `BLOB_STORE=s3`: static credentials, with `S3_SECRET_KEY`. Leave both empty to use the usual chain: `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`, the shared credentials file, or an instance / task / pod role |
| `S3_SECRET_KEY` | — | `BLOB_STORE=s3`: see `S3_ACCESS_KEY` |
| `S3_PREFIX` | — | `BLOB_STORE=s3`: key prefix inside the bucket, e.g. `crashcart/` |

## Tuning

| Variable | Default | Meaning |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | Port to listen on |
| `TRUST_PROXY` | `false` | Behind a reverse proxy: take the client address from `X-Forwarded-For` (rate limits per IP on the sign-in pages). Leave off when clients reach CrashCart directly — the header is then theirs to forge |
| `RATE_LIMIT` | `6000` | Requests per minute allowed per DSN key or API key (100/s — a burst of cached crashes after an outage fits), counted in memory per process (each replica enforces it on its own traffic). `0` disables |
| `ALERT_INTERVAL` | `10m` | How often to check for unhandled-error spikes |
| `WORKERS` | `4` | Parallelism for symbolication and alert delivery |

## Per project

Sampling is set per project in the viewer (Settings → Sampling). See
[Projects & DSNs](/guide/projects#sampling).
