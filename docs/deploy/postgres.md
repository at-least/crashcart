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

By default every event is stored (a few hundred bytes of columns plus
the gzipped payload, typically 2–5 KB), so the database grows with the
traffic: roughly 1 GB per 300 000 events. When that is more than you want
to keep, lower the project's **sample rate** under
[Settings → Sampling](/guide/projects#sampling-and-daily-quota): the
first events of every issue are always stored (100 by default, 500 for
crashes), then only that fraction; the rest are counted but not stored.
From then on the database grows with the number of *issues*, not
events — a project with a thousand distinct issues keeps a few hundred
thousand events, a few hundred MB, whether it receives ten thousand
events a day or ten million.

Events and sessions are partitioned by week; CrashCart creates the
partitions ahead of time and drops the expired ones (`RETENTION_DAYS`).
The statistics behind the overview, sparklines and release health are
rollup tables it keeps current itself (every minute, on one replica).
Nothing runs inside the database on its own, so `pg_dump` / `pg_restore`
and major-version upgrades are plain Postgres.
