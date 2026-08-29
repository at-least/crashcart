# CrashCart

Open-source error tracking for mobile and web apps. Works with the
**Sentry SDK**: point any Sentry SDK at CrashCart and your crashes, errors
and release health show up here instead. Keep the client libraries you
already ship; swap only the backend.

One binary, one Postgres, nothing else.

**Documentation: [crashcart.app](https://crashcart.app)**

## Quick start

```sh
git clone https://github.com/crashcartapp/crashcart
cd crashcart
docker compose up -d
docker compose exec crashcart /crashcart project shop-ios "Shop app (iOS)" ios
# project shop-ios (id 1)
# DSN: http://<key>@localhost:8080/1
```

Paste the DSN into the SDK and open <http://localhost:8080>.

No Docker? Download a [release binary](https://github.com/crashcartapp/crashcart/releases)
and follow [Go binary + systemd](https://crashcart.app/deploy/binary).

## What you get

- **Issues, not logs** — crashes with the same stack trace are one issue,
  with a status, an exact count and the releases it was seen on
- **Release health** — crash-free rate per release, and what a release broke
- **Readable stack traces** — upload ProGuard / R8 mappings, source maps or
  dSYMs with `curl` or `sentry-cli`
- **Alerts** — new issue, regression, crash spike → webhook or Telegram
- **Your data, portable** — one command exports everything to a plain file
  that restores into any CrashCart

Runs on plain Postgres (Neon, Supabase, RDS, …) or TimescaleDB. There is
also a [serverless edition](https://github.com/crashcartapp/crashcart-serverless)
for Cloudflare Workers.

## Learn more

- [Getting started](https://crashcart.app/guide/getting-started)
- [Connect an SDK](https://crashcart.app/guide/sdks)
- [Deploy with Docker Compose](https://crashcart.app/deploy/docker) · [Go binary + systemd](https://crashcart.app/deploy/binary)
- [Configuration](https://crashcart.app/deploy/configuration)
- [CLI](https://crashcart.app/reference/cli) · [HTTP API](https://crashcart.app/reference/api)
- [SDK compatibility](https://crashcart.app/reference/sdks)

## Contributing

Design notes are in [ARCHITECTURE.md](ARCHITECTURE.md), code layout and
conventions in [CLAUDE.md](CLAUDE.md). `make test` runs the unit tests;
`make test-db` the database-backed ones (see the Makefile for a one-line
TimescaleDB container).

## License

[MIT](LICENSE)
