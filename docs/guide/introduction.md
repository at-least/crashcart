# Introduction

CrashCart is open-source error tracking for mobile and web apps. It is
**Sentry SDK compatible**: point any Sentry SDK at a CrashCart DSN and it
receives crashes, errors, messages and release-health sessions.

CrashCart does not imitate the Sentry product. The viewer, the HTTP API and
the data model are its own. What it shares with Sentry is the wire protocol
the SDKs speak, so you keep the client libraries you already ship and swap
only the backend.

```
Sentry SDK ──POST /api/{id}/envelope/──▶ crashcart ──▶ Postgres (+ TimescaleDB)
Browser    ──GET  /p/{slug}/…  (htmx) ──▶    │
Scripts    ──GET  /api/projects/… ──────▶    │
sentry-cli ──POST /api/0/…/files/dsyms/ ─▶   └──▶ symbolicate sidecar (dSYM only, optional)
```

## What you get

- **One binary, one database.** A stateless Go binary and Postgres.
  TimescaleDB is used when available and makes storage cheaper at scale, but
  plain Postgres — Neon, Supabase, RDS — works unchanged.
- **Issues, not logs.** Events are grouped by a fingerprint of the stack
  trace into *issues* with a status lifecycle (unresolved → triaged →
  resolved → regression / ignored), exact counts, first/last release.
- **Release health.** Sessions from the SDKs give crash-free rates per
  release, and the crash-spike detector compares the last hour to the day
  before.
- **Symbolication.** ProGuard / R8 mappings and source maps are resolved
  in-process; iOS/macOS dSYMs through an optional sidecar. Uploads work with
  `curl` or `sentry-cli`.
- **Alerts** to webhooks or Telegram for new issues, regressions and crash
  spikes.
- **A portable export format.** Every table streams out as NDJSON and loads
  back into any CrashCart database, including the
  [serverless edition](/deploy/serverless).

## Compatibility scope

CrashCart accepts what the SDKs send for error tracking:

| Envelope item | Handling |
|---|---|
| `event` (errors, messages, crashes) | Stored, grouped into issues |
| `session`, `sessions` | Stored, aggregated into release health |
| `transaction`, `profile`, `replay`, `client_report` | Accepted and dropped |

Performance tracing, profiling and session replay are out of scope. The
Sentry Web API is not implemented — CrashCart has its own
[JSON API](/reference/api) — with one exception: the debug-file upload
endpoints `sentry-cli` uses, so symbol upload from CI keeps working.

See [SDK compatibility](/reference/sdks) for the client libraries exercised
end to end.

## Two editions

| | Go edition (this documentation) | [Serverless edition](/deploy/serverless) |
|---|---|---|
| Runtime | One Go binary | Cloudflare Workers |
| Storage | Postgres, TimescaleDB optional | D1 + R2 |
| Best for | Self-hosting, larger volumes | Small apps on the Cloudflare Free plan |
| Data | Same [export format](/reference/export-format) — move in either direction | |

Both implement the same ingest protocol, the same grouping and the same
viewer.

## Next

[Getting started](./getting-started) runs CrashCart with Docker Compose in
about a minute.
