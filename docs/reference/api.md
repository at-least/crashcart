# HTTP API

Base URL is wherever CrashCart listens (`PUBLIC_URL`). Conventions:

- JSON bodies and responses, `snake_case` keys
- Times are RFC 3339 in UTC; ids are integers (< 2⁵³)
- `Authorization: Bearer <key>` with one of `API_KEYS` on every route below
  except ingest and `/health`. When `API_KEYS` is empty the API is open.
- Errors are `{"error": "message"}` with a 4xx/5xx status
- `RATE_LIMIT` requests per minute per key; `429` when exceeded

## Projects

```
GET    /api/projects
POST   /api/projects                      {"slug","name","platform"}
GET    /api/projects/{slug}
PATCH  /api/projects/{slug}               {"name","platform","sample_keep_first","sample_rate","daily_quota"} (any subset)
DELETE /api/projects/{slug}
```

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
GET /api/projects/{slug}/overview?from=&to=&release=&environment=
```

Totals for the window, an hourly timeline of events / crashes / errors, the
latest release with its crash-free rate, and the top issues.

## Issues

```
GET    /api/projects/{slug}/issues            ?status=&release=&environment=&q=&from=&to=&before=
GET    /api/projects/{slug}/issues/{fingerprint}
PATCH  /api/projects/{slug}/issues/{fingerprint}     {"status": "resolved"}
POST   /api/projects/{slug}/issues/bulk              {"fingerprints": ["…"], "status": "ignored"}
```

- `status` — `unresolved` | `triaged` | `resolved` | `regression` | `ignored`
- `q` — substring match on title and location
- `from` / `to` — RFC 3339; `before` — pagination cursor from the previous
  page's last `last_seen_id`

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

## Events

```
GET /api/projects/{slug}/events        ?level=&release=&environment=&platform=&user_id=&fingerprint=&from=&to=&before=&tag.<key>=
GET /api/projects/{slug}/events/{id}
```

The list returns summaries (id, time, level, title, release, environment,
platform, user, device, OS, `fingerprint`). The detail returns the full
event: exception chain with original **and** symbolicated frames,
breadcrumbs, tags, user, contexts, and the raw payload as received.

Event ids encode the event time (`unix_ms × 1000 + random`), so they sort
chronologically and a time window is an id range.

## Releases

```
GET /api/projects/{slug}/releases          ?from=&to=&environment=
GET /api/projects/{slug}/releases/{version}
```

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

The release detail adds a daily series of `{day, total, crashed, errored}`.

## Alerts

```
GET    /api/projects/{slug}/alerts                     rules + channels
PATCH  /api/projects/{slug}/alerts/{type}              {"enabled": true}      type: new_issue | regression | crash_spike
POST   /api/projects/{slug}/alerts/channels            {"kind":"webhook","config":{"url":"…"}}
                                                       {"kind":"telegram","config":{"chat_id":"…"}}
DELETE /api/projects/{slug}/alerts/channels/{id}
```

The webhook payload is documented in [Alerts](/guide/alerts#webhook-payload).

## Symbols

```
GET    /api/projects/{slug}/symbols
POST   /api/projects/{slug}/symbols          multipart: kind (proguard|sourcemap|dsym), release, file
DELETE /api/projects/{slug}/symbols/{id}
```

Uploading re-queues the release's unsymbolicated events from the last
`COMPRESS_AFTER`. See [Symbolication](/guide/symbolication).

## Export / import

```
GET  /api/export                 ?project=<slug>     NDJSON stream
POST /api/import                 body: NDJSON
```

Same semantics as `crashcart export` / `import`; format in
[Export format](./export-format).

## Ingest (Sentry protocol)

```
POST /api/{project_id}/envelope/       Sentry envelope; the normal SDK path
POST /api/{project_id}/store/          legacy single-event JSON
```

Authenticated by the DSN key, sent by the SDK as `X-Sentry-Auth` or in the
`sentry_key` query parameter. Accepts gzip. Responds `200` with an empty
body; `401` wrong key, `404` unknown project, `413` envelope over 20 MB,
`429` rate limit or daily quota.

Envelope items handled: `event`, `session`, `sessions`. Others
(`transaction`, `profile`, `replay_event`, `client_report`, …) are accepted
and dropped so SDKs never see an error.

CORS preflight is answered with `CORS_ORIGIN`.

## sentry-cli compatibility

```
GET|POST /api/0/organizations/{org}/chunk-upload/               options / chunk upload
POST     /api/0/projects/{org}/{slug}/files/difs/assemble/      assemble uploaded chunks
GET|POST /api/0/projects/{org}/{slug}/files/dsyms/              legacy upload; lookup by debug_id
```

`{org}` is ignored; `{slug}` is the CrashCart slug or numeric id. Bearer
auth with an API key (`SENTRY_AUTH_TOKEN`). The chunk-upload URL returned
in the options response is built from `PUBLIC_URL`.

## Health

```
GET /health        200 when the process and the database are up
```
