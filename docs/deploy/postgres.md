# Binary & managed Postgres

CrashCart is a single binary. Bring any Postgres 16+ — with or without
TimescaleDB.

## Building

```sh
make build            # → bin/crashcart (needs Go 1.24+)
# or
make docker           # the image used by docker-compose
```

Go builds need no Node: the viewer's CSS is a committed artifact.

## Running

```sh
export DATABASE_URL=postgres://user:pass@host:5432/crashcart?sslmode=require
export PUBLIC_URL=https://crashcart.example.com
export API_KEYS=…
export VIEWER_PASSWORD=…
bin/crashcart serve
```

`serve` runs the HTTP server, the job worker and the schedulers in one
process. Migrations run at startup; the database role needs `CREATE` on
the database. With TimescaleDB, either the role can `CREATE EXTENSION` or a
superuser runs `CREATE EXTENSION timescaledb` once beforehand.

The full list of variables is in [Configuration](./configuration).

## TimescaleDB or plain Postgres

The migrator picks a schema variant on first run:

| `TIMESCALE` | Behaviour |
|---|---|
| `auto` (default) | Probes `CREATE EXTENSION timescaledb`; uses it if it succeeds |
| `on` | Requires it; fails otherwise |
| `off` | Plain Postgres even if the extension exists |

A database keeps the variant it was created with. There is no in-place
conversion: to move between them, `crashcart export` from the old database
and `crashcart import` into a freshly migrated new one.

**With TimescaleDB** — `events` and `sessions` are hypertables keyed by
time, hourly/daily statistics are continuous aggregates, retention drops
whole chunks, data older than `COMPRESS_AFTER` is compressed (typically
10–20× on Sentry payloads).

**Plain Postgres** — the same statistics are rolled up by the scheduler
every 10 minutes and served through views that add the current hour live;
retention deletes in batches of 5000. Behaviour is identical; the API and
viewer don't know the difference. Plan for **5–10× the storage** (no
compression). Below a few million events a month there is no measurable
difference; TimescaleDB starts paying off in storage and retention churn at
tens of millions.

## Managed providers

| Provider | Notes |
|---|---|
| **Neon** | Plain Postgres (ships the Apache-2 TimescaleDB edition only, which lacks compression and continuous aggregates). Works with `TIMESCALE=auto`. |
| **Supabase** | Plain Postgres (extension removed on PG17). |
| **RDS / Cloud SQL / Azure** | Plain Postgres. |
| **Timescale Cloud** | Full TimescaleDB; the managed option when you want compression. |
| **Self-managed** | `timescale/timescaledb:latest-pg16` image, as in the compose file. |

A zero-ops deployment is the Docker image on **Cloud Run** or **Fly.io**
(scale to zero) with `DATABASE_URL` pointing at Neon. Because
`crashcart serve` runs the schedulers in-process, a scaled-to-zero instance
runs retention and crash-spike checks only while it is up; for strict
schedules, call `crashcart retention` and `crashcart alerts` from a cron
job instead ([Operations](./operations#cron-style-operation)).

## Running as a service

A minimal systemd unit:

```ini
[Unit]
Description=CrashCart
After=network.target postgresql.service

[Service]
EnvironmentFile=/etc/crashcart.env
ExecStart=/usr/local/bin/crashcart serve
Restart=always
User=crashcart

[Install]
WantedBy=multi-user.target
```

with `/etc/crashcart.env` holding the variables above (mode `0600`).
