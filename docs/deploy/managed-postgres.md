# Managed Postgres providers

CrashCart runs on any Postgres 16 or newer. What differs between hosts is
whether they offer the **TimescaleDB** extension, and in which build:

- **Community build** (`timescaledb.license = timescale`) — compression,
  chunk-drop retention, continuous aggregates. CrashCart uses
  TimescaleDB mode: 5–10× less storage at volume.
- **Apache-2 build** — hypertables only, no compression. CrashCart treats
  this as plain Postgres. Most managed hosts ship this build.
- **Not available** — plain Postgres.

Plain Postgres is fine below a few million events a month; see
[Which edition?](/deploy/which-edition#timescaledb-and-compression) for
when it matters. Detection is automatic (`TIMESCALE=auto`), so you never
have to configure anything — this page only tells you what to expect.

## Support by provider

Checked against each provider's documentation on 2026-08-29. Things
change; the [check](#check-your-own-database) at the bottom takes ten
seconds.

| Provider | TimescaleDB | CrashCart runs as |
|---|---|---|
| [Tiger Cloud](https://www.tigerdata.com/) (formerly Timescale Cloud) | Community build | **TimescaleDB** |
| [Neon](https://neon.com/docs/extensions/timescaledb) | Apache-2 build | Plain Postgres |
| [Supabase](https://supabase.com/docs/guides/database/extensions/timescaledb) | Apache-2 build on Postgres 15 only; removed on Postgres 17 | Plain Postgres |
| [Azure Database for PostgreSQL](https://learn.microsoft.com/en-us/azure/postgresql/extensions/concepts-extensions-considerations#timescaledb) | Apache-2 build | Plain Postgres |
| [Crunchy Bridge](https://docs.crunchybridge.com/extensions-and-languages) | Apache-2 build | Plain Postgres |
| [Aiven](https://aiven.io/docs/products/postgresql/concepts/timescaledb) | Apache-2 build | Plain Postgres |
| [Render](https://render.com/docs/postgresql-extensions) | Apache-2 build | Plain Postgres |
| [PlanetScale for Postgres](https://planetscale.com/docs/postgres/extensions/timescaledb) | Apache-2 build | Plain Postgres |
| [DigitalOcean](https://docs.digitalocean.com/products/databases/postgresql/details/supported-extensions/) | Listed; build not documented, believed Apache-2 | Plain Postgres (verify) |
| [Scaleway](https://www.scaleway.com/en/docs/managed-databases-for-postgresql-and-mysql/reference-content/postgresql-extensions/) | Listed; build not documented | Unknown — verify |
| [AWS RDS / Aurora](https://docs.aws.amazon.com/AmazonRDS/latest/PostgreSQLReleaseNotes/postgresql-extensions.html) | Not available | Plain Postgres |
| [Google Cloud SQL / AlloyDB](https://docs.cloud.google.com/sql/docs/postgres/extensions) | Not available | Plain Postgres |
| [Fly.io Managed Postgres](https://fly.io/docs/mpg/extensions/) | Not available | Plain Postgres |
| [Heroku Postgres](https://devcenter.heroku.com/articles/heroku-postgres-extensions-postgis-full-text-search) | Not available | Plain Postgres |
| [Xata](https://xata.io/docs/platform/extensions) | Not available | Plain Postgres |
| [Railway](https://docs.railway.com/guides/postgresql) | Not in the default Postgres; their [TimescaleDB template](https://github.com/railwayapp-templates/timescale-postgis-ssl) runs the full image | TimescaleDB with the template |

Among hosted services, Tiger Cloud is the only one that ships the
Community build. Everywhere else, the full build means running the
`timescale/timescaledb` image yourself — [Docker Compose](/deploy/docker),
[Kubernetes](/deploy/kubernetes), a VM — or a template that does
(Railway above, Fly's unmanaged
[`postgres-flex-timescaledb`](https://fly.io/docs/postgres/getting-started/enabling-timescale/)
image).

## Check your own database

```sql
CREATE EXTENSION IF NOT EXISTS timescaledb;
SHOW timescaledb.license;
```

`timescale` → CrashCart will use TimescaleDB. `apache` (or the
`CREATE EXTENSION` fails) → plain Postgres. This is exactly the check
CrashCart runs at first start; the outcome is recorded in the database
and stays — see [Postgres options](/deploy/postgres).

Found a provider that changed, or one that's missing? The edit link at
the bottom of this page goes straight to the file.
