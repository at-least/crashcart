# Glossary

The vocabulary used across the viewer, the API and this documentation.

## Core concepts

| Term | Definition | Not |
|---|---|---|
| **Event** | One error, message or crash report sent by an SDK | log entry, record |
| **Issue** | A group of events sharing a fingerprint; the unit of triage | error group, bug |
| **Fingerprint** | Hash of the exception chain and the in-app stack used for grouping (an SDK `fingerprint` wins) | signature |
| **Release** | The version the SDK reports as `event.release` | app version, build |
| **Environment** | `production`, `staging`, `development`, … | env, profile |
| **Platform** | SDK family: `ios`, `android`, `flutter`, `react-native`, `web`, `backend`, `other` | OS, device type |
| **Project** | One DSN; the container for issues, events, releases and settings | |
| **DSN** | `scheme://<key>@host/<project id>` — the address an SDK reports to | |
| **Session** | One app run (mobile) or page load / request batch, as reported by the SDK for release health | |
| **Symbol file** | ProGuard / R8 mapping, source map or dSYM used to symbolicate frames | |
| **Transaction** | `event.transaction`: the screen, route or request the event happened in | screen, page |
| **Culprit** | Sentry's stack culprit: the innermost in-app frame as `module-or-file in function`, shown under the issue title | location |

## Event levels

| Level | Meaning |
|---|---|
| **fatal** | The process died |
| **error** | An error; whether it was caught is a separate fact (see *handled*) |
| **warning** | Unexpected but non-blocking |
| **info** | Informational; app-flow tracking |
| **debug** | Verbose, development only |

## Handled and unhandled

`level` is severity; **handled** is a separate fact the SDK sends as
`exception.mechanism.handled`. **Unhandled** (`false`) means the SDK
caught the error in a last-resort handler — a crash, an uncaught
exception, an unhandled promise rejection; it is Sentry's "Unhandled"
tag. An exception without a mechanism is neither handled nor unhandled,
as in Sentry. A mobile crash is both `fatal` and unhandled; a JavaScript unhandled
error is unhandled at level `error`. The overview's *Unhandled errors*,
the spike alert, the `handled=false` filter and the sampling keep factor
all count unhandled events. `exception.mechanism.type` (the
**mechanism**: `UncaughtExceptionHandler`, `signalhandler`, `onerror`,
`ANR`, …) is shown on the event.

"Crash" is a session word: a **crashed session** (`session.status =
crashed`) is one the app did not exit normally; an **errored session**
did not crash but reported `errors > 0`. The **crash-free rate** is what
release health reports.

## Issue status

| Status | Meaning |
|---|---|
| **unresolved** | Open |
| **resolved** | Fixed (records the releases it had been seen on) |
| **regression** | Was resolved, reappeared on a release it had not been seen on before (Sentry's *Regressed*, under "resolve in next release" rules) |
| **ignored** | Known, won't fix — for good, or until a condition: a time, a number of further events, or escalating (Sentry's *Archived* / "Archive until …") |

Sentry's statuses; there is no "triaged" state. An issue's level is its latest event's.

An ignored issue comes back to unresolved when its condition is met
(checked every minute): the time has passed; `event_count` reached the
count set when it was ignored; or it **escalates** — its stored events in
the last hour are 3× its hourly rate of the 24 h before it was ignored,
and at least 10 (the unhandled-spike rule, per issue), which also fires
the `escalating` alert.

## Event fields

| Field | Sentry SDK source | Example |
|---|---|---|
| `level` | `event.level` | `fatal` |
| `message` | `logentry.formatted` / `message`, else `Type: value` of the main exception | `NullPointerException: Attempt to invoke virtual method` |
| `platform` | `event.platform` | `android` |
| `release` | `event.release` only | `2.4.1` |
| `environment` | `event.environment` | `production` |
| `error_type` | the main exception's `type`: the one thrown last (`values[-1]`, or the one without a `parent_id`); its causes are shown under it | `NullPointerException` |
| `transaction` | `event.transaction` | `CartFragment` |
| `culprit` | computed: innermost in-app frame, `module-or-file in function` | `com.example.CartFragment in onCreateView` |
| `device_model` | `contexts.device.model` | `iPhone16,1` |
| `os_version` | `contexts.os.version` | `18.0` |
| `user_id` | `user.id` | `user-001` |
| `device_id` | `tags.device_id` — a CrashCart convention; the SDKs send no device id | `did-abc-123` |
| `handled` | `exception.mechanism.handled` (absent without a mechanism) | `false` = unhandled |
| `tags.server_name` | `server_name` (Sentry makes it a tag) | `web-1` |

## Counts

| Term | Meaning |
|---|---|
| **event_count** | Exact number of events matched to an issue, including sampled-out ones |
| **stored_count** | Events of the issue actually kept |
| **users** | Distinct `user.id`s seen on the issue |
| **unhandled** | Events with `handled = false` (overview, releases, spike alert) |
| **crash-free rate** | Sessions that did not crash ÷ all sessions, per release |
