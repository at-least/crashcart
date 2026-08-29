# Postgres options

CrashCart needs Postgres 16 or newer. TimescaleDB is optional.

## With or without TimescaleDB

CrashCart uses TimescaleDB when it's available and plain Postgres
otherwise. Nothing changes in how you use it. The difference:

| | Plain Postgres | With TimescaleDB |
|---|---|---|
| Works on | Neon, Supabase, RDS, Cloud SQL, a local `apt install postgresql` | The `timescale/timescaledb` image (what Docker Compose uses), Timescale Cloud, self-managed |
| Storage | 5–10× more (no compression) | Compressed after 48 h |
| Matters at | — | Tens of millions of events a month |

Below a few million events a month there is no practical difference.
Pick whatever Postgres you already have.

The choice is made the first time CrashCart starts against a database and
stays. To switch later, [export and import](./operations#moving-to-another-database)
into a new database.

The `TIMESCALE` variable controls detection: `auto` (default), `on` or
`off`.

## Permissions

The database user needs to create tables. On hosts that offer TimescaleDB,
either let the user create the extension or create it once as an admin:

```sql
CREATE EXTENSION timescaledb;
```

## Managed Postgres

Take the provider's connection URL as `DATABASE_URL`:

```
DATABASE_URL=postgres://user:pass@host/crashcart?sslmode=require
```

For a zero-ops setup see [Fly.io + Neon](./fly).
