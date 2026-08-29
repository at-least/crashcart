# Serverless edition (Cloudflare Workers + D1)

[crashcart-serverless](https://github.com/crashcartapp/crashcart-serverless)
is CrashCart on Cloudflare Workers, with D1 as the database and R2 for
blobs. Same SDK setup, same viewer, same export format.

## When to use it

Nothing to run, free for small apps, $5/month for most others. See
[Which edition?](/deploy/which-edition) for the comparison with the Go
edition and the limits worth knowing.

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
