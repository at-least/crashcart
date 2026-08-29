# Which edition?

CrashCart comes in two editions. Same SDK setup, same viewer, same alerts,
same export format — the difference is where it runs and what that costs.

| | Go edition | [Serverless edition](https://github.com/crashcartapp/crashcart-serverless) |
|---|---|---|
| Runs on | Any server with Postgres — Docker, a binary, Kubernetes, Fly.io | Cloudflare Workers, D1 and R2 |
| You manage | A process and a database | Nothing — no servers, no database |
| Price | The server and the Postgres you pick | Free plan for small apps; Workers Paid ($5/month) covers roughly a million events a month |
| Scales to | Tens of millions of events a month and beyond | A few million events a month (see [Limits](#serverless-limits)) |
| iOS / macOS symbolication | Optional sidecar container | Needs Workers Paid |
| Storage at volume | [TimescaleDB](#timescaledb-and-compression) compresses events 5–10× and drops old data for free | No compression; retention deletes are billed writes |

## Pick the serverless edition if…

- **You don't want to run anything.** No VM, no Postgres, no image to
  update, no scale-to-zero tuning. Deploy from your laptop, done.
- **You want a predictable bill.** $0 on the Free plan for an Android or
  web app; $5/month flat on Workers Paid, which is enough for most apps
  with real users. No compute-hours to watch, no surprise invoices.
- **Your app is small to medium.** Up to roughly a million events a month
  fits comfortably in the $5 plan.

This is the default recommendation for a new project.

## Pick the Go edition if…

- **You already run Postgres**, or want the data in your own cloud, network
  or datacenter.
- **You have real volume** — many millions of events a month, or you want
  a long retention window. Postgres has no per-row write price and no
  10 GB ceiling, and with TimescaleDB storage and retention get cheap
  (see [below](#timescaledb-and-compression)).
- **You want iOS symbolication on a free tier.** The dSYM sidecar runs
  next to the server; on Cloudflare it needs the paid plan.
- **You want to run it on a laptop or in CI.** One binary, one
  `docker compose up`.

Prefer zero-ops but still want Postgres? [Fly.io + Neon](/deploy/fly)
scales to zero and costs cents to a few dollars a month for a small app.
Note that Neon's free plan holds 0.5 GB, and crash traffic tends to keep
the database awake, so past a few tens of thousands of events a month a
paid Neon plan usually costs more than the serverless edition's $5.

## TimescaleDB and compression

The Go edition works on any Postgres 16+. If the
[TimescaleDB](https://www.timescale.com/) extension is present, CrashCart
uses it automatically; nothing changes in how you use the product. What
it changes underneath:

- **Compression.** Events and sessions are stored in daily chunks;
  chunks older than 48 hours are compressed, and crash payloads compress
  well — expect 5–10× less disk than plain Postgres.
- **Retention drops chunks.** Expiring old data means dropping a whole
  day's chunk, not deleting rows one by one. No vacuum, no write
  amplification, no cost.
- **Stats are pre-computed.** Hourly and daily counts are continuous
  aggregates, so the overview and release-health pages stay fast at tens
  of millions of events.

TimescaleDB comes with the `timescale/timescaledb` image (what
[Docker Compose](/deploy/docker) uses), Timescale Cloud, or a
self-managed Postgres. Managed Postgres — Neon, Supabase, RDS, Cloud SQL
— ships either nothing or the Apache-2 build without compression
([provider list](/deploy/managed-postgres)), and CrashCart falls back to
plain Postgres:
rolled-up stats tables, batched deletes, no compression. Below a few
million events a month you won't notice. Details and the `TIMESCALE`
setting: [Postgres options](/deploy/postgres).

The serverless edition has no equivalent. D1 stores rows uncompressed,
and retention is a `DELETE` that is metered like an insert — which is
why its price rises with volume while the Go edition's stays flat.

## Serverless limits

Worth knowing before you pick it for a busier app:

- **Storage.** D1 caps a database at 10 GB. With inline payloads and the
  default 30-day retention that is roughly three million events. Shorter
  retention or pushing payloads to R2 (`PAYLOAD_INLINE_MAX`) raises the
  ceiling.
- **Writes are metered.** Beyond the plan's included rows, D1 bills per
  million rows written; each stored event is about ten rows (indexes and
  retention deletes count). Above a few million events a month the Go
  edition is cheaper.
- **Large imports** are split into several requests because a single
  Worker invocation has a CPU time limit.

## Changing your mind later

Both editions read and write the same [export format](/reference/export-format).
Export from one, import into the other — see
[Moving data between editions](/deploy/serverless#moving-data-between-editions).
