# CLI

The `crashcart` binary is both the server and the admin tool. Every
subcommand reads [`DATABASE_URL`](/deploy/configuration) and the other
environment variables; most connect to the database and exit.

```
crashcart serve                             HTTP server + job worker + schedulers (default)
crashcart init                              create the schema and exit
crashcart retention                         create partitions, run one sweep and roll the stats up
crashcart alerts                            run one unhandled-spike check
crashcart seed [slug]                       write a week of demo data (default project "demo")
crashcart export [slug]                     stream NDJSON to stdout (all projects, or one)
crashcart import                            load NDJSON from stdin (idempotent)
crashcart project <slug> <name> [platform]  create a project and print its DSN
crashcart rotate-key <slug>                 issue a new DSN key (the old one keeps working until deleted)
crashcart project-keys list <slug>          list a project's retired-but-still-valid DSN keys
crashcart project-keys delete <slug> <id>   delete a retired DSN key (stops it within the ingest cache TTL)
crashcart user add <email> [name]           create a viewer account (password from CRASHCART_PASSWORD, else prompted)
crashcart user passwd <email>               set a viewer account's password (same source)
crashcart apikey create <name>              create an API key and print its secret (shown once)
crashcart apikey list                       list API keys with their state and last use
crashcart apikey revoke <id>                revoke an API key
crashcart symbolicate                       dSYM symbolication sidecar (needs llvm-symbolizer, no database)
crashcart version                           print the version and exit
```

With Docker Compose, prefix with `docker compose exec crashcart /crashcart`.

## `serve`

Runs everything in one process: HTTP on `LISTEN_ADDR`, `WORKERS` job
goroutines, and the schedulers — the stats rollup and the ignored-issue
check every minute, the retention sweep hourly, the unhandled-spike check
every `ALERT_INTERVAL` (each on one replica at a time). The schema is
created first. This is the default when no subcommand is given.

## `init`

Creates the schema in an empty database and exits (every command does this
on start; `init` is for a deploy pipeline step, or to prepare a database
before `import`). On a database that already has a schema it checks the
schema version and exits non-zero on a mismatch (see
[Upgrading](/deploy/operations#upgrading)).

## `retention`

Creates the coming weeks' partitions, runs one retention sweep and rolls the
statistics up. Exits when done — for cron.

## `alerts`

Evaluates the unhandled-spike rule once for every project and queues alerts.
Exits when done — for cron.

## `seed [slug]`

Writes a week of realistic demo data (issues, events, sessions across a few
releases) into the project `slug`, creating it if needed. Default `demo`.

## `export [slug]`

Writes everything to stdout as NDJSON in the [export
format](./export-format): all projects, or just `slug`.

```sh
crashcart export > backup.ndjson
crashcart export shop-ios | gzip > shop-ios.ndjson.gz
```

## `import`

Reads NDJSON from stdin and upserts. Safe to run twice or against a live
database; unknown project slugs are created; lines with an unknown `"t"` are
counted as `skipped`. The report (rows per table, lines committed) goes to
stderr as JSON.

```sh
crashcart import < backup.ndjson
```

## `project <slug> <name> [platform]`

Creates a project and prints its id and DSN (using `PUBLIC_URL` when set).
`platform`, if given, must be one of the SDK families listed under
[Glossary](./glossary#core-concepts).

## `rotate-key <slug>`

Generates a new current DSN key and prints the new DSN. The old key is not
invalidated — it keeps authenticating, listed under `project-keys list`,
until explicitly deleted with `project-keys delete`.

## `symbolicate`

Runs the dSYM symbolication sidecar: HTTP on `LISTEN_ADDR`,
`llvm-symbolizer` from `PATH`, a disk cache of the dSYMs it has used
(`SYMBOLICATE_CACHE_DIR`, `SYMBOLICATE_CACHE_MAX_MB`). The only subcommand
that does not read `DATABASE_URL`; the server reaches it through
`SYMBOLICATE_URL`. See [Symbolication](/guide/symbolication#ios-macos).

## Exit status

`0` on success. Any error is logged and exits `1`.
