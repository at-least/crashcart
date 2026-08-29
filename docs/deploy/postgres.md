# The database and the object store

CrashCart keeps two kinds of data in two places:

- **Postgres 14 or newer**, any build: projects, issues, the event
  columns the viewer filters on, sessions, statistics, users. No
  extensions, nothing to install in the database.
- **An S3-compatible bucket**: the raw event payloads (the crash as the
  SDK sent it: stack, breadcrumbs, contexts), symbol files (ProGuard
  mappings, source maps, dSYMs) and sentry-cli upload chunks.

The database stays small — a few hundred bytes per event — so backups,
`VACUUM` and upgrades stay quick; the bulk of the bytes sits in storage
that is cheap and, with a lifecycle rule, expires by itself.

## Postgres

| | |
|---|---|
| Self-hosted | The `postgres` image (what Docker Compose and the Kubernetes manifests run), or your distro's package |
| Managed | Anything: RDS, Cloud SQL, Azure, Neon, Supabase, Aiven, Render, … |

The database user needs to create tables in its database. Take the
connection URL as `DATABASE_URL`:

```
DATABASE_URL=postgres://user:pass@host/crashcart?sslmode=require
```

Events and sessions are partitioned by week; CrashCart creates the
partitions ahead of time and drops the expired ones (`RETENTION_DAYS`).
The statistics behind the overview, sparklines and release health are
rollup tables it keeps current itself (every minute, on one replica).
Nothing runs inside the database on its own, so `pg_dump` / `pg_restore`
and major-version upgrades are plain Postgres.

## The bucket

| | |
|---|---|
| Self-hosted | [MinIO](https://min.io) (bundled in the compose file), Garage, SeaweedFS, … |
| Managed | AWS S3, Cloudflare R2, Backblaze B2, DigitalOcean Spaces, Hetzner, … — anything S3-compatible |

Give CrashCart a **bucket of its own**: at startup it creates the bucket
if it can and replaces the bucket's lifecycle configuration with its own
rules (`events/` expire `RETENTION_DAYS` + 7 days after they were written,
`symbols/` after twice `RETENTION_DAYS`, `chunks/` after a day). When the
credentials may not set lifecycle rules, the startup log prints the rules
to create by hand.

```
S3_ENDPOINT=https://<account>.r2.cloudflarestorage.com   # empty for AWS S3
S3_REGION=auto                                           # AWS: the bucket's region
S3_BUCKET=crashcart
S3_ACCESS_KEY=…
S3_SECRET_KEY=…
```

Payloads are gzipped and written to Postgres with the event (a small
spool table), then moved to the bucket in batches — packed into one
object per 8 MB of compressed payloads (`events/<day>/<pack>`; the event row
records where its payload sits) — so the bucket sees one PUT per batch
rather than per event and request charges stay negligible at any volume.
Nothing is lost if the bucket is unreachable: payloads wait in the spool
(readable from there meanwhile) until it is back. Symbol files are one
object each (`symbols/<project>/<id>`). An object that outlives its rows
just expires.
