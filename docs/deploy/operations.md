# Operations

## Backups

```sh
crashcart export > backup-$(date +%F).ndjson       # everything
crashcart export shop-ios > shop-ios.ndjson         # one project
```

The file is plain newline-delimited JSON — payloads and symbol files
included — and restores into **any** CrashCart, on a different Postgres:

```sh
crashcart import < backup.ndjson
```

Importing is safe to repeat and safe on a live database: existing rows are
kept, missing ones added, projects created as needed.

Regular Postgres backups (`pg_dump`, volume snapshots) work too and are
faster for like-for-like restores; the export file is the portable one.

## Retention

- Raw events and sessions are deleted after `RETENTION_DAYS` (30): a
  week's partition is dropped once it is older than that.
- Symbol files are kept twice as long.
- Issues keep their status, counts and history after their events are gone.
- Timelines, sparklines and release health are kept for about a year.

Change `RETENTION_DAYS` and restart; it applies to existing data.

## Upgrading

Pull the new image or binary and restart. There are no migrations: the
schema carries a version, and a binary refuses to start against a
database of another version, saying so in the log. When that happens the
data moves by export / import:

```
crashcart export > backup.ndjson      # with the old binary, old DATABASE_URL
createdb crashcart_new                 # (or any empty database)
DATABASE_URL=… crashcart import < backup.ndjson   # with the new binary
```

then point `DATABASE_URL` at the new database and start. A full export
carries everything — projects, events, symbol files, alert settings, the
viewer accounts and the API keys (hashed, so existing keys keep working). A release note
says whether an upgrade changes the schema; most do not. Back up first
either way if you want to be able to roll back.

## Running without a long-lived server

`crashcart serve` does its housekeeping in the background. On platforms
that scale to zero, or if you prefer cron, run these one-shot commands on
a schedule instead:

```
crashcart retention     partitions, expired data, stats rollup  (every few minutes)
crashcart alerts        check for unhandled-error spikes  (every 10 minutes)
```

## Health check

`GET /health` returns `200` when CrashCart and its database are up, `503`
otherwise. Use it for container and load-balancer health checks.

## Metrics

`GET /metrics` serves Prometheus text; it takes an API key like the JSON
API (`Authorization: Bearer <key>` — in Prometheus, `authorization:
credentials`) and counts against `RATE_LIMIT`. Counters are per process
(sum them across replicas); the gauges read the database and are the same
everywhere.

| Metric | What to watch |
|---|---|
| `crashcart_ingest_envelopes_total{result}` | `quota` climbing: a project hit its daily quota; `unauthorized`: a wrong DSN; `error`: look at the log |
| `crashcart_ingest_events_total{outcome}` | `stored` vs `sampled` is the sampling ratio in effect; `duplicate` are resends |
| `crashcart_ingest_sessions_total` | release-health input volume |
| `crashcart_rate_limited_total{scope}` | 429s by `ingest`, `api`, `web`; steady growth on `ingest` means `RATE_LIMIT` is too low for a project |
| `crashcart_jobs_total{kind,outcome}`, `crashcart_jobs_pending`, `crashcart_jobs_dead` | `retry` / `dead` rising: the sidecar or a webhook is failing; a growing `pending` gauge means the workers do not keep up |
| `crashcart_symbolicate_events_total{outcome}` | `moved` is events regrouped after symbolication; `unresolved` means dSYMs are missing |
| `crashcart_alerts_total{type,kind,outcome}`, `crashcart_alerts_suppressed_total{type}` | `failed`: a channel is down; `suppressed`: alerts folded by the cooldown |
| `crashcart_stats_dirty_hours` | the rollup backlog; stays near the number of projects normally, grows after an import and drains at 500 hours/minute |
| `crashcart_rollup_hours_total{outcome}` | `expired` are dirty hours older than retention, cleared without recomputing |
| `crashcart_retention_partitions_dropped_total{table}`, `crashcart_retention_issues_expired_total{reason}` | what the hourly sweep removed |
| `crashcart_issues` | issue rows; the sampling story assumes this grows slowly |
| `crashcart_listener_reconnects_total` | LISTEN connection losses; steady growth points at a proxy or NAT dropping idle sockets |
| `crashcart_web_login_failures_total`, `crashcart_web_csrf_rejected_total` | password guessing; cross-site form posts |

## Rate limiting

Each DSN key and each API key may make `RATE_LIMIT` requests per minute
(600). Beyond that, requests get `429`; Sentry SDKs back off and resend
crashes later, so nothing is lost for a short burst.

## Moving to another database

Export, then import into the new database:

```sh
crashcart export > dump.ndjson
# point DATABASE_URL at the new database and start CrashCart once
crashcart import < dump.ndjson
```
