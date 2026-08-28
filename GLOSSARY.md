# CrashCart — Glossary

Standardized terminology. Use these consistently across code, docs, viewer.

## Core Concepts

| Term | Definition | NOT this |
|---|---|---|
| **Event** | A single log/crash/error sent by SDK | ~~log entry~~ ~~record~~ |
| **Release** | App version or deployment identifier | ~~app version~~ ~~build~~ |
| **Environment** | production / staging / development | ~~env~~ ~~profile~~ |
| **Platform** | ios / android / node / python / javascript | ~~OS~~ ~~device type~~ |
| **Issue** | Group of events with same fingerprint | ~~error group~~ ~~bug~~ |
| **Fingerprint** | Hash of stack trace for grouping | ~~signature~~ |

## Event Levels

| Level | Meaning |
|---|---|
| **fatal** | App crashed, unhandled exception |
| **error** | Caught exception, needs attention |
| **warning** | Unexpected but non-blocking |
| **info** | Informational, app flow tracking |
| **debug** | Verbose, development only |

## Issue Status

| Status | Meaning |
|---|---|
| **unresolved** | New, not yet reviewed |
| **triaged** | Acknowledged, being investigated |
| **resolved** | Fixed |
| **regression** | Was resolved, reappeared in new release |
| **ignored** | Known, won't fix |

## Event Metadata

| Field | Sentry SDK source | Example |
|---|---|---|
| `level` | event.level | fatal |
| `message` | logentry.formatted | NullPointerException in CartFragment |
| `platform` | event.platform | ios |
| `release` | contexts.app.app_version | 2.4.1 |
| `environment` | event.environment | production |
| `error_type` | exception.values[0].type | NullPointerException |
| `error_location` | computed: deepest app frame | CartFragment.java:142 |
| `device_model` | contexts.device.model | iPhone16,1 |
| `os_version` | contexts.os.version | 18.0 |
| `user_id` | user.id | user-001 |
| `device_id` | tags.device_id | did-abc-123 |
| `handled` | exception.mechanism.handled | false = crash |

## Branding Rules

| ✅ Use | ❌ Don't use |
|---|---|
| CrashCart | ~~Trace Sentry~~ |
| Sentry SDK compatible | ~~Sentry backend~~ |
| Works with Sentry SDK | ~~Sentry alternative~~ |
| Open-source error tracking | ~~Sentry replacement~~ |
| `diagnose` crash | ~~analyze Sentry crash~~ |

## Abbreviations

| Abbr | Full |
|---|---|
| PK | Primary Key (logs.id = event timestamp_ms × 1000 + random) |
| D1 | Cloudflare D1 (SQLite at edge) |
| R2 | Cloudflare R2 (object storage) |
| UPSERT | INSERT ... ON CONFLICT DO UPDATE (SQLite/PG) / ON DUPLICATE KEY UPDATE (MySQL) |
| ALERT_TYPES | `alert_types` table — 4 predefined alert detectors: crash_spike, new_error, regression, high_count (toggle via `enabled`) |

## Database Backend Terms

| Term | Definition |
|---|---|
| **Dialect** | SQL flavor: `"sqlite"`, `"postgres"`, or `"mysql"`. Set at init via `setDialect()`. Controls SQL fragment generation in `dialect.ts`. |
| **Adapter** | Module that wraps a DB driver to match the D1Database interface (`prepare().bind().run()/.all()/.first()`). See `postgres.ts`, `mysql.ts`. |
| **Hyperdrive** | Cloudflare connection pooling for external DBs (Postgres, MySQL). Optional optimization — CrashCart works without it via HTTP queries. |
| **DATABASE_URL** | Env var that selects the backend. `postgresql://` → Postgres, `mysql://` → MySQL. Omit → D1. |
