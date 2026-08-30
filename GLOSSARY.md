# CrashCart — Glossary

The vocabulary is Sentry's: what the SDK sends is named the way the
Sentry protocol and UI name it, so nothing has to be translated in either
direction. Use these terms consistently across code, docs and the viewer;
do not coin synonyms.

## Core concepts

| Term | Definition | NOT this |
|---|---|---|
| **Event** | One error, message or crash report sent by an SDK (an envelope `event` item) | ~~log entry~~ ~~record~~ |
| **Issue** | A group of events with the same fingerprint; the unit of triage | ~~error group~~ ~~bug~~ |
| **Fingerprint** | Hash of the stack trace used for grouping | ~~signature~~ |
| **Release** | The version the SDK reports (`event.release`) | ~~app version~~ ~~build~~ |
| **Environment** | `event.environment`: production / staging / development | ~~env~~ ~~profile~~ |
| **Platform** | SDK family: ios / android / javascript / node / python … | ~~OS~~ ~~device type~~ |
| **Transaction** | `event.transaction`: the screen, route or request the event happened in | ~~screen~~ ~~page~~ |
| **Culprit** | Where the error is: the innermost in-app frame, `File.ext:line` (Sentry shows it under the title) | ~~location~~ |
| **Session** | One app run / page load reported for release health (`session` / `sessions` items) | |

## Event: level and handled

Two independent facts, as the SDK sends them:

| Term | SDK source | Meaning |
|---|---|---|
| **Level** | `event.level` | Severity: `fatal` (the process died), `error`, `warning`, `info`, `debug` |
| **Unhandled** | `exception.mechanism.handled = false` | The SDK caught the error in a last-resort handler — a crash, an uncaught exception, an unhandled rejection. Sentry's "Unhandled" tag |
| **Handled** | `exception.mechanism.handled = true` | The app caught it and called `captureException` |
| **Mechanism** | `exception.mechanism.type` | How it was caught: `UncaughtExceptionHandler`, `signalhandler`, `mach`, `onerror`, `unhandledrejection`, `ANR`, … |

A mobile / native crash is both `fatal` and unhandled; a JavaScript
unhandled error is unhandled at level `error`. Statistics, the spike
alert, the event filter and the sampling keep factor all count
**unhandled** (`handled = false`). There is no separate "crash" notion on
events — the word is reserved for sessions.

## Session: release health

| Term | SDK source | Meaning |
|---|---|---|
| **Session status** | `session.status` | `exited`, `crashed`, `errored`, `abnormal` (`ok` while running) |
| **Crashed sessions** | `status = crashed` | Sessions that ended in a crash |
| **Crash-free rate** | computed | `1 − crashed ÷ total`, per release |
| **Adoption** | computed | A release's share of the sessions in the window |

## Issue status

| Status | Meaning |
|---|---|
| **unresolved** | New, not yet reviewed |
| **triaged** | Acknowledged, being investigated |
| **resolved** | Fixed (records the releases it had been seen on) |
| **regression** | Was resolved, reappeared on a release it had not been seen on before — only ingest sets it |
| **ignored** | Known, won't fix |

## Event fields

| Column / field | Sentry SDK source | Example |
|---|---|---|
| `level` | `event.level` | fatal |
| `message` | `logentry.formatted` / `message` / exception value | Attempt to invoke virtual method |
| `platform` | `event.platform` | android |
| `release` | `event.release` (`contexts.app.app_version`) | 2.4.1 |
| `environment` | `event.environment` | production |
| `transaction` | `event.transaction` | CartFragment |
| `error_type` | `exception.values[].type` (the primary exception) | NullPointerException |
| `culprit` | computed: innermost in-app frame | CartFragment.java:142 |
| `handled` | `exception.mechanism.handled` | false |
| `device_model` | `contexts.device.model` | Pixel 8 |
| `os_version` | `contexts.os.version` | 14 |
| `user_id` | `user.id` | user-001 |
| `device_id` | `tags.device_id` | did-abc-123 |
| `sdk_name` | `sdk.name` | sentry.java.android |

Issue title is Sentry's `Type: value`; the culprit and transaction are
shown under it, not in it.

## Alerts

| Type | Fires when |
|---|---|
| `new_issue` | A fingerprint is seen for the first time |
| `regression` | A resolved issue comes back on a new release |
| `unhandled_spike` | Unhandled errors in the last hour are 3× the 24 h baseline (and at least 10) |

## Branding

| ✅ Use | ❌ Don't use |
|---|---|
| CrashCart | ~~Trace Sentry~~ |
| Sentry SDK compatible / works with Sentry SDKs | ~~Sentry backend~~ ~~Sentry replacement~~ |
