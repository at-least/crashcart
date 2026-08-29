# Compared to Sentry

CrashCart works with the Sentry SDK, so the first question is always "how
is it different from Sentry?" Honest answer below, in both directions.

## What's the same

- **The SDKs.** Every Sentry SDK — iOS, Android, Flutter, React Native,
  browser, Node, Python, Go, Java, .NET, Rust — ships crashes to CrashCart
  unchanged. Only the DSN changes.
- **The upload tooling.** `sentry-cli` uploads ProGuard mappings, source
  maps and dSYMs to CrashCart as it would to Sentry. The Android Gradle
  plugin works too.
- **The core loop.** Errors are grouped into issues by stack trace, issues
  have a status and a lifecycle (resolved → regression), releases have a
  crash-free rate, and you get told when something new breaks.

## What's different

| | CrashCart | Sentry (SaaS or self-hosted) |
|---|---|---|
| Scope | Error tracking and release health, nothing else | Errors, performance tracing, profiling, session replay, logs, uptime, cron monitoring |
| Users | One viewer password, shared. API keys for automation | Accounts, teams, roles, SSO |
| Running it | One binary + Postgres, or a Cloudflare Worker. 512 MB of RAM is plenty | Self-hosted needs 16 GB RAM, 4 cores and ~20 services; or the hosted plans |
| Integrations | Webhooks and Telegram | Slack, Jira, GitHub, PagerDuty and dozens more |
| Ownership of the data | Your Postgres or your Cloudflare account. No telemetry, nothing leaves the server unless you add an alert channel | Sentry's cloud, or your own self-hosted install |
| Price | Free and MIT; what your server costs | Per-event plans; self-hosted is free |

Two more things to know before you switch:

- **Data sent by the SDK that CrashCart does not store** — transactions,
  profiles, replays — is accepted and discarded. Leaving those SDK
  features on is harmless; you just won't see the data anywhere.
- **iOS / macOS** ingest and grouping work from captured envelopes, but
  sentry-cocoa has not yet been run against CrashCart end to end with a
  real device — see [SDK compatibility](/reference/sdks). If you do, tell
  us what you saw.

## Who it is for

CrashCart fits a team that wants crash reports and release health from
its own apps, on a server it controls, without operating a large system
or paying per event. If you need performance tracing, replay, or
per-person accounts and permissions, Sentry is the better tool — and
nothing stops you from running both on the same SDK with two DSNs while
you decide.

## Moving from Sentry

There is no import of historical Sentry data; issues and events start
fresh. The move is small:

1. [Install CrashCart](/deploy/which-edition) and create one project per
   app and platform.
2. Replace the DSN in each app with the CrashCart one. Ship a release.
3. Point `sentry-cli` (or the Gradle plugin) at CrashCart for symbol
   uploads — `SENTRY_URL` and an API key, see
   [Symbolication](./symbolication).
4. Recreate alert rules under **Settings → Alerts**.

Old builds in the field keep reporting to Sentry until they are updated;
run both for a release or two, then turn Sentry off.
