# Glossary

The vocabulary used across the viewer, the API and this documentation.

## Core concepts

| Term | Definition | Not |
|---|---|---|
| **Event** | One crash, error or message sent by an SDK | log entry, record |
| **Issue** | A group of events sharing a fingerprint; the unit of triage | error group, bug |
| **Fingerprint** | Hash of the stack trace used for grouping | signature |
| **Release** | The app version or deployment identifier the SDK reports | app version, build |
| **Environment** | `production`, `staging`, `development`, … | env, profile |
| **Platform** | SDK family: `ios`, `android`, `flutter`, `react-native`, `web`, `backend`, `other` | OS, device type |
| **Project** | One DSN; the container for issues, events, releases and settings | |
| **DSN** | `scheme://<key>@host/<project id>` — the address an SDK reports to | |
| **Session** | One app run (mobile) or page load / request batch, as reported by the SDK for release health | |
| **Symbol file** | ProGuard / R8 mapping, source map or dSYM used to symbolicate frames | |

## Event levels

| Level | Meaning |
|---|---|
| **fatal** | The app crashed; unhandled exception |
| **error** | Caught exception that needs attention |
| **warning** | Unexpected but non-blocking |
| **info** | Informational; app-flow tracking |
| **debug** | Verbose, development only |

## Issue status

| Status | Meaning |
|---|---|
| **unresolved** | New, not yet reviewed |
| **triaged** | Acknowledged, being investigated |
| **resolved** | Fixed (records the release it was resolved on) |
| **regression** | Was resolved, reappeared on a different release |
| **ignored** | Known, won't fix |

## Event fields

| Field | Sentry SDK source | Example |
|---|---|---|
| `level` | `event.level` | `fatal` |
| `message` | `logentry.formatted` | `NullPointerException in CartFragment` |
| `platform` | `event.platform` | `android` |
| `release` | `event.release` / `contexts.app.app_version` | `2.4.1` |
| `environment` | `event.environment` | `production` |
| `error_type` | `exception.values[0].type` | `NullPointerException` |
| `error_location` | computed: deepest in-app frame | `CartFragment.java:142` |
| `device_model` | `contexts.device.model` | `iPhone16,1` |
| `os_version` | `contexts.os.version` | `18.0` |
| `user_id` | `user.id` | `user-001` |
| `device_id` | `tags.device_id` | `did-abc-123` |
| `handled` | `exception.mechanism.handled` | `false` = crash |

## Counts

| Term | Meaning |
|---|---|
| **event_count** | Exact number of events matched to an issue, including sampled-out ones |
| **stored_count** | Events of the issue actually kept |
| **users** | Distinct `user.id`s seen on the issue |
| **crash-free rate** | Sessions that did not crash ÷ all sessions, per release |

## Positioning

CrashCart is **Sentry SDK compatible** and **works with the Sentry SDK**. It
is not a Sentry backend, mirror or drop-in replacement for the Sentry
product: the protocol is shared, the product is its own.
