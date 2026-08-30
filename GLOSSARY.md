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
| **Fingerprint** | Hash of the exception chain and the in-app stack used for grouping (an SDK `fingerprint` wins) | ~~signature~~ |
| **Release** | The version the SDK reports (`event.release`) | ~~app version~~ ~~build~~ |
| **Environment** | `event.environment`: production / staging / development | ~~env~~ ~~profile~~ |
| **Platform** | SDK family: ios / android / javascript / node / python … | ~~OS~~ ~~device type~~ |
| **Transaction** | `event.transaction`: the screen, route or request the event happened in | ~~screen~~ ~~page~~ |
| **Culprit** | Sentry's stack culprit: the innermost in-app frame as `module-or-file in function` (shown under the title) | ~~location~~ |
| **Session** | One app run / page load reported for release health (`session` / `sessions` items) | |
| **Attachment** | A file the SDK attached to an event (an envelope `attachment` item): a crash screenshot (`screenshot.png`), a view hierarchy, a log | ~~upload~~ ~~asset~~ |

## Event: level and handled

Two independent facts, as the SDK sends them:

| Term | SDK source | Meaning |
|---|---|---|
| **Level** | `event.level` | Severity: `fatal` (the process died), `error`, `warning`, `info`, `debug` |
| **Unhandled** | `exception.mechanism.handled = false` | The SDK caught the error in a last-resort handler — a crash, an uncaught exception, an unhandled rejection. Sentry's "Unhandled" tag |
| **Handled** | `exception.mechanism.handled = true` | The app caught it and called `captureException`. No mechanism at all: neither badge, as in Sentry |
| **Mechanism** | `exception.mechanism.type` | How it was caught: `UncaughtExceptionHandler`, `signalhandler`, `mach`, `onerror`, `unhandledrejection`, `ANR`, … |

A mobile / native crash is both `fatal` and unhandled; a JavaScript
unhandled error is unhandled at level `error`. Statistics, the spike
alert, the event filter and the sampling keep factor all count
**unhandled** (`handled = false`). There is no separate "crash" notion on
events — the word is reserved for sessions.

## Session: release health

| Term | SDK source | Meaning |
|---|---|---|
| **Session status** | `session.status` (+ `errors`) | `exited`, `crashed`, `abnormal` (`ok` while running); **errored** = did not crash but `errors > 0` |
| **Crashed sessions** | `status = crashed` | Sessions that ended in a crash |
| **Crash-free rate** | computed | `1 − crashed ÷ total`, per release |
| **Adoption** | computed | A release's share of the sessions in the window |

## Issue status

| Status | Meaning |
|---|---|
| **unresolved** | Open |
| **resolved** | Fixed (records the releases it had been seen on) |
| **regression** | Was resolved, reappeared on a release it had not been seen on before — only ingest sets it (Sentry's *Regressed*, under "resolve in next release" rules) |
| **ignored** | Known, won't fix — for good, or **until** a condition (Sentry's *Archived* / "Archive until …"): a time, a number of further events, or **escalating** |

Sentry's statuses, without the substatuses (`new` / `ongoing` /
`escalating`); there is no "triaged" state. An issue's `level` is its
latest event's.

| Ignore condition | Column | Comes back to unresolved when |
|---|---|---|
| **until a time** | `ignore_until` | the time has passed |
| **until N more events** | `ignore_until_count` (= `event_count` + N at ignore time) | `event_count` reaches it |
| **until escalating** | `ignore_until_escalating`, `ignore_baseline` | its stored events in the last hour are 3× its hourly rate of the 24 h before it was ignored, and at least 10 — the `unhandled_spike` rule applied to one issue. Fires the `escalating` alert |

Checked every minute (`alerts.CheckIgnored`). The viewer's plain
**Ignore** is "until escalating" (Sentry's default *Archive*); the API's
plain `{"status": "ignored"}` is for good.

## Event fields

| Column / field | Sentry SDK source | Example |
|---|---|---|
| `level` | `event.level` | fatal |
| `message` | `logentry.formatted` / `message`, else `Type: value` of the main exception | Attempt to invoke virtual method |
| `platform` | `event.platform` | android |
| `release` | `event.release` only | 2.4.1 |
| `environment` | `event.environment` | production |
| `transaction` | `event.transaction` | CartFragment |
| `error_type` | the main exception's `type`: the one thrown last (`values[-1]`, or the one without a `parent_id`); its causes are shown under it | NullPointerException |
| `culprit` | computed: innermost in-app frame, `module-or-file in function` | com.example.CartFragment in onCreateView |
| `handled` | `exception.mechanism.handled` (null without a mechanism) | false |
| `device_model` | `contexts.device.model` only | Pixel 8 |
| `os_version` | `contexts.os.version` only | 14 |
| `user_id` | `user.id` | user-001 |
| `device_id` | `tags.device_id` — a CrashCart convention, the SDKs send no device id | did-abc-123 |
| `sdk_name` | `sdk.name` | sentry.java.android |
| `tags.server_name` | `server_name` (Sentry makes it a tag) | web-1 |

Issue title is Sentry's `Type: value`; the culprit and transaction are
shown under it, not in it.

## Alerts

| Type | Fires when |
|---|---|
| `new_issue` | A fingerprint is seen for the first time |
| `regression` | A resolved issue comes back on a new release |
| `unhandled_spike` | Unhandled errors in the last hour are 3× the 24 h baseline (and at least 10) |
| `escalating` | An issue ignored until escalating came back: its events in the last hour are 3× its rate of the 24 h before it was ignored (and at least 10) |

## Branding

| ✅ Use | ❌ Don't use |
|---|---|
| CrashCart | ~~Trace Sentry~~ |
| Sentry SDK compatible / works with Sentry SDKs | ~~Sentry backend~~ ~~Sentry replacement~~ |
