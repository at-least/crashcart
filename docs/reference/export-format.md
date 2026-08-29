# CrashCart export format (NDJSON, format 1)

This is the interchange contract between every CrashCart implementation
(the Go/TimescaleDB server in this repo, the serverless port, and any future
one). An implementation is compatible when it can **write** a file that this
document describes and **read** any such file, regardless of which
implementation produced it. Implementations share this document, not code.

The Go reference implementation is `internal/export/export.go`; its
`TestRoundTrip` is the reference behaviour when this text is ambiguous.
Change this document *before* changing either.

## Container

- UTF-8, newline-delimited JSON: exactly one JSON object per line, `\n`
  terminated. Blank lines are ignored.
- Every object has a string field `"t"` naming its kind. The first line is
  `_meta`; the rest are table rows.
- A reader must accept lines up to 16 MiB. Writers must not emit longer ones
  (an event payload is at most a 20 MB envelope, so this fits).
- Writers must not HTML-escape JSON (`<`, `>`, `&` are written verbatim).

## Order

```
_meta
projects*            (all projects, sorted by slug)
issues*              (per project in slug order; within a project by fingerprint)
events*              (per project; by id ascending)
sessions*            (per project; by id ascending)
symbol_files*        (per project; by kind, release, filename)
alert_rules*         (per project; by type)
alert_channels*      (per project; by insertion order)
```

Readers may rely on **projects appearing before any row that references
them** and nothing else about ordering. Writers must keep the table order
above; the within-table sort is a determinism convenience, not a contract.

## Scalar encodings

| Column type | JSON |
|---|---|
| timestamp (`TIMESTAMPTZ`) | integer, unix **milliseconds** UTC |
| time-series id (`events.id`, `sessions.id`) | integer, `unix_ms × 1000 + 0..999` (see *Ids*) |
| counts / sizes | integer |
| JSON / JSONB column | embedded JSON value (object or array), never a string |
| bytes (`BYTEA`) | base64 string (standard alphabet, with padding) |
| `NULL` | field **omitted**; readers also accept an explicit `null` |
| boolean | `true` / `false` |

## Ids

Rows never carry a database identity id. A row refers to its project by
`"project": "<slug>"`. Projects, symbol files and alert channels are
identified by their natural keys (below), so a dump loads into any database.

`events.id` and `sessions.id` are exported and kept on import. They encode
the event time (`ms × 1000 + random 0..999`) and are the primary key, the
time dimension, and the dedupe key. They stay below 2^53 until year 2255, so
JavaScript `number` is safe.

## `_meta`

```json
{"t":"_meta","format":1,"exported_at":1756450000000,"app":"crashcart"}
```

- `format` — integer. A reader that supports format *N* must refuse a file
  with `format > N` and must read `format ≤ N`.
- `exported_at` — unix ms.
- `app` — free-form producer name (`"crashcart"`, `"crashcart-serverless"`,
  …). Readers must not branch on it.

## Rows

Required fields are those without `?`. `?` fields may be omitted (NULL).
Types: `str`, `int`, `float`, `bool`, `json`, `b64`, `ms` (unix ms),
`id` (time-series id).

### `projects`

```
slug              str    natural key
name              str
platform?         str    one of: ios android flutter react-native web backend other
public_key        str    DSN public key; unique per database
sample_keep_first int    default 100
sample_rate       float  0 < x ≤ 1; default 1
daily_quota?      int    default 100000
created_at        ms
```

Import: upsert on `slug`; all listed columns are replaced. Missing
`public_key` → a fresh random 32-hex key is generated. `sample_rate ≤ 0` →
1. A project slug that first appears on a non-`projects` row is created with
`name = slug` and a fresh key.

### `issues`

```
project           str    slug
fingerprint       str    natural key with project
title             str
level             str    fatal error warning info debug
error_type?       str
screen?           str
platform?         str
status            str    unresolved triaged resolved ignored regression; default unresolved
event_count       int    events seen (including sampled-out)
stored_count      int    events actually stored
first_seen        ms
last_seen         ms
first_release?    str
last_release?     str
resolved_release? str
created_at        ms
updated_at        ms
```

Import: upsert on `(project, fingerprint)`. **Counts are replaced, not
added.** `created_at`/`updated_at` of `0` or missing → now.

### `events`

```
project           str    slug
id                id     required, ≠ 0
event_id          str    Sentry event_id (32 hex)
level             str    fatal error warning info debug
message           str
platform?         str
environment?      str
release?          str
device_id?        str
device_model?     str
os_version?       str
screen?           str
error_type?       str
error_location?   str
handled?          bool
sdk_name?         str
user_id?          str
fingerprint?      str    references issues.fingerprint in the same project
symbolicated      bool
tags              json   object; default {}
breadcrumbs       json   array;  default []
payload           json   required; the raw Sentry event object, never rewritten
symbols?          json   symbolicated frames (only when symbolicated)
```

Import: insert; conflict on `id` → skip (`ON CONFLICT DO NOTHING`).
`tags`/`breadcrumbs` missing or `null` → defaults. A row without `id` or
`payload` is an error.

### `sessions`

```
project           str    slug
id                id
release           str
environment?      str
status            str    ok exited errored crashed abnormal
count             int    ≥ 1 (aggregated session count); default 1
```

Import: insert; conflict on `id` → skip.

### `symbol_files`

```
project           str    slug
kind              str    proguard sourcemap dsym
release           str
debug_id?         str
filename          str
size              int    bytes; 0 → derived from data
data              b64
uploaded_at       ms
```

Import: upsert on `(project, kind, release, filename)`; `debug_id`, `size`,
`data`, `uploaded_at` replaced.

### `alert_rules`

```
project           str    slug
type              str    new_issue regression crash_spike
enabled           bool
cooldown_minutes  int
last_triggered?   ms
```

Import: upsert on `(project, type)`.

### `alert_channels`

```
project           str    slug
kind              str    webhook telegram
config            json   object; kind-specific; default {}
created_at        ms
```

Import: insert only when no row with identical `(project, kind, config)`
exists (JSON equality, not string equality).

## Not exported

Aggregates (`event_stats_hourly`, `issue_stats_hourly`,
`release_health_daily` or their equivalents), the job queue, upload chunks
and rate-limit buckets. Aggregates must be recomputable from `events` and
`sessions`; the rest expire.

## Reader rules

1. Refuse `format` greater than what you support; otherwise proceed.
2. A line whose `t` you do not know is counted as `skipped` and ignored —
   newer exports load on older readers.
3. Import is idempotent: importing the same file twice, or onto a live
   database, changes nothing the second time (except `alert_channels`
   ordering and any `created_at` filled with *now* on rows that omitted it).
4. Report per-table row counts on completion (`{"rows":{"events":123,…}}`).
5. Fail fast on the first malformed line, reporting its 1-based line number.

## Evolving the format

- **Additive change** (new optional field, new table): keep `format` at 1.
  Old readers ignore unknown fields and unknown `t`.
- **Breaking change** (renamed/removed required field, changed encoding):
  bump `format`, keep reading the previous one for at least one release.
- Update this file, then the Go implementation and its `TestRoundTrip`,
  then the other implementations.

## Compatibility testing

Each implementation keeps a round-trip test (write → read → compare) and
should also import a fixture produced by *another* implementation. Producing
a fixture: `crashcart seed && crashcart export > export-v1.ndjson`.
