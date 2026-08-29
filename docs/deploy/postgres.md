# The database

CrashCart needs **Postgres 16 or newer with TimescaleDB** — the Community
build (`timescaledb.license = timescale`), which is what gives compressed
storage (5–10× smaller after 48 h), free retention (a day's chunk is
dropped, not deleted row by row) and the pre-computed stats behind the
overview and release-health pages. This is what the Docker Compose and
Kubernetes installs run.

## Where to get it

| | |
|---|---|
| Self-hosted | The [`timescale/timescaledb`](https://hub.docker.com/r/timescale/timescaledb) image, or the [TimescaleDB package](https://docs.tigerdata.com/self-hosted/latest/install/) for your distro next to your own Postgres |
| Managed | [Tiger Cloud](https://www.tigerdata.com/) (formerly Timescale Cloud) |

Most other managed Postgres hosts (Neon, Supabase, RDS, Cloud SQL, Azure,
Aiven, Render, …) either don't offer the extension or ship the Apache-2
build, which loads but refuses compression and continuous aggregates.
CrashCart checks the license on startup and refuses to start against
those, so you find out immediately rather than at the first compression
job.

## Permissions

The database user needs to create tables and the extension. On a host
where the user cannot create extensions, create it once as an admin:

```sql
CREATE EXTENSION timescaledb;
```

## Connection

Take the connection URL as `DATABASE_URL`:

```
DATABASE_URL=postgres://user:pass@host/crashcart?sslmode=require
```

Compression, chunk retention (`RETENTION_DAYS`, `COMPRESS_AFTER`) and the
stats refresh are TimescaleDB policies — they run inside the database and
need no cron on your side. See [Configuration](./configuration).
