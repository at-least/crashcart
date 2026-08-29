# Introduction

CrashCart is open-source error tracking for mobile and web apps. It works
with the **Sentry SDK**: point any Sentry SDK at a CrashCart DSN and your
crashes, errors, messages and release health show up in CrashCart.

You keep the client libraries you already ship and swap only the backend.
CrashCart's viewer, API and data model are its own — it does not imitate
the Sentry product.

## What you get

- **Self-hosted in a minute.** One Docker Compose file: CrashCart and a
  Postgres database. Nothing else to run.
- **Issues, not logs.** Crashes with the same stack trace are grouped into
  one issue with a status (unresolved → triaged → resolved), an exact count,
  and the releases it was first and last seen on.
- **Release health.** Crash-free rate per release, and which issues a
  release introduced.
- **Readable stack traces.** Upload ProGuard / R8 mappings, source maps or
  dSYMs — with `curl` or `sentry-cli` — and frames show real file names and
  lines.
- **Alerts** to a webhook or Telegram when a new issue appears, a resolved
  one comes back, or crashes spike.
- **Your data stays portable.** One command exports everything to a plain
  text file that restores into any CrashCart.

## What it does not do

CrashCart is for error tracking. It does not do performance tracing,
profiling or session replay. If your SDK sends those, they are accepted
and discarded — no errors, just nothing stored.

## Two editions

| | Go edition (this documentation) | Serverless edition |
|---|---|---|
| Runs on | Any server with Postgres | Cloudflare Workers (Free plan is enough for small apps) |
| Best for | Self-hosting, larger volumes | Small apps, zero servers |

Both work with the same SDKs and share the same export format, so you can
move between them. See [Serverless edition](/deploy/serverless).

## Next

[Getting started](./getting-started) — from nothing to your first crash in
the viewer.
