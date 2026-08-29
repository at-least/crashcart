# Before going live

Whichever way you installed CrashCart, check these before pointing real
apps at it.

- [ ] **HTTPS.** Apps send crash reports over the DSN's URL; put TLS in
      front (Caddy, nginx, a load balancer). CrashCart does not terminate
      TLS itself.
- [ ] **`PUBLIC_URL`** is the HTTPS address your apps use. It's what
      appears in DSNs, alert links and `sentry-cli` uploads.
- [ ] **First account created.** Open the viewer once and create it on
      `/setup` — until then anyone reaching the server can claim it.
- [ ] **API keys** exist only for what needs them (CI uploads, scripts),
      and revoked ones are gone from `crashcart apikey list`.
- [ ] **Postgres and MinIO passwords** are not the defaults
      (`POSTGRES_PASSWORD`, `MINIO_PASSWORD` in `.env` for Docker Compose).
- [ ] **The bucket is CrashCart's own** — it replaces the bucket's
      lifecycle rules at startup. Check the startup log: if the rules could
      not be set, create them by hand.
- [ ] **Backups.** Schedule `crashcart export > backup.ndjson` (or
      `pg_dump` plus a bucket copy). See [Operations](./operations#backups).
- [ ] **Retention** (`RETENTION_DAYS`, default 30) matches how long you
      want to keep raw events.
- [ ] Health check `GET /health` is wired into your monitoring.

Details for every setting: [Configuration](./configuration). What is
stored and who can reach it: [Security & privacy](./security).
