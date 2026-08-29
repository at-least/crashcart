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
| Storage at volume | With [TimescaleDB](#timescaledb-and-compression): 5–10× compression, old data dropped for free. On managed Postgres: plain rows | No compression; retention deletes are billed writes |

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

## Three ways your data is stored {#timescaledb-and-compression}

"Go edition" really covers two setups, because CrashCart adapts to the
Postgres it finds. Nothing changes in how you use the product; what
changes is what storage and retention cost as volume grows.

| | Go + TimescaleDB | Go + plain Postgres | Serverless (D1) |
|---|---|---|---|
| Where | The `timescale/timescaledb` image ([Docker Compose](/deploy/docker), [Kubernetes](/deploy/kubernetes)), Tiger Cloud, self-managed | Managed Postgres: Neon, Supabase, RDS, Cloud SQL, Azure… ([provider list](/deploy/managed-postgres)) | Cloudflare |
| Disk per event | ~1/5–1/10 — daily chunks, compressed after 48 h | Full size | Full size, 10 GB per database |
| Expiring old data | Drops a whole day's chunk — instant, free | Deletes rows in batches, then vacuum | Deletes rows; each delete is a billed write |
| Overview & release-health stats | Continuous aggregates, pre-computed | Rolled-up tables, current hour computed live | Rolled-up tables |
| Comfortable up to | Tens of millions of events a month and beyond | A few million events a month | About a million events a month at $5; a few million with tuning |
| What you pay for as volume grows | Little — compressed disk | Disk and the database plan | D1 rows written |

Below a few million events a month the three feel the same. Above that,
Go + TimescaleDB is the one that stays cheap, which is why the
[Docker Compose](/deploy/docker) install ships it by default.

### TimescaleDB

**Benefits.** Events compress 5–10×, so 30 days of a busy app fits on a
small disk. Expiring old data drops a whole day's chunk — no deletes, no
vacuum, no load on ingest. Hourly and daily stats are continuous
aggregates, so the overview and release health stay fast however many
events are behind them. Cost is flat: a bigger app needs a bigger disk,
not a bigger plan.

**Limits.** You run Postgres — disk, backups, upgrades — or pay
[Tiger Cloud](https://www.tigerdata.com/), the only hosted service with
the Community build. Managed Postgres hosts don't offer it
([provider list](/deploy/managed-postgres)). It is one Postgres, so it
scales up, not out; in practice one machine goes a very long way.

**Fits.** Any volume, from a side project to tens of millions of events a
month. The only option for very high volume or retention beyond a month
at scale.

### Plain Postgres

**Benefits.** Any Postgres 16+ works — a free Neon or Supabase tier,
RDS, Cloud SQL, or `apt install postgresql`. With a managed host you get
backups, HA and scale-to-zero without running anything, and with
[Fly.io + Neon](/deploy/fly) the whole setup costs cents. Same product,
same viewer, same features.

**Limits.** Rows are full size — about 3 KB per event plus indexes — so
disk grows in step with traffic; Neon's free 0.5 GB is roughly 50–100k
events at 30 days' retention. Expiring old data means deleting rows in
batches and vacuuming, which competes with ingest as volume rises. Stats
for the current hour are computed live. Cost is driven by disk and the
database plan, which climbs faster than a compressed disk would.

**Fits.** Up to a few million events a month. Past that, export and
import into a TimescaleDB setup — one command each way.

### D1

**Benefits.** Nothing to run, at all. Free on the Workers Free plan for
Android and web apps, $5 a month flat on Workers Paid for most others.
Scheduled jobs run on cron triggers, cold starts are milliseconds, and
one Cloudflare account is the only dependency.

**Limits.** 10 GB per database and uncompressed rows — roughly three
million events at 30 days' retention with payloads inline. Every event is
about ten D1 rows written (indexes count), and expiring old data is
billed like inserting it, so cost rises with volume rather than
flattening. Large imports are split across requests because of the Worker
CPU limit, and iOS dSYM symbolication needs the paid plan.

**Fits.** Up to about a million events a month at $5; a few million with
shorter retention or payloads in R2. Beyond that, the Go edition.

Most managed Postgres hosts either don't offer TimescaleDB or ship the
Apache-2 build, which has no compression; CrashCart detects that
automatically and runs the plain-Postgres setup. Details and the
`TIMESCALE` setting: [Postgres options](/deploy/postgres).

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
