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
