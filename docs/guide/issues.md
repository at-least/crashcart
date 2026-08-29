# Issues

An **issue** is a group of events with the same stack trace. Issues are
what you triage; the events underneath are the evidence.

## How events become issues

Events are grouped by their stack trace — the frames from your own code,
ignoring SDK and system frames. Events without a stack trace (messages)
group by type and text.

Obfuscated and readable stacks look different, so an issue can be split in
two until a [symbol file](./symbolication) arrives. CrashCart
re-symbolicates recent events when you upload one and merges the issue.
Upload mappings when you ship a release, not after the crashes come in.

## Status

| Status | Meaning |
|---|---|
| **Unresolved** | New, nobody has looked at it |
| **Triaged** | Acknowledged, someone is on it |
| **Resolved** | Fixed. CrashCart remembers which releases it had been seen on |
| **Regression** | Was resolved, then seen again on a release it had *never* been seen on |
| **Ignored** | Known, won't fix. Hidden from the default list |

A resolved issue that keeps crashing on **releases it was already known
on** stays resolved — old builds in the field aren't a regression. It
becomes a regression when it shows up on a release it had never been
seen on before you resolved it: the one that was supposed to carry the
fix.

Change status on the issue page, in bulk from the list, or with the
[keyboard](./viewer#keyboard-triage).

## What an issue shows

- **Title** — error type and where in your code it happened, e.g.
  `NullPointerException at CartFragment.java:142`
- **Events** — exact count, including events dropped by
  [sampling](./projects#sampling-and-daily-quota)
- **Users** — how many distinct users hit it
- **First / last seen**, and the **release** each happened on
- **Stack trace** of the latest event, symbolicated when possible
- **Breakdown** by release, device, OS version and environment
- **Events** — the individual occurrences, with breadcrumbs, tags and user

## How long issues are kept

Raw events are deleted after [`RETENTION_DAYS`](/deploy/configuration)
(30 by default). The issue itself — its status, counts and history — is
kept, so an old resolved issue can still be detected as a regression
months later.
