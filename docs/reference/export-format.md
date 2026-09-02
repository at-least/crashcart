# CrashCart export format (NDJSON, format 3)

The file `crashcart export` writes and `crashcart import` reads: a full,
portable copy of one or all projects for backups and for moving between
databases. The Go implementation is
`internal/export/export.go`; its `TestRoundTrip` is the reference behaviour
when this text is ambiguous. Change this document *before* changing the
code.

## Container

- UTF-8, newline-delimited JSON: exactly one JSON object per line, `\n`
  terminated. Blank lines are ignored.
- Every object has a string field `"t"` naming its kind. The first line is
  `_meta`; the rest are table rows.
- A reader must accept lines up to 96 MiB. Writers must not emit longer
  ones (a symbol file is at most 50 MB, ~67 MB as base64 on one line; an
  event payload is at most a 20 MB envelope).
- Writers must not HTML-escape JSON (`<`, `>`, `&` are written verbatim).

## Order

```
_meta
users*               (full exports only; sorted by email)
api_keys*            (full exports only; by id)
projects*            (all projects, sorted by slug)
releases*            (per project in slug order; within a project by release)
issues*              (per project in slug order; within a project by fingerprint)
events*              (per project; by occurred_at, event_id ascending)
attachments*         (per project; by occurred_at, event_id, n ascending)
user_reports*        (per project; by event_id ascending)
sessions*            (per project; by started_at, sid ascending)
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
| timestamp (`TIMESTAMPTZ`) | string, RFC 3339 in UTC with fractional seconds as needed (`2026-08-20T09:59:00.000123Z`); readers accept any RFC 3339 offset |
| counts / sizes | integer |
| JSON / JSONB column | embedded JSON value (object or array), never a string |
| bytes (`BYTEA`) | base64 string (standard alphabet, with padding) |
| `NULL` | field **omitted**; readers also accept an explicit `null` |
| boolean | `true` / `false` |

## Keys

Rows never carry a database identity id. A row refers to its project by
`"project": "<slug>"`. Every table is identified by its natural key (below),
so a dump loads into any database:

- events: `(project, event_id, occurred_at)` — the SDK's event id and timestamp
- attachments: `(project, event_id, occurred_at, n)` — the event's key plus the file's position
- user_reports: `(project, event_id)` — no `occurred_at`: unlike attachments, a report is not tied to a stored event's row
- sessions: `(project, sid, started_at)` — the SDK's session id (aggregate rows carry a generated `agg-…` sid)
- releases: `(project, release)`
- issues: `(project, fingerprint)`
- projects: `slug`; symbol files: `(project, kind, release, filename)`;
  alert rules: `(project, type)`; alert channels: `(project, kind, config)`
- users: `email`; api keys: `key_hash` (global, not per project)

## `_meta`

```json
{"t":"_meta","format":3,"exported_at":"2026-08-29T10:00:00Z","app":"crashcart"}
```

- `format` — integer. A reader that supports format *N* must refuse a file
  with `format > N` and must read `format ≤ N`.
- `exported_at` — timestamp.
- `app` — free-form producer name. Readers must not branch on it.

## Rows

Required fields are those without `?`. `?` fields may be omitted (NULL).
Types: `str`, `int`, `float`, `bool`, `json`, `b64`, `ts` (timestamp).

### `users`

```
email             str    natural key, lowercased
name              str
password_hash     str    bcrypt
created_at        ts
```

Import: insert; an existing user with that email is left untouched
(password included). Written by full exports only (`crashcart export`
without a project), so an instance moved with export / import keeps its
accounts.

### `api_keys`

```
name              str
key_hash          b64    sha256 of the secret; natural key
prefix            str    the secret's first characters, for display
created_by?       str    user email
created_at        ts
last_used_at?     ts
revoked_at?       ts
```

Import: insert; an existing key with that hash is left untouched.
`created_by` resolves to the user by email (NULL when unknown). Full
exports only. The secret itself is never in the file — only its hash — so
existing keys keep working after a move and no new secret is created.

### `projects`

```
slug              str    natural key
name              str
platform?         str    one of: ios android flutter react-native web backend other
public_key        str    DSN public key; unique per database
sample_keep_first int    default 100
sample_rate       float  0 < x ≤ 1; default 1
daily_quota?      int    default 0 (unlimited)
created_at        ts
```

Import: upsert on `slug`; all listed columns are replaced. Missing
`public_key` → a fresh random 32-hex key is generated. `sample_rate ≤ 0` →
1. A project slug that first appears on a non-`projects` row is created with
`name = slug` and a fresh key.

### `releases`

```
project           str    slug
release           str    natural key with project
platforms         json   array of str; platform families seen on this release
first_seen        ts
```

Upserted on `(project, release)`: platforms are merged, `first_seen` keeps
the earlier value.

### `issues`

```
project           str    slug
fingerprint       str    natural key with project
title             str
level             str    fatal error warning info debug
error_type?       str
transaction?      str    event.transaction (format 1 wrote this as `screen`; still read)
platform?         str
status            str    unresolved resolved ignored regression; default unresolved (`triaged`, format 2 and earlier, imports as unresolved)
event_count       int    events seen (including sampled-out)
stored_count      int    events actually stored
first_seen        ts
last_seen         ts
first_release?    str
last_release?     str
releases?         [str]  every release the issue was seen on ("" = events without one); default []
resolved_releases? [str] `releases` at resolve time (regression detection)
ignore_until?     ts     ignored: back to unresolved at this time
ignore_until_count? int  ignored: back when event_count reaches this
ignore_until_escalating? bool  ignored: back when the issue escalates; default false
ignore_baseline?  int    stored events in the 24 h before it was ignored (the escalation baseline)
created_at        ts
updated_at        ts
```

Import: upsert on `(project, fingerprint)`. **Counts never go down**:
`event_count` / `stored_count` become the greater of the row's and the
file's (a backup restored onto a live project does not rewind its
counts). A missing timestamp → now.

### `events`

```
project           str    slug
occurred_at       ts     required
event_id          str    required; 32 hex (the SDK's event_id, or one derived from the body at ingest)
level             str    fatal error warning info debug
message           str
platform?         str
environment?      str
release?          str
device_id?        str
device_model?     str
os_version?       str
transaction?      str    event.transaction (format 1: `screen`; still read)
error_type?       str
culprit?          str    Sentry's stack culprit, "module-or-file in function" (format 1: `error_location`; still read)
handled?          bool
sdk_name?         str
user_id?          str
fingerprint?      str    references issues.fingerprint in the same project
symbolicated      bool
tags              json   object; default {}
payload?          json   the raw Sentry event object exactly as the SDK sent it (absent when the row has none)
symbols?          json   symbolicated frames (only when symbolicated)
```

Import: insert; conflict on `(project, event_id, occurred_at)` → skip
(`ON CONFLICT DO NOTHING`). `tags` missing or `null` → default. A row
without `occurred_at` or `event_id` is an error.

### `attachments`

```
project           str    slug
occurred_at       ts     required; the event's occurred_at
event_id          str    required; 32 hex, the event's
n                 int    position among the event's attachments (0-based)
filename          str    default "attachment"
content_type      str    default application/octet-stream
attachment_type   str    default event.attachment
size              int    bytes; 0 → derived from data
data              b64
```

Import: insert; conflict on `(project, event_id, occurred_at, n)` → skip.
The event itself need not be in the file (a row without its event is
never shown, and expires with the partition).

### `user_reports`

```
project           str    slug
event_id          str    required; 32 hex, the report's event (need not be in the file, or exist at all)
name?             str
email?            str
comments          str
received_at       ts
```

Import: upsert on `(project, event_id)`; all listed columns replaced
(a resend overwrites, as ingest itself does). Sentry's `user_report`
envelope item; the newer `feedback` item is not accepted or exported.

### `sessions`

```
project           str    slug
started_at        ts     required
sid               str    required
release           str
environment?      str
status            str    ok exited errored crashed abnormal
count             int    ≥ 1 (aggregated session count); default 1
```

Import: insert; conflict on `(project, sid, started_at)` → skip.

### `symbol_files`

```
project           str    slug
kind              str    proguard sourcemap dsym
release           str
debug_id?         str
filename          str
size              int    bytes; 0 → derived from data
data              b64
uploaded_at       ts
```

Import: upsert on `(project, kind, release, filename)`; `debug_id`, `size`,
`data`, `uploaded_at` replaced.

### `alert_rules`

```
project           str    slug
type              str    new_issue regression unhandled_spike (format 1: `crash_spike`; still read)
enabled           bool
cooldown_minutes  int
last_triggered?   ts
```

Import: upsert on `(project, type)`.

### `alert_channels`

```
project           str    slug
kind              str    webhook telegram
config            json   object; kind-specific; default {}
created_at        ts
```

Import: insert only when no row with identical `(project, kind, config)`
exists (JSON equality, not string equality).

## Not exported

The statistics rollups (`event_stats_hourly_rolled`,
`issue_stats_hourly_rolled`, `release_health_hourly_rolled` and their
dirty keys), the job queue, upload chunks, viewer session cookies
(`user_sessions`) and the schema version. The rollups are recomputed after
import (`crashcart import` marks the imported hours dirty and rolls them
up); the rest expire or belong to the target database.

## Reader rules

1. Refuse `format` greater than what you support; otherwise proceed.
2. A line whose `t` you do not know is counted as `skipped` and ignored —
   newer exports load on older readers.
3. Import is idempotent: importing the same file twice changes nothing
   the second time (except `alert_channels` ordering and any `created_at`
   filled with *now* on rows that omitted it). Importing onto a live
   database **replaces** the listed columns of `projects`, `issues`
   (except the counts, which never go down), `symbol_files`,
   `alert_rules` and `user_reports` with the file's values; events,
   attachments and sessions are never overwritten; users and API keys
   are never overwritten.
4. Report per-table row counts on completion (`{"rows":{"events":123,…}}`).
5. Fail fast on the first malformed line, reporting its 1-based line number.
   `crashcart import` commits every 20 000 lines; a failed import keeps
   the chunks committed before the bad line (the error says how many) and,
   being idempotent, is simply re-run once the file is fixed.

## Format history

- **3** — issue status `triaged` is gone (Sentry has no such state); a
  format-2 file's `triaged` imports as `unresolved`. Culprits are Sentry's
  `module-or-file in function`. Later, additively (format unchanged):
  the `attachments` table, the issue ignore fields (`ignore_until`,
  `ignore_until_count`, `ignore_until_escalating`, `ignore_baseline`) and
  the `user_reports` table.
- **2** — Sentry vocabulary: `screen` → `transaction`, `error_location` →
  `culprit`, alert type `crash_spike` → `unhandled_spike`. A format-1 file
  still imports (the old names are read).
- **1** — initial.

## Evolving the format

- **Additive change** (new optional field, new table): keep `format` at 3.
  Old readers ignore unknown fields and unknown `t`.
- **Breaking change** (renamed/removed required field, changed encoding):
  bump `format`, keep reading the previous one for at least one release.
- Update this file, then the implementation and its `TestRoundTrip`.
