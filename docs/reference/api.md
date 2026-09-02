# HTTP API

Base URL is wherever CrashCart listens (`PUBLIC_URL`). Conventions:

- JSON bodies and responses, `snake_case` keys
- Times are RFC 3339 in UTC; ids are integers (< 2⁵³)
- `Authorization: Bearer <key>` with an API key on every route below except
  ingest and `/health`. Keys are created on the viewer's **Account** page or
  with `crashcart apikey create`; the secret (`cc_…`) is shown once
- Errors are `{"error": "message"}` with a 4xx/5xx status
- `RATE_LIMIT` requests per minute per key; `429` when exceeded

## Time windows

Overview, issues, releases and (optionally) events take a window:

| Parameter | Meaning |
|---|---|
| `days` | Window ending now. Default `7`, maximum `90` |
| `from`, `to` | RFC 3339 bounds. `to` is exclusive and defaults to now; `from` defaults to `to − days`. The span may not exceed 90 days |

## Projects

```
GET    /api/projects
POST   /api/projects                        {"slug","name","platform"}            → 201
GET    /api/projects/{slug}
PATCH  /api/projects/{slug}                 any of {"name","platform","sample_keep_first","sample_rate","daily_quota"}
DELETE /api/projects/{slug}
POST   /api/projects/{slug}/rotate-key      new DSN key → project object
```

Validation: `slug` matches `^[a-z0-9][a-z0-9._-]{0,63}$` and is not
reserved (`409` if it exists); `platform` is one of `ios`, `android`,
`flutter`, `react-native`, `web`, `backend`, `other`; `sample_keep_first ≥ 0`;
`0 ≤ sample_rate ≤ 1`; `daily_quota ≥ 0` (`0` = unlimited).

Project object:

```json
{
  "id": 1,
  "slug": "shop-ios",
  "name": "Shop app (iOS)",
  "platform": "ios",
  "sample_keep_first": 100,
  "sample_rate": 1.0,
  "daily_quota": 100000,
  "created_at": "2026-08-01T09:00:00Z",
  "dsn": "https://<key>@crashcart.example.com/1"
}
```

`DELETE` removes the project and everything under it.

## Overview

```
GET /api/projects/{slug}/overview?days=7
```

```json
{
  "from": "2026-08-22T08:00:00Z",
  "to": "2026-08-29T08:00:00Z",
  "totals": { "events": 4021, "unhandled": 37, "errors": 1190 },
  "levels": { "fatal": 37, "error": 1190, "warning": 402, "info": 2392 },
  "new_issues": 3,
  "regressions": 1,
  "crash_free": { "release": "2.4.1", "rate": 99.958, "sessions": 88120 },
  "timeline": [
    { "bucket": "2026-08-22T08:00:00Z", "release": "2.4.1", "events": 21, "unhandled": 0 }
  ]
}
```

`crash_free` is the latest release seen in the window, `null` when there
are no sessions. `timeline` is hourly, split by release.

## Issues

```
GET    /api/projects/{slug}/issues
GET    /api/projects/{slug}/issues/{fingerprint}
PATCH  /api/projects/{slug}/issues/{fingerprint}     {"status": "resolved"}
POST   /api/projects/{slug}/issues/bulk              {"fingerprints": ["…"], "status": "ignored"}
```

A status change body is `{"status": "unresolved" | "resolved" | "ignored"}`;
with `ignored`, optional conditions say when the issue comes back to
unresolved on its own (any combination; none = ignored for good):

| Field | Meaning |
|---|---|
| `ignore_minutes` | back after this many minutes |
| `ignore_events` | back after this many further events (`ignore_until_count` = the count now + this) |
| `ignore_until_escalating` | back when the issue's events in an hour are 3× its rate of the 24 h before it was ignored, and at least 10 — with an `escalating` alert |

```json
{"status": "ignored", "ignore_minutes": 10080, "ignore_until_escalating": true}
```

List parameters (all optional):

| Parameter | Meaning |
|---|---|
| `status` | `unresolved` · `resolved` · `regression` · `ignored` |
| `level` | `fatal` · `error` · `warning` · `info` · `debug` |
| `release` | Exact match on `last_release` |
| `q` | Substring match on title and culprit |
| `sort` | `last_seen` (default) · `first_seen` · `events` — always descending |
| `limit`, `offset` | Page size (default 50) and offset |
| `days`, `from`, `to` | Window on `last_seen`; default 7 days |

Response: `{"issues": [...], "total": n}`.

Issue object:

```json
{
  "fingerprint": "5f1c3a…",
  "title": "NSInvalidArgumentException: -[__NSArrayI objectAtIndex:]: index 3 beyond bounds",
  "level": "fatal",
  "error_type": "NSInvalidArgumentException",
  "transaction": "CartViewController",
  "platform": "ios",
  "status": "unresolved",
  "event_count": 1284,
  "stored_count": 212,
  "first_seen": "2026-08-20T11:02:13Z",
  "last_seen": "2026-08-29T08:41:55Z",
  "first_release": "2.4.0",
  "last_release": "2.4.1",
  "releases": ["2.4.0", "2.4.1"],
  "resolved_releases": null,
  "ignore_until": null,
  "ignore_until_count": null,
  "ignore_until_escalating": false,
  "created_at": "…",
  "updated_at": "…",
  "sparkline": [0, 2, 5, 11, 9, 3]
}
```

`sparkline` is hourly counts for the last 24 h, present on list responses.

The detail (`GET …/issues/{fingerprint}`, takes a window) adds:

```json
{
  "…issue fields…": "",
  "timeline": [ { "bucket": "2026-08-29T07:00:00Z", "events": 9 } ],
  "breakdown": {
    "release":      [ { "value": "2.4.1", "count": 1102 } ],
    "device_model": [ { "value": "iPhone16,1", "count": 340 } ],
    "os_version":   [ { "value": "18.0", "count": 601 } ],
    "environment":  [ { "value": "production", "count": 1284 } ]
  },
  "latest_event_id": "e1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4",
  "oldest_event_id": "0a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d"
}
```

`latest_event_id` / `oldest_event_id` are the newest and oldest *stored*
events of the issue (`""` when none is stored), usable with
`GET …/events/{event_id}`.

`PATCH` and `bulk` accept `unresolved`, `resolved` and `ignored` (with
the conditions above; `regression` is ingest's verdict); `bulk` returns
`{"updated": n, "status": "…"}`.

## Events

```
GET /api/projects/{slug}/events
GET /api/projects/{slug}/events/{event_id}
GET /api/projects/{slug}/events/{event_id}/attachments/{n}
```

The event detail carries `attachments`: one entry per file the SDK
attached (a crash screenshot, a view hierarchy, …) with `n`, `filename`,
`content_type`, `attachment_type`, `size` and `url`; the URL returns the
bytes (images under their own type, anything else as an
`application/octet-stream` download).

List parameters (all optional):

| Parameter | Meaning |
|---|---|
| `level`, `release`, `environment`, `platform`, `error_type`, `user_id`, `device_id`, `device_model`, `os_version`, `transaction`, `culprit`, `fingerprint` | Exact match |
| `tag.<key>` | Exact match on a tag, e.g. `tag.tenant=acme` |
| `q` | Substring match on the message |
| `handled` | `false`: only unhandled events (`exception.mechanism.handled = false` — crashes, uncaught exceptions); `true`: only handled ones. Sentry's `yes` / `no` are accepted too |
| `before` | Cursor: the `next_before` of the previous page; returns the events after it (newest first) |
| `limit` | Page size, default 50 |
| `days`, `from`, `to` | Window. **When none is given the list is unbounded** and pages through all retained events by cursor |

Response: `{"events": [...], "more": bool, "next_before": string | null}` —
pass `next_before` as `before` for the next page (it is opaque:
`<occurred_at RFC3339>_<event_id>`, URL-encode it).

List items are summaries (`event_id`, `occurred_at`, level, message,
release, environment, platform, user, device, OS, `fingerprint`, tags),
newest first. The detail (by the Sentry `event_id`) returns the full event
row: the exception chain with original **and** symbolicated frames,
breadcrumbs, tags, user, contexts, and the raw payload as received
(`null` for an event imported without one).

The event detail also carries `user_report`: the Sentry `user_report`
envelope item associated with this event (`event_id`, `name`, `email`,
`comments`, `received_at`), or absent (the field is omitted) when the
event has none.


## User reports

```
GET /api/projects/{slug}/user_reports
```

Every `user_report` envelope item received by the project, newest first —
including reports whose event was dropped by per-issue sampling, is still
in flight, or never arrives at all: this list has no join to `events`, so
it is the only place such a report surfaces.

| Parameter | Meaning |
|---|---|
| `limit` | Page size, default 50 |
| `offset` | Rows to skip, capped at 10000 |

Response: `{"user_reports": [{"event_id","name","email","comments","received_at"}, ...], "total": n}`.


## Releases

```
GET /api/projects/{slug}/releases?days=30
GET /api/projects/{slug}/releases/{version}?days=30
```

List response `{"releases": [...]}`:

```json
{
  "release": "2.4.1",
  "platforms": ["ios"],
  "first_seen": "…",
  "last_seen": "…",
  "events": 4021,
  "unhandled": 37,
  "errors": 1190,
  "sessions": { "total": 88120, "crashed": 37, "errored": 902 },
  "crash_free_rate": 99.958,
  "new_issues": 3
}
```

`crash_free_rate` is `null` when the release has no sessions.

The detail returns:

```json
{
  "release": { "…release object…": "" },
  "from": "…", "to": "…",
  "daily_health": [ { "day": "2026-08-28T00:00:00Z", "total": 12040, "crashed": 5, "errored": 130, "crash_free_rate": 99.958 } ],
  "timeline":     [ { "bucket": "2026-08-28T13:00:00Z", "events": 60, "unhandled": 1 } ],
  "issues_introduced": [ "…issues whose first_release is this version…" ],
  "issues_present":    [ "…issues whose last_release is this version…" ]
}
```

## Alerts

```
GET    /api/projects/{slug}/alerts                     {"rules": [...], "channels": [...]}
PATCH  /api/projects/{slug}/alerts/{type}              {"enabled": bool, "cooldown_minutes": n}   (either)
POST   /api/projects/{slug}/alerts/channels            {"kind":"webhook","config":{"url":"https://…"}}   → 201
                                                       {"kind":"telegram","config":{"chat_id":"…"}}      → 201
DELETE /api/projects/{slug}/alerts/channels/{id}
```

`{type}` is `new_issue`, `regression`, `unhandled_spike` or `escalating`. A rule:

```json
{ "project_id": 1, "type": "unhandled_spike", "enabled": true, "cooldown_minutes": 60, "last_triggered": "…" }
```

`cooldown_minutes` is the minimum gap between two alerts of the same type
for the project. A webhook `url` must be `http(s)`; a Telegram channel
needs `chat_id` and `TELEGRAM_BOT_TOKEN` on the server. The webhook
payload is documented in [Alerts](/guide/alerts#where-alerts-go).

## Symbols

```
GET    /api/projects/{slug}/symbols          {"symbols": [...]}
POST   /api/projects/{slug}/symbols          multipart: kind, release, file   → 201 {"symbols": [...]}
DELETE /api/projects/{slug}/symbols/{id}
```

`kind` is `proguard`, `sourcemap` or `dsym`. A symbol file:

```json
{ "id": 12, "project_id": 1, "kind": "proguard", "release": "2.4.1", "debug_id": "…", "filename": "mapping.txt", "size": 1048576, "uploaded_at": "…" }
```

Uploading also symbolicates the release's most recent unsymbolicated
events (newest first, up to 2000). See [Symbolication](/guide/symbolication).

## Export / import

Export and import are **CLI commands** in the Go edition —
`crashcart export` and `crashcart import` ([CLI](./cli)) — not HTTP routes.
Format: [Export format](./export-format).

## Ingest (Sentry protocol)

```
POST /api/{project_id}/envelope/       Sentry envelope; the normal SDK path
POST /api/{project_id}/store/          legacy single-event JSON
```

The trailing slash is optional. Authenticated by the DSN key, sent by the
SDK as `X-Sentry-Auth` or the `sentry_key` query parameter. Accepts gzip.

| Status | Meaning |
|---|---|
| `200` | Accepted |
| `401` | Wrong or missing key |
| `413` | Envelope over 20 MB, or too many events in one envelope |
| `429` | Rate limit, or the project's daily quota exceeded |

Envelope items handled: `event`, `session`, `sessions`. Others
(`transaction`, `profile`, `replay_event`, `client_report`, …) are accepted
and dropped so SDKs never see an error. CORS preflight is answered with
`CORS_ORIGIN`.

## sentry-cli compatibility

```
GET|POST /api/0/organizations/{org}/chunk-upload/                       options / chunk upload
POST     /api/0/projects/{org}/{project}/files/difs/assemble/           assemble uploaded chunks
GET|POST /api/0/projects/{org}/{project}/files/dsyms/                   legacy upload; lookup by ?debug_id=
POST     /api/0/projects/{org}/{project}/files/dsyms/associate/         associate (accepted; no-op)
POST     /api/0/projects/{org}/{project}/files/proguard-artifact-releases   Gradle plugin release association
```

`{org}` is ignored; `{project}` is the CrashCart slug or numeric id. Bearer
auth with an API key (`SENTRY_AUTH_TOKEN`). The chunk-upload URL returned
in the options response is built from `PUBLIC_URL`.

## Health

```
GET /health        {"status":"ok"} — 503 {"status":"db unavailable"} when Postgres does not answer
```
