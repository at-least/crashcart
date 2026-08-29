# Postgres options

CrashCart needs Postgres 16 or newer. TimescaleDB is optional.

## With or without TimescaleDB

CrashCart uses TimescaleDB when it's available and plain Postgres
otherwise. Nothing changes in how you use it. The difference:

| | Plain Postgres | With TimescaleDB |
|---|---|---|
| Works on | Neon, Supabase, RDS, Cloud SQL, any Postgres | The `timescale/timescaledb` image, Timescale Cloud, self-managed |
| Storage | 5–10× more (no compression) | Compressed after 48 h |
| Matters at | — | Tens of millions of events a month |

Below a few million events a month there is no practical difference.
Pick whatever Postgres you already have.

The choice is made the first time CrashCart starts against a database and
stays. To switch later, [export and import](./operations#moving-to-another-database)
into a new database.

The `TIMESCALE` variable controls detection: `auto` (default), `on` or
`off`.

## Managed Postgres

Point `DATABASE_URL` at your provider and start CrashCart:

```sh
DATABASE_URL=postgres://user:pass@host/crashcart?sslmode=require \
PUBLIC_URL=https://crashcart.example.com \
API_KEYS=… VIEWER_PASSWORD=… \
crashcart serve
```

The database user needs permission to create tables. On providers that
offer TimescaleDB, either let the user create the extension or create it
once as an admin: `CREATE EXTENSION timescaledb`.

A zero-ops setup is the CrashCart Docker image on Cloud Run or Fly.io with
`DATABASE_URL` pointing at Neon.

## Running the binary directly

```sh
make build          # → bin/crashcart (needs Go 1.24+)
bin/crashcart serve
```

`serve` is the whole application — web, ingest, background work — in one
process. A minimal systemd unit:

```ini
[Unit]
Description=CrashCart
After=network.target

[Service]
EnvironmentFile=/etc/crashcart.env
ExecStart=/usr/local/bin/crashcart serve
Restart=always
User=crashcart

[Install]
WantedBy=multi-user.target
```

with the environment variables in `/etc/crashcart.env`.
