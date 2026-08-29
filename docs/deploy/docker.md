# Docker Compose on a VPS

CrashCart, Postgres and Caddy (for automatic HTTPS) on one Linux server.
Every step below has been run as written on a fresh Ubuntu machine.

::: info Storage: TimescaleDB
The compose file runs the `timescale/timescaledb` image: compressed
storage and free retention — comfortable at tens of millions of events a
month. See [The database](./postgres).
:::

**You need**

- A VPS with a public IP and ports **80** and **443** open (1 GB RAM is
  enough to start)
- A domain, e.g. `crashcart.example.com`, with an **A record** pointing at
  that IP
- SSH access as a user who can run `sudo`

## 1. Install Docker

```sh
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker $USER
newgrp docker
docker compose version
```

## 2. Get the compose file

```sh
git clone https://github.com/crashcartapp/crashcart
cd crashcart
```

## 3. Configure CrashCart

Settings live in a `.env` file next to `docker-compose.yml` (it is
git-ignored). Start from the example:

```sh
cp .env.example .env
```

and set these:

```sh
POSTGRES_PASSWORD=<long random string>
PUBLIC_URL=https://crashcart.example.com
API_KEYS=<long random string>
VIEWER_PASSWORD=<another long random string>
```

Generate the strings with `openssl rand -hex 32`. Everything else in the
file is optional and explained inline.

## 4. Add Caddy

Caddy gets a certificate from Let's Encrypt and renews it by itself.
Append this service to `docker-compose.yml` and a `caddy` volume:

```yaml
  caddy:
    image: caddy:2
    restart: unless-stopped
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy:/data
    depends_on:
      - crashcart

volumes:
  pgdata:
  caddy:
```

Create `Caddyfile` next to it, with your domain:

```
crashcart.example.com {
    reverse_proxy crashcart:8080
    request_body {
        max_size 25MB
    }
}
```

Remove the `ports:` block from the `crashcart` service — only Caddy needs
to be reachable from outside.

## 5. Start

```sh
docker compose up -d
docker compose logs -f caddy      # wait for "certificate obtained", then Ctrl-C
```

Check from your laptop:

```sh
curl https://crashcart.example.com/health
# {"status":"ok"}
```

## 6. Create a project

```sh
docker compose exec crashcart /crashcart project shop-ios "Shop app (iOS)" ios
# project shop-ios (id 1)
# DSN: https://<key>@crashcart.example.com/1
```

Open `https://crashcart.example.com` (any username, your
`VIEWER_PASSWORD`), then paste the DSN into the SDK —
[Connect an SDK](/guide/sdks).

Go through [Before going live](./checklist) once.

## Upgrading

```sh
cd crashcart
docker compose pull
docker compose up -d
```

`latest` follows the newest release. To pin a version, set
`image: ghcr.io/crashcartapp/crashcart:0.1.0` (or `:0.1` for the newest
patch release of that line) in `docker-compose.yml`.

## iOS crashes

To symbolicate iOS / macOS crashes, uncomment the `symbolicate` service
in `docker-compose.yml` and set `SYMBOLICATE_URL=http://symbolicate:8080`
in `.env`. Android and JavaScript need nothing extra.

## Backups

```sh
docker compose exec crashcart /crashcart export > backup-$(date +%F).ndjson
```

Put that in cron. Restore with `crashcart import < backup.ndjson`. See
[Operations](./operations#backups).

## If something doesn't work

| Symptom | Check |
|---|---|
| `certificate obtained` never appears | The A record must point at this server and ports 80/443 must be open (`sudo ufw status`, cloud firewall). Caddy retries by itself once DNS is right |
| `/health` returns `503` | Postgres isn't up yet: `docker compose logs db` |
| The DSN says `http://localhost:8080` | `PUBLIC_URL` isn't set in `.env`; fix it and `docker compose up -d` |
| SDK gets `401` | Wrong key in the DSN. `docker compose exec crashcart /crashcart rotate-key shop-ios` prints a fresh one |
| Large crash reports fail with `413` | Raise `max_size` in the Caddyfile |
