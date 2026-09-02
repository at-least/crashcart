# Issues

An **issue** is a group of events with the same stack trace. Issues are
what you triage; the events underneath are the evidence.

## How events become issues

Events are grouped by their exception chain and the stack frames from
your own code, ignoring SDK and system frames. Events without a stack
trace (messages) group by type and text. An SDK-set `fingerprint` wins.

Obfuscated and readable stacks look different, so an issue can be split in
two until a [symbol file](./symbolication) arrives. CrashCart
re-symbolicates recent events when you upload one and merges the issue.
Upload mappings when you ship a release, not after the crashes come in.

## Status

| Status | Meaning |
|---|---|
| **Unresolved** | Open |
| **Resolved** | Fixed. CrashCart remembers which releases it had been seen on |
| **Regression** | Was resolved, then seen again on a release it had *never* been seen on (Sentry's *Regressed*) |
| **Ignored** | Known, won't fix — for good or *until* something: a time, a number of further events, or the issue escalating. Hidden from the default list (Sentry's *Archived*) |

These are Sentry's statuses; resolving works like Sentry's "resolve in
next release":

a resolved issue that keeps crashing on **releases it was already known
on** stays resolved — old builds in the field aren't a regression. It
becomes a regression when it shows up on a release it had never been
seen on before you resolved it: the one that was supposed to carry the
fix.

Change status on the issue page, in bulk from the list, or with the
[keyboard](./viewer#keyboard-triage).

### Ignoring with a condition

Ignoring hides an issue; the condition says when it should come back to
**Unresolved** on its own, like Sentry's "Archive until …":

| Ignore … | Comes back when |
|---|---|
| **until escalating** (the default) | its rate in the last hour is well above what it was in the day before you ignored it — the same rule as the [unhandled-error spike](./alerts), applied to this one issue. That also sends an [*escalating* alert](./alerts) |
| **for a number of days** | the time has passed |
| **until N more events** | that many more events have arrived (counting sampled-out ones) |
| **forever** | never |

Pick it in the status control on the issue page ("Ignored until
escalating", "Ignored for 7 days", …) or next to the **Ignore** button
in the list. The condition is shown under the status; from the
[API](/reference/api#issues) it is `ignore_minutes`, `ignore_events` and
`ignore_until_escalating` — any combination, or none for good.

## What an issue shows

- **Title** — Sentry's `Type: value` of the exception thrown last, e.g.
  `NullPointerException: Attempt to invoke virtual method`, with the
  culprit (`com.example.CartFragment in onCreateView`) and the
  transaction under it; the causes are listed in the stack trace
- **Events** — exact count, including events dropped by
  [sampling](./projects#sampling-and-daily-quota)
- **Users** — how many distinct users hit it
- **First / last seen**, and the **release** each happened on
- **Stack trace** of the latest event, symbolicated when possible, and its
  screenshot when the SDK attached one
- **Breakdown** by release, device, OS version and environment
- **Events** — the individual occurrences, with breadcrumbs, tags and user

## How long issues are kept

Raw events are deleted after [`RETENTION_DAYS`](/deploy/configuration).
The issue itself — its status, counts and history — is kept longer, so
an old resolved issue can still be detected as a regression months
later.
