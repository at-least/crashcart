# CrashCart — Glossary

The vocabulary is Sentry's: what the SDK sends is named the way the
Sentry protocol and UI name it, so nothing has to be translated in either
direction. Use these terms consistently across code, docs and the viewer;
do not coin synonyms.

This is a vocabulary, not a specification. How each value is computed
from an envelope is defined in `internal/sentry` (`analyze.go`:
`Fingerprint`, `Culprit`, the main-exception and level rules;
`envelope.go`: the event / session / attachment fields), and the columns
in `internal/db/schema.sql`.

## Core concepts

| Term | Meaning | NOT this |
|---|---|---|
| **Event** | One error, message or crash report sent by an SDK (an envelope `event` item) | ~~log entry~~ ~~record~~ |
| **Issue** | A group of events with the same fingerprint; the unit of triage | ~~error group~~ ~~bug~~ |
| **Fingerprint** | The grouping hash (`sentry.Fingerprint`; an SDK-supplied `fingerprint` wins) | ~~signature~~ |
| **Release** | The version the SDK reports (`event.release`) | ~~app version~~ ~~build~~ |
| **Environment** | `event.environment`: production / staging / development | ~~env~~ ~~profile~~ |
| **Platform** | The SDK family an event is folded into (`sentry.Families`) | ~~OS~~ ~~device type~~ |
| **Transaction** | `event.transaction`: the screen, route or request the event happened in | ~~screen~~ ~~page~~ |
| **Culprit** | Sentry's stack culprit, `module-or-file in function` (`sentry.Culprit`); shown under the title | ~~location~~ |
| **Session** | One app run / page load reported for release health (`session` / `sessions` items) | |
| **Attachment** | A file the SDK attached to an event (an envelope `attachment` item): a screenshot, a view hierarchy, a log | ~~upload~~ ~~asset~~ |
| **User Feedback** | A user-typed name/email/comments about a crash (an envelope `user_report` item), tied to one event | ~~user report~~ (only as the wire item type) ~~feedback~~ (Sentry's newer, session-replay-linked item; not accepted) |
| **Monitor** | A named, scheduled job CrashCart expects to hear from (an envelope `check_in` item's `monitor_slug` and `monitor_config`); created only by the SDK's own upsert, never by hand | ~~cron~~ ~~schedule~~ |
| **Check-in** | One run of a monitor: `in_progress` when it starts, `ok`/`error` when it ends, or `missed`/`timeout` when CrashCart notices it didn't (the `checkin_status` enum) | |

## Event: level and handled

Two independent facts, as the SDK sends them:

| Term | SDK source | Meaning |
|---|---|---|
| **Level** | `event.level` | Severity: `fatal`, `error`, `warning`, `info`, `debug` (the `event_level` enum) |
| **Unhandled** | `exception.mechanism.handled = false` | The SDK caught the error in a last-resort handler — a crash, an uncaught exception, an unhandled rejection. Sentry's "Unhandled" tag |
| **Handled** | `exception.mechanism.handled = true` | The app caught it and called `captureException`. No mechanism at all: neither badge, as in Sentry |
| **Mechanism** | `exception.mechanism.type` | How it was caught: `UncaughtExceptionHandler`, `signalhandler`, `onerror`, `unhandledrejection`, `ANR`, … |

A mobile / native crash is both `fatal` and unhandled; a JavaScript
unhandled error is unhandled at level `error`. Statistics, the spike
alert, the event filter and the sampling keep factor all count
**unhandled**. There is no separate "crash" notion on events — the word
is reserved for sessions.

## Session: release health

| Term | Meaning |
|---|---|
| **Session status** | `session.status` as sent: `ok`, `exited`, `crashed`, `errored`, `abnormal` (the `session_status` enum); **errored** = did not crash but reported errors |
| **Crashed sessions** | Sessions that ended in a crash |
| **Crash-free rate** | Share of sessions that did not crash, per release |
| **Adoption** | A release's share of the sessions in the window |

There are no per-user numbers (crash-free *users*, affected users) — by
decision; sessions only.

## Issue status

Sentry's statuses (`issue_status` enum), without the substatuses
(`new` / `ongoing` / `escalating`) and without a "triaged" state:

| Status | Meaning |
|---|---|
| **unresolved** | Open |
| **resolved** | Fixed (records the releases it had been seen on) |
| **regression** | Was resolved, reappeared on a release it had not been seen on before — only ingest sets it (Sentry's *Regressed*, "resolve in next release") |
| **ignored** | Known, won't fix — for good, or **until** a condition (Sentry's *Archived* / "Archive until …"): a time, a number of further events, or **escalating** |

The conditions are the `ignore_*` columns on `issues`, lifted by
`alerts.CheckIgnored`; the viewer's plain **Ignore** means "until
escalating" (Sentry's default *Archive*), the API's plain `ignored` means
for good.

## Event fields

Names as the SDK sends them; the mapping to columns is `internal/sentry`
(`Event`) and `ingest`. Two CrashCart conventions on top of Sentry's:

| Field | Convention |
|---|---|
| `device_id` | the `tags.device_id` tag — the SDKs send no device id (`sentry.Event.DeviceID`) |
| `regression` | CrashCart's own alert rule (Sentry alerts on regressions through workflow rules) |

Issue title is Sentry's `Type: value`; the culprit and transaction are
shown under it, not in it.

## Alerts

The rule types are the `alert_type` enum (`new_issue`, `regression`,
`unhandled_spike`, `escalating`, `monitor_failed`, `monitor_recovered`);
what each fires on is `internal/alerts/alerts.go` (`IsSpike` for the two
spike-shaped ones; the monitor pair in `internal/alerts/monitors.go`).

## Branding

| ✅ Use | ❌ Don't use |
|---|---|
| CrashCart | ~~Trace Sentry~~ |
| Sentry SDK compatible / works with Sentry SDKs | ~~Sentry backend~~ ~~Sentry replacement~~ |
