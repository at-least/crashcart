# HTTP API

Base URL is wherever CrashCart listens (`PUBLIC_URL`). Conventions:

- JSON bodies and responses, `snake_case` keys
- Times are RFC 3339 in UTC; ids are integers (< 2⁵³)
- `Authorization: Bearer <key>` with one of `API_KEYS` on every route below
  except ingest and `/health`. When `API_KEYS` is empty the API is open
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
  "totals": { "events": 4021, "crashes": 37, "errors": 1190 },
  "levels": { "fatal": 37, "error": 1190, "warning": 402, "info": 2392 },
  "new_issues": 3,
  "regressions": 1,
  "crash_free": { "release": "2.4.1", "rate": 99.958, "sessions": 88120 },
  "timeline": [
    { "bucket": "2026-08-22T08:00:00Z", "release": "2.4.1", "events": 21, "crashes": 0 }
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

List parameters (all optional):

| Parameter | Meaning |
|---|---|
| `status` | `unresolved` · `triaged` · `resolved` · `regression` · `ignored` |
| `level` | `fatal` · `error` · `warning` · `info` · `debug` |
| `release` | Exact match on `last_release` |
| `q` | Substring match on title and location |
| `sort` | `last_seen` (default) · `first_seen` · `events` — always descending |
| `limit`, `offset` | Page size (default 50) and offset |
| `days`, `from`, `to` | Window on `last_seen`; default 7 days |

Response: `{"issues": [...], "total": n}`.

Issue object:

```json
{
  "fingerprint": "5f1c3a…",
  "title": "NSInvalidArgumentException at CartViewController.swift:88",
  "level": "fatal",
  "error_type": "NSInvalidArgumentException",
  "screen": "CartViewController.swift:88",
  "platform": "ios",
  "status": "unresolved",
  "event_count": 1284,
  "stored_count": 212,
  "users": 391,
  "first_seen": "2026-08-20T11:02:13Z",
  "last_seen": "2026-08-29T08:41:55Z",
  "first_seen_id": 1755687733000123,
  "last_seen_id": 1756456915000842,
  "first_release": "2.4.0",
  "last_release": "2.4.1",
  "resolved_release": null,
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
  "latest_event_id": 1756456915000842,
  "oldest_event_id": 1755687733000123
}
```

`PATCH` and `bulk` accept any of the five statuses; `bulk` returns
`{"updated": n, "status": "…"}`.

## Events

```
GET /api/projects/{slug}/events
GET /api/projects/{slug}/events/{id}
```

List parameters (all optional):

| Parameter | Meaning |
|---|---|
| `level`, `release`, `environment`, `platform`, `error_type`, `user_id`, `device_id`, `device_model`, `os_version`, `screen`, `error_location`, `fingerprint` | Exact match |
| `tag.<key>` | Exact match on a tag, e.g. `tag.tenant=acme` |
| `q` | Substring match on the message |
| `crash` | `1` / `true`: only unhandled (fatal) events |
| `before` | Cursor: an event id; returns events older than it |
| `limit` | Page size, default 50 |
| `days`, `from`, `to` | Window. **When none is given the list is unbounded** and pages through all retained events by cursor |

Response: `{"events": [...], "more": bool, "next_before": id | null}` —
pass `next_before` as `before` for the next page.

List items are summaries (id, `time`, level, title, release, environment,
platform, user, device, OS, `fingerprint`, tags). The detail returns the
full event row: the exception chain with original **and** symbolicated
frames, breadcrumbs, tags, user, contexts, and the raw payload as received.

Event ids encode the event time (`unix_ms × 1000 + random`), so they sort
chronologically and a time window is an id range.

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
  "crashes": 37,
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
  "timeline":     [ { "bucket": "2026-08-28T13:00:00Z", "events": 60, "crashes": 1 } ],
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

`{type}` is `new_issue`, `regression` or `crash_spike`. A rule:

```json
{ "project_id": 1, "type": "crash_spike", "enabled": true, "cooldown_minutes": 60, "last_triggered": "…" }
```

`cooldown_minutes` is the minimum gap between two alerts of the same type
for the project. A webhook `url` must be `http(s)`; a Telegram channel
needs `chat_id` and `TELEGRAM_BOT_TOKEN` on the server. The webhook
payload is documented in [Alerts](/guide/alerts#webhook-payload).

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

Uploading re-queues the release's unsymbolicated events from the last
`COMPRESS_AFTER`. See [Symbolication](/guide/symbolication).

## Export / import

Export and import are **CLI commands** in the Go edition —
`crashcart export` and `crashcart import` ([CLI](./cli)) — not HTTP routes.
The serverless edition exposes them as `GET /api/export` and
`POST /api/import`. Format: [Export format](./export-format).

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
