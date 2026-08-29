# Getting started

CrashCart is *Sentry SDK compatible*: any Sentry SDK pointed at a CrashCart
DSN reports crashes, errors, messages and release-health sessions into it.

```
Sentry SDK ──POST /api/{id}/envelope/──▶ crashcart ──▶ Postgres (+ TimescaleDB)
Browser    ──GET  /p/{slug}/…  (htmx) ──▶    │
Scripts    ──GET  /api/projects/… ──────▶    │
sentry-cli ──POST /api/0/…/files/dsyms/ ─▶   └──▶ symbolicate sidecar (dSYM only, optional)
```

## Run it

```sh
docker compose up -d                       # TimescaleDB + crashcart on :8080
docker compose exec crashcart /crashcart project shop "Shop app" ios
# project shop (id 1)
# DSN: http://<key>@localhost:8080/1
```

## Point an SDK at it

Paste the DSN into the SDK:

```swift
SentrySDK.start { $0.dsn = "http://<key>@localhost:8080/1" }
```

Open `http://localhost:8080` to browse issues as they arrive.

## Next steps

- [Projects & DSNs](./projects) — how projects, platforms and releases relate.
- [Export format](../export-format) — the NDJSON interchange contract.
