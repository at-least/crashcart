# Fly.io + Neon

The zero-ops option: CrashCart runs on [Fly.io](https://fly.io) and
scales to zero when idle; the database is a free
[Neon](https://neon.tech) Postgres. For a small app this costs nothing or
close to it. Every step below has been run as written.

::: info Storage: Go edition, plain Postgres
Neon ships TimescaleDB without compression, so CrashCart runs in plain
mode: full-size rows, batched retention — fine to a few million events a
month. Neon's free plan holds 0.5 GB. See
[three ways your data is stored](./which-edition#timescaledb-and-compression).
:::

**You need**

- The `flyctl` CLI, logged in (`curl -L https://fly.io/install.sh | sh`,
  then `fly auth signup` or `fly auth login`)
- A Neon account

## 1. Create the database on Neon

In the [Neon console](https://console.neon.tech), **New project**. Pick a
region — remember which; you'll put the Fly app next to it. On the
project page, copy the **connection string** (the direct one, not pooled):

```
postgresql://…@ep-….aws.neon.tech/neondb?sslmode=require
```

## 2. Create the Fly app

The repository ships a `fly.toml` (GHCR image, scale-to-zero, health
check). Clone it and launch with a name of your own:

```sh
git clone https://github.com/crashcartapp/crashcart
cd crashcart
fly launch --copy-config --no-deploy --name my-crashcart --region iad
```

Use a Fly region close to your Neon region (`iad` for AWS us-east,
`sjc` for us-west, `fra` for eu-central, `sin` for ap-southeast, …;
`fly platform regions` lists them). The app name must be unique on Fly;
it becomes `https://my-crashcart.fly.dev`.

## 3. Set the secrets

```sh
fly secrets set \
  DATABASE_URL='postgresql://…neon.tech/neondb?sslmode=require' \
  API_KEYS="$(openssl rand -hex 32)" \
  VIEWER_PASSWORD="$(openssl rand -hex 16)" \
  PUBLIC_URL=https://my-crashcart.fly.dev
```

Note the values you generated — `fly secrets list` shows only digests.

## 4. Deploy

```sh
fly deploy --ha=false
```

`--ha=false` keeps it to one machine, which is all a small install needs.
Then:

```sh
curl https://my-crashcart.fly.dev/health
# {"status":"ok"}
```

The first request after idle takes a second or two while the machine
starts.

## 5. Create a project

```sh
fly ssh console -C "/crashcart project shop-ios 'Shop app (iOS)' ios"
# project shop-ios (id 1)
# DSN: https://<key>@my-crashcart.fly.dev/1
```

Open `https://my-crashcart.fly.dev` (any username, your
`VIEWER_PASSWORD`), paste the DSN into the SDK —
[Connect an SDK](/guide/sdks) — and go through
[Before going live](./checklist) once.

## Custom domain

```sh
fly certs add crashcart.example.com
```

Add the DNS records it prints, then
`fly secrets set PUBLIC_URL=https://crashcart.example.com`.

## Upgrading

```sh
fly deploy --ha=false
```

pulls the newest release (`fly.toml` points at `:latest`). To pin a
version, set `image = "ghcr.io/crashcartapp/crashcart:0.1.1"` in
`fly.toml`.

## Scale to zero and housekeeping

`fly.toml` stops the machine when idle and starts it on the next request.
Retention and crash-spike checks run in the background while it is awake
— fine for most apps. If you need them on an exact schedule, set
`min_machines_running = 1` in `fly.toml` to keep it running.

## If something doesn't work

| Symptom | Check |
|---|---|
| `fly deploy` times out on health checks | `fly logs` — almost always a wrong `DATABASE_URL`. Fix with `fly secrets set` (it redeploys) |
| `functionality not supported under the current "apache" license` in the logs | Upgrade to CrashCart 0.1.1 or later, which detects Neon's TimescaleDB build and uses plain Postgres |
| The DSN says `http://localhost:8080` | `PUBLIC_URL` isn't set |
| `fly ssh console` fails | The machine is stopped; hit `/health` once first |
