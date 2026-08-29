# Which database?

CrashCart is one Go binary and a Postgres. It adapts to the Postgres it
finds: with TimescaleDB it uses compression, chunk-based retention and
continuous aggregates; on plain Postgres it rolls stats up itself. Nothing
changes in how you use the product — what changes is what storage and
retention cost as volume grows.

| | TimescaleDB | Plain Postgres |
|---|---|---|
| Where | The `timescale/timescaledb` image ([Docker Compose](/deploy/docker), [Kubernetes](/deploy/kubernetes)), Tiger Cloud, self-managed | Managed Postgres: Neon, Supabase, RDS, Cloud SQL, Azure… ([provider list](/deploy/managed-postgres)) |
| You manage | A process and a database (or pay Tiger Cloud) | A process; the host runs the database |
| Disk per event | ~1/5–1/10 — daily chunks, compressed after 48 h | Full size |
| Expiring old data | Drops a whole day's chunk — instant, free | Deletes rows in batches, then vacuum |
| Overview & release-health stats | Continuous aggregates, pre-computed | Rolled-up tables, current hour computed live |
| Comfortable up to | Tens of millions of events a month and beyond | A few million events a month |
| What you pay for as volume grows | Little — compressed disk | Disk and the database plan |
| iOS / macOS symbolication | Optional sidecar container | Optional sidecar container |

Below a few million events a month the two feel the same. Above that,
TimescaleDB is the one that stays cheap, which is why the
[Docker Compose](/deploy/docker) install ships it by default.

## Pick plain Postgres if…

- **You don't want to run a database.** A free Neon or Supabase tier,
  RDS, Cloud SQL — backups, HA and scale-to-zero come with the host.
- **You want the cheapest possible start.** [Fly.io + Neon](/deploy/fly)
  scales to zero and costs cents to a few dollars a month for a small app.
- **Your app is small to medium.** Up to a few million events a month.

## Pick TimescaleDB if…

- **You have real volume** — many millions of events a month, or you want
  a long retention window.
- **You run your own server anyway.** `docker compose up` on a VPS gives
  you the compressed setup with nothing extra to configure.
- **You want retention to be free.** Expiring a day of data drops a chunk
  instead of deleting rows.

## TimescaleDB

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

## Plain Postgres

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

Most managed Postgres hosts either don't offer TimescaleDB or ship the
Apache-2 build, which has no compression; CrashCart detects that
automatically and runs the plain-Postgres setup. Details and the
`TIMESCALE` setting: [Postgres options](/deploy/postgres).

## Changing your mind later

Both setups read and write the same [export format](/reference/export-format).
Export from one, import into the other — see
[Moving to another database](/deploy/operations#moving-to-another-database).
