# CLI

The `crashcart` binary is both the server and the admin tool. Every
subcommand reads [`DATABASE_URL`](/deploy/configuration) and the other
environment variables; most connect to the database and exit.

```
crashcart serve                              HTTP server + job worker + schedulers (default)
crashcart init                               create the schema and exit
crashcart retention                          reconcile policies and run one sweep
crashcart alerts                             run one crash-spike check
crashcart seed [slug]                        write a week of demo data (default project "demo")
crashcart export [slug]                      stream NDJSON to stdout (all projects, or one)
crashcart import                             load NDJSON from stdin (idempotent)
crashcart project <slug> <name> [platform]   create a project and print its DSN
crashcart rotate-key <slug>                  replace the project's DSN key and print the new DSN
crashcart user add <email> [name]            create a viewer account (password: CRASHCART_PASSWORD or prompt)
crashcart user passwd <email>                change an account's password
crashcart apikey create <name>               create an API key; prints the secret once
crashcart apikey list                        list API keys with last use
crashcart apikey revoke <id>                 revoke an API key
```

With Docker Compose, prefix with `docker compose exec crashcart /crashcart`.

## `serve`

Runs everything in one process: HTTP on `LISTEN_ADDR`, `WORKERS` job
goroutines, the retention scheduler and the crash-spike scheduler
(`ALERT_INTERVAL`). Migrations run first. This is the default when no
subcommand is given.

## `init`

Creates the schema in an empty database and exits (every command does
this on start; `init` is for a deploy pipeline step, or to prepare a
database before `import`). Fails when the database has no TimescaleDB
Community build (see [The database](/deploy/postgres)).

## `retention`

Reconciles the compression and retention policies with the current
`COMPRESS_AFTER` / `RETENTION_DAYS`, runs one retention sweep, and on plain
Postgres rolls up the hourly statistics. Exits when done — for cron.

## `alerts`

Evaluates the crash-spike rule once for every project and queues alerts.
Exits when done — for cron.

## `seed [slug]`

Writes a week of realistic demo data (issues, events, sessions across a few
releases) into the project `slug`, creating it if needed. Default `demo`.

## `export [slug]`

Writes everything to stdout as NDJSON in the
[export format](./export-format): all projects, or just `slug`. Per-table
row counts go to stderr.

```sh
crashcart export > backup.ndjson
crashcart export shop-ios | gzip > shop-ios.ndjson.gz
```

## `import`

Reads NDJSON from stdin and upserts. Safe to run twice or against a live
database; unknown project slugs are created; lines with an unknown `"t"`
are counted as `skipped`. Counts go to stderr.

```sh
crashcart import < backup.ndjson
```

## `project <slug> <name> [platform]`

Creates a project and prints its id and DSN (using `PUBLIC_URL` when set).
`platform`, if given, must be one of the SDK families: `ios`, `android`,
`flutter`, `react-native`, `web`, `backend`, `other`.

## `rotate-key <slug>`

Generates a new DSN key. The old one is rejected immediately.

## Exit status

`0` on success. Any error is logged and exits `1`.
