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

### On a schedule, with cron

Write to a temp name and `mv` into place, so a crash or a full disk mid-export
never leaves a truncated file where a good backup used to be; prune old
dumps so the directory doesn't grow forever. `%` in a crontab command must
be escaped as `\%` — unescaped, cron treats it as a newline. cron runs
`/bin/sh`, not bash, so the commands below stick to POSIX syntax (no
`{a,b}` brace expansion).

With Docker Compose, from the host's crontab (`crontab -e`):

```
0 3 * * * f=/var/backups/crashcart/$(date +\%F).ndjson; mkdir -p /var/backups/crashcart && docker compose -f /path/to/docker-compose.yml exec -T crashcart /crashcart export > "$f.tmp" && mv "$f.tmp" "$f" && find /var/backups/crashcart -name '*.ndjson' -mtime +7 -delete
```

With the binary + systemd install, as the `crashcart` user
(`sudo crontab -u crashcart -e`) — cron does not read `/etc/crashcart.env`
the way the systemd unit does, so pass it explicitly:

```
0 3 * * * f=/var/backups/crashcart/$(date +\%F).ndjson; mkdir -p /var/backups/crashcart && env $(cat /etc/crashcart.env | xargs) crashcart export > "$f.tmp" && mv "$f.tmp" "$f" && find /var/backups/crashcart -name '*.ndjson' -mtime +7 -delete
```

Either way, a failing cron job is only as loud as your mail setup — point
`MAILTO` in the crontab at an address you read, or wrap the command so a
non-zero exit reaches whatever you use for alerts.

## Retention

- Raw events and sessions are deleted after
  [`RETENTION_DAYS`](./configuration); deletion runs in bulk, so a row
  can outlive the cutoff by a little.
- Symbol files are kept longer than events, so late crashes of an old
  build still symbolicate.
- Issues keep their status, counts and history after their events are gone.
- Timelines, sparklines and release health are kept for much longer
  than the raw events.

Change `RETENTION_DAYS` and restart; it applies to existing data.

## Upgrading

Pull the new image or binary and restart: any pending schema migration
applies automatically before the server starts taking traffic (under a
lock, so restarting several replicas at once is safe — only one applies
it). A release note says whether an upgrade carries a migration; most
don't, and an upgrade with none is exactly pull-and-restart.

Downgrading a binary against an already-migrated database is refused at
startup rather than silently running against a schema it doesn't know —
back up first (`crashcart export > backup.ndjson`) if you want a way
back. Export / import remain how you move a database between
environments or restore a backup; they carry everything — projects,
events, symbol files, alert settings, the viewer accounts and the API
keys (hashed, so existing keys keep working).

## Running without a long-lived server

`crashcart serve` does its housekeeping in the background. On platforms
that scale to zero, or if you prefer cron, run these one-shot commands on
a schedule instead:

```
crashcart retention     partitions, expired data, stats rollup  (every few minutes)
crashcart alerts        check for unhandled-error spikes  (as often as you would set ALERT_INTERVAL)
```

## Health check

`GET /health` returns `200` when CrashCart and its database are up, `503`
otherwise. Use it for container and load-balancer health checks.

## Rate limiting

Each DSN key and each API key may make [`RATE_LIMIT`](./configuration)
requests per minute. Beyond that, requests get `429`; Sentry SDKs back
off and resend crashes later, so nothing is lost for a short burst.

## Moving to another database

Export, then import into the new database:

```sh
crashcart export > dump.ndjson
# point DATABASE_URL at the new database and start CrashCart once
crashcart import < dump.ndjson
```
