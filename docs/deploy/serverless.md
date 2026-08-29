# Serverless edition

[crashcart-serverless](https://github.com/crashcartapp/crashcart-serverless)
is CrashCart on Cloudflare Workers. Same SDK setup, same viewer, same export format.

## When to use it

| | Go edition | Serverless edition |
|---|---|---|
| You need | A server and Postgres | A Cloudflare account |
| Cost for a small app | A small VM | Free plan |
| Volume | Millions of events a month and up | Small apps |
| iOS symbolication | Optional sidecar | Needs Workers Paid |

## Deploy

```sh
git clone https://github.com/crashcartapp/crashcart-serverless
cd crashcart-serverless
npm install
npx wrangler login
npx wrangler d1 create crashcart          # paste the database_id into wrangler.jsonc
npx wrangler r2 bucket create crashcart-blobs
npm run db:migrate:remote
npx wrangler secret put API_KEYS
npx wrangler secret put VIEWER_PASSWORD   # optional
npx wrangler deploy
```

Open the Worker URL, create a project, copy its DSN. The same optional
settings apply: `PUBLIC_URL`, `RETENTION_DAYS`, `CORS_ORIGIN`,
`PII_REDACT`, `CUSTOM_TAGS`, `TELEGRAM_BOT_TOKEN`.

## Moving data between editions

```sh
# serverless → Go
curl -H "Authorization: Bearer $KEY" https://<worker>/api/export > dump.ndjson
crashcart import < dump.ndjson

# Go → serverless
crashcart export > dump.ndjson
node scripts/import.mjs --url https://<worker> --key $KEY dump.ndjson
```
