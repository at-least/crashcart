# The database

CrashCart needs **Postgres 14 or newer**, any build, and nothing else:
projects, issues, events with their raw payloads (gzipped), sessions,
statistics, symbol files and users all live in the one database. No
extensions, nothing to install in it, one thing to back up.

| | |
|---|---|
| Self-hosted | The `postgres` image (what Docker Compose and the Kubernetes manifests run), or your distro's package |
| Managed | Anything: RDS, Cloud SQL, Azure, Neon, Supabase, Aiven, Render, … |

The database user needs to create tables in its database. Take the
connection URL as `DATABASE_URL`:

```
DATABASE_URL=postgres://user:pass@host/crashcart?sslmode=require
```

## How big it gets

Not with the number of events — with the number of *issues*. Per-issue
[sampling](/guide/projects#sampling-and-daily-quota) stores the first
events of every issue (100 by default, 500 for crashes) and then a small
fraction (1 %); the rest are counted but not stored. A project with a
thousand distinct issues keeps on the order of a few hundred thousand
events, a few hundred MB with payloads — whether it receives ten thousand
events a day or ten million. Set `sample_rate` to 1 on a project to store
everything; then the database grows with the traffic.

Events and sessions are partitioned by week; CrashCart creates the
partitions ahead of time and drops the expired ones (`RETENTION_DAYS`).
The statistics behind the overview, sparklines and release health are
rollup tables it keeps current itself (every minute, on one replica).
Nothing runs inside the database on its own, so `pg_dump` / `pg_restore`
and major-version upgrades are plain Postgres.
