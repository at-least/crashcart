# Docker Compose

The recommended way to self-host: one `docker-compose.yml`, two
containers (CrashCart and Postgres), one volume.

```sh
git clone https://github.com/crashcartapp/crashcart
cd crashcart
docker compose up -d
```

CrashCart is now on port 8080. Migrations run automatically.

## Before exposing it

Edit the `crashcart` service's `environment` in `docker-compose.yml`:

```yaml
environment:
  # The address your apps and browsers use. Shown in DSNs and alert links.
  PUBLIC_URL: https://crashcart.example.com
  # Protect the API. Comma-separated; use long random strings.
  API_KEYS: "…"
  # Protect the viewer (any username, this password).
  VIEWER_PASSWORD: "…"
  # Only if you use a browser SDK and want to restrict which sites may send events.
  # CORS_ORIGIN: https://shop.example.com
  # Days to keep raw events.
  RETENTION_DAYS: "30"
```

Then put a reverse proxy with TLS in front of port 8080 (Caddy, nginx,
Traefik). CrashCart does not terminate TLS itself.

::: tip
The ingest endpoint your apps send to is always reachable with just the
DSN key — that's how Sentry SDKs work. `API_KEYS` and `VIEWER_PASSWORD`
protect everything else. Set both.
:::

All settings are listed in [Configuration](./configuration).

## iOS crashes (dSYM sidecar)

To symbolicate iOS / macOS crashes, uncomment the `symbolicate` service
and add `SYMBOLICATE_URL: http://symbolicate:8080` to `crashcart`'s
environment. Android and JavaScript symbolication need nothing extra.

## Upgrading

```sh
docker compose pull      # or: docker compose build crashcart
docker compose up -d
```

Migrations and any changed retention settings apply on start.

## Backups

```sh
docker compose exec crashcart /crashcart export > backup.ndjson
```

Restore into any CrashCart with `crashcart import < backup.ndjson`. See
[Operations](./operations#backups).

## More than one instance

CrashCart keeps no state of its own, so you can run several containers
against the same database behind a load balancer.
