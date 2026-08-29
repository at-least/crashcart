# Operations

## Retention and compression

- **Raw data** (`events`, `sessions`) is dropped after `RETENTION_DAYS`.
  With TimescaleDB whole chunks are dropped; on plain Postgres rows are
  deleted in 5000-row batches.
- **Issues** are never dropped by retention; they keep their status and
  counts after their events are gone.
- **Hourly / daily statistics** (timelines, sparklines, release health) are
  kept for 400 days.
- **Compression** (TimescaleDB only) applies to chunks older than
  `COMPRESS_AFTER`. Compressed chunks are read-only for symbolication, which
  is why a symbol upload only re-symbolicates events newer than that.

Both policies are reconciled from the environment at startup and on
`crashcart retention`.

## Backups

```sh
crashcart export > backup-$(date +%F).ndjson       # all projects
crashcart export shop-ios > shop-ios.ndjson         # one project
```

The dump is the versioned [export format](/reference/export-format): every
table as newline-delimited JSON, projects referenced by slug, so it restores
into **any** CrashCart database — a different Postgres, the plain or
TimescaleDB variant, or the serverless edition.

```sh
crashcart import < backup.ndjson
```

Import upserts: events and sessions `ON CONFLICT DO NOTHING`, issues and
symbol files on their natural key, alert channels only when no identical one
exists. Importing twice, or onto a live database, is safe. Unknown project
slugs are created. Aggregates are not in the dump; they recompute.

`pg_dump` / volume snapshots work as well and are faster to restore
like-for-like; the NDJSON dump is the portable one.

## Jobs

Symbolication and alert delivery are rows in the `jobs` table, claimed with
`SELECT … FOR UPDATE SKIP LOCKED` by the `WORKERS` goroutines of every
running `crashcart serve`. A job retries with backoff and is dropped after
8 attempts. Job kinds: `symbolicate {event}`, `resymbolicate {release}`,
`alert {type, fingerprint}`.

## Cron-style operation

`crashcart serve` runs the schedulers in-process. If you prefer external
scheduling — or run a scale-to-zero deployment — these one-shot commands
each do one pass and exit:

```
crashcart retention     reconcile policies, run one sweep (and roll up stats on plain Postgres)
crashcart alerts        run one crash-spike check
```

## Rate limiting

`RATE_LIMIT` requests per minute per credential — the DSN key for ingest,
the API key for the API — in 60-second fixed windows stored in Postgres, so
it holds across replicas. Exceeding it returns `429`. Sentry SDKs back off
on `429` and resend cached crashes later.

## Health

`GET /health` returns `200` when the process is up and the database
answers. Use it for load-balancer and container health checks.

## Scaling

- The binary is **stateless**; run replicas behind a load balancer.
- Ingest is one transaction per envelope; no hot rows (aggregates are not
  touched at ingest).
- Per-event write cost: one insert for a message; one insert, one issue
  upsert and at most one job row for an exception.
- Storage is bounded per issue by [sampling](/guide/projects#sampling-and-quota)
  and per project by `daily_quota`.
- TimescaleDB starts to matter at tens of millions of events a month; see
  [TimescaleDB or plain Postgres](./postgres#timescaledb-or-plain-postgres).

## Moving between databases or editions

There is no in-place conversion between the TimescaleDB and plain
variants, and the serverless edition uses D1. In all cases the path is the
same:

```sh
crashcart export > dump.ndjson
# migrate the new database (start crashcart once, or `crashcart migrate`)
crashcart import < dump.ndjson
```

For the serverless edition, `POST /api/import` on the Worker (or its
batching script for large dumps) and `GET /api/export` in the other
direction.
