# The database

CrashCart needs **Postgres 15 or newer**, any build, and nothing else:
projects, issues, events with their raw payloads (gzipped), sessions,
statistics, symbol files and users all live in the one database. No
extensions, nothing to install in it, one thing to back up. (The big
bytes — symbol files and raw payloads — can go to an S3-compatible bucket
instead: [Payloads in a bucket](#payloads-in-a-bucket).)

| | |
|---|---|
| Self-hosted | The `postgres` image (what Docker Compose and the Kubernetes manifests run), or your distro's package |
| Managed | Anything: RDS, Cloud SQL, Azure, Neon, Supabase, Aiven, Render, … |

The database user needs to create tables in its database. Take the
connection URL as `DATABASE_URL`:

```
DATABASE_URL=postgres://user:pass@host/crashcart?sslmode=require
```

## Payloads in a bucket

Raw event payloads are most of the database: 30,000 events with 30 KB
payloads are about 1 GB with them and 20 MB without. Everything that
scales with database size — backups, restores, replication lag, the
export file — scales with those bytes. `BLOB_STORE=s3` keeps them (and
uploaded symbol files) in an S3-compatible bucket instead — AWS, MinIO,
Cloudflare R2, Backblaze — and the database to metadata:

```
BLOB_STORE=s3
S3_BUCKET=crashcart
S3_ENDPOINT=https://<account>.r2.cloudflarestorage.com   # empty for AWS
S3_ACCESS_KEY=…  S3_SECRET_KEY=…                          # or the AWS credential chain
```

Ingest does not slow down and does not depend on the bucket: a payload
is written to Postgres in the same transaction as its event and packed
into the bucket in the background, in ~8 MB objects per project and week
(one request per object, which is what keeps an object-store bill
small); a bucket outage only means a growing spool. Reading an event
fetches its bytes from its pack. A week's packs are deleted when its
events expire, and a project's when the project is.

Switching is safe at any time, in either direction: each event and
symbol file is read from wherever it was written, so rows from before
the change stay put and new ones go to the bucket. `crashcart export`
carries the bytes whichever way they are held, and `import` writes them
the destination's way — that is also how you move an existing instance
over. A Postgres backup alone is then metadata: pair it with the bucket
(its own versioning or replication), or use the export. All variables:
[Configuration](./configuration).

## How big it gets

By default every event is stored (a few hundred bytes of columns plus
the gzipped payload, typically 2–5 KB), so the database grows with the
traffic: as a rule of thumb, roughly 1 GB per 300 000 events. When that
is more than you want to keep, lower the project's **sample rate** under
[Settings → Sampling](/guide/projects#sampling-and-daily-quota): the
first events of every issue are always stored, then only that fraction;
the rest are counted but not stored. From then on the database grows
with the number of *issues*, not events — a project with a thousand
distinct issues keeps a bounded number of events, a few hundred MB,
whether it receives ten thousand events a day or ten million.

CrashCart does its own housekeeping — expiring data past
[`RETENTION_DAYS`](./configuration) and keeping the statistics behind
the overview, sparklines and release health current. Nothing runs
inside the database on its own, so `pg_dump` / `pg_restore` and
major-version upgrades are plain Postgres.
