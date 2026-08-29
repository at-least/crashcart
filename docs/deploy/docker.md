# Docker Compose

The repository's `docker-compose.yml` runs TimescaleDB and CrashCart. It is
the recommended way to self-host: two containers, one volume.

```sh
git clone https://github.com/crashcartapp/crashcart
cd crashcart
docker compose up -d
```

## Production settings

Edit the `crashcart` service's `environment` before exposing it:

```yaml
services:
  crashcart:
    environment:
      DATABASE_URL: postgres://crashcart:crashcart@db:5432/crashcart?sslmode=disable
      # Externally visible base URL: printed in DSNs, sentry-cli upload URLs, alert links.
      PUBLIC_URL: https://crashcart.example.com
      # Bearer tokens for /api/* (comma-separated). Empty = open.
      API_KEYS: "long-random-string-1,long-random-string-2"
      # HTTP basic auth for the viewer (any username). Empty = open.
      VIEWER_PASSWORD: "another-long-random-string"
      # Restrict browser ingest to your site(s). Default *.
      CORS_ORIGIN: https://shop.example.com
      RETENTION_DAYS: "30"
```

Then put a reverse proxy with TLS in front of port 8080 (Caddy, nginx,
Traefik). CrashCart does not terminate TLS itself. Ingest from the SDKs,
the viewer and the API all share the one port.

::: tip Ingest is public by design
`/api/{id}/envelope/` is authenticated only by the DSN key, as with any
Sentry-compatible backend — apps in the field must be able to reach it.
`API_KEYS` protects `/api/projects/…`; `VIEWER_PASSWORD` protects the UI.
Set both.
:::

Change the Postgres password in both services if the database port is ever
reachable from outside the compose network.

## dSYM sidecar (iOS / macOS)

Uncomment the `symbolicate` service and set `SYMBOLICATE_URL` on
`crashcart`:

```yaml
  crashcart:
    environment:
      SYMBOLICATE_URL: http://symbolicate:8080

  symbolicate:
    build: ./container/symbolicate
    restart: unless-stopped
```

ProGuard / R8 and source maps do not need the sidecar. See
[Symbolication](/guide/symbolication).

## Upgrading

```sh
docker compose pull      # or: docker compose build crashcart
docker compose up -d
```

Migrations run at startup. Retention and compression policies are
reconciled from the environment on every start, so changing
`RETENTION_DAYS` or `COMPRESS_AFTER` and restarting is enough.

## Backups

```sh
docker compose exec crashcart /crashcart export > backup.ndjson
```

restores with `crashcart import < backup.ndjson` into any CrashCart
database — including a plain-Postgres one or the serverless edition.
`pg_dump` of the volume works too. See [Operations](./operations#backups).

## Scaling out

The binary is stateless. Run several `crashcart` containers behind a load
balancer against one database; each runs the job worker and the schedulers,
which coordinate through Postgres (`SKIP LOCKED`). Ingest is one
transaction per envelope.
