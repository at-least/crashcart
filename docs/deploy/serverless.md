# Serverless edition

**crashcart-serverless** (a separate repository)
is the same product on **Cloudflare Workers + D1 + R2**. Same ingest
protocol, same fingerprinting, same viewer, same export format — data moves
between the two editions in either direction.

## When to use which

| | Go edition | Serverless edition |
|---|---|---|
| Runs on | Any host with Postgres | Cloudflare (Free plan is enough for small apps) |
| Ops | A container and a database | `wrangler deploy` |
| Volume | Millions of events a month and up | Small apps; the Free plan's ~10 ms CPU per request is the ceiling |
| dSYM symbolication | Optional sidecar | Container (Workers Paid) |
| ProGuard / source maps | Inline | Inline |

## Deploy

```sh
git clone <crashcart-serverless repository>
cd crashcart-serverless
npm install
npx wrangler login
npx wrangler d1 create crashcart          # paste the database_id into wrangler.jsonc
npx wrangler r2 bucket create crashcart-blobs
npm run db:migrate:remote
npx wrangler secret put API_KEYS          # Bearer tokens for /api/*
npx wrangler secret put VIEWER_PASSWORD   # optional
npx wrangler deploy
```

Open the Worker URL, create a project, copy the DSN from its settings page.

Optional variables mirror the Go edition: `PUBLIC_URL`, `RETENTION_DAYS`,
`CORS_ORIGIN`, `PII_REDACT`, `CUSTOM_TAGS`, `TELEGRAM_BOT_TOKEN`.

## Moving data

```sh
# serverless → Go
curl -H "Authorization: Bearer $KEY" https://<worker>/api/export > dump.ndjson
crashcart import < dump.ndjson

# Go → serverless
crashcart export > dump.ndjson
node scripts/import.mjs --url https://<worker> --key $KEY dump.ndjson   # batches of 500 rows
```

Both implementations are tested against dumps produced by the other. The
contract is the [export format](/reference/export-format).
