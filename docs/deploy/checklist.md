# Before going live

Whichever way you installed CrashCart, check these before pointing real
apps at it.

- [ ] **HTTPS.** Apps send crash reports over the DSN's URL; put TLS in
      front (Caddy, nginx, a load balancer). CrashCart does not terminate
      TLS itself.
- [ ] **`PUBLIC_URL`** is the HTTPS address your apps use. It's what
      appears in DSNs, alert links and `sentry-cli` uploads.
- [ ] **`API_KEYS`** is set. Without it the API — projects, issues, symbol
      uploads — is open to anyone who can reach the server.
- [ ] **`VIEWER_PASSWORD`** is set, unless the viewer is only reachable on a
      private network.
- [ ] **Postgres password** is not the default `crashcart`
      (`POSTGRES_PASSWORD` in `.env` for Docker Compose).
- [ ] **Backups.** Schedule `crashcart export > backup.ndjson` (or
      `pg_dump`). See [Operations](./operations#backups).
- [ ] **Retention** (`RETENTION_DAYS`, default 30) matches how long you
      want to keep raw events.
- [ ] Health check `GET /health` is wired into your monitoring.

Details for every setting: [Configuration](./configuration).
