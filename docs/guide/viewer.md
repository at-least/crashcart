# The viewer

The viewer is the web UI at `/`. Every view is a URL you can share, and
it is issue-centric: you move from an overview to a list of issues, into
one issue, then into individual events.

It needs an account: the first one is created on `/setup` when the
viewer is opened for the first time; more users and the API keys live on
**Account** (top right). See [Security](/deploy/security).

## Home

`/` lists projects — unhandled errors in the last day, open issues, the
latest release and its crash-free rate — and lets you create one.

## Overview

`/p/{slug}` — totals for the selected window, unhandled errors by
release over time, and the newest issues and regressions.

Every page carries the same filter bar: **time window**, **release**,
**environment**, and any [`CUSTOM_TAGS`](/deploy/configuration) you
configured. Filters live in the query string, so a filtered view is a
shareable link.

## Issues

`/p/{slug}/issues` — the issue list. Columns: title (with the culprit under it),
level, a sparkline of the last week, event count, first and last seen,
and the release.

Filter by **status** (`unresolved`, `resolved`, `regression`,
`ignored`), release, and free-text search (`q=` matches the title and
the exception type). Select rows to change status in bulk; the select
next to **Ignore** says [until when](./issues#ignoring-with-a-condition)
(until escalating by default).

### Keyboard triage

| Key | Action |
|---|---|
| `j` / `k` | Next / previous issue |
| `r` | Mark resolved |
| `i` | Ignore |
| `x` | Toggle selection |

A banner appears when new issues arrive while the page is open; it does
not reload the list under you.

## Issue

`/p/{slug}/issues/{fingerprint}` — one issue:

- **Stack trace** of the latest event, symbolicated when symbol files are
  available; in-app frames highlighted, SDK and system frames collapsed.
- **Breakdown** by release, device model, OS version, environment.
- **Events** belonging to the issue, paged, with the same filters.
- **Status** control — Unresolved, Resolved, or Ignored with a
  [condition](./issues#ignoring-with-a-condition) — and, for an ignored
  issue, when it comes back.
- The latest event's **screenshot**, when the SDK attached one.

Statuses and what they mean are in [Issues & grouping](./issues#status).

## Events

`/p/{slug}/events` — every stored event regardless of grouping, including
messages and non-exception events that are not part of any issue. Filters:
level, release, environment, platform, `user_id`, tags.

`/p/{slug}/events/{event_id}` — the full event: exception chain, symbolicated and
original frames, attachments (screenshots inline, other files as
downloads), breadcrumbs, tags, user, contexts, the raw payload, and a
user feedback report if one was submitted.

## Feedback

`/p/{slug}/feedback` — user feedback reports (name, email, comments a
person typed into the SDK's crash dialog), newest first, linking back
to the event when it's still there. A report is kept even if its event
was never stored — sampling, or the report simply arriving first — so
this list can hold entries the event list can't.

## Releases

`/p/{slug}/releases` — releases seen in the window with sessions,
crash-free rate, new issues introduced, and first/last seen.
`/p/{slug}/releases/{version}` drills into one. See
[Releases & release health](./releases).

## Monitors

`/p/{slug}/monitors` — Sentry's cron monitoring: every monitor an SDK's
crons integration has reported a schedule for, its last status and when
it's next expected. `/p/{slug}/monitors/{slug}` drills into one: its
recent check-ins and a **Delete** button. A monitor is created only by
the SDK's own `check_in` envelope item — there's no form here to add one
by hand.

## Settings

`/p/{slug}/settings`:

- **DSN** and **Rotate key**; **Previous keys** — DSN keys a rotation
  retired but nobody has deleted, with when each was last used
- **Platform** label
- **Sampling**: keep first, sample rate
- **Alerts**: enable each detector, add webhook / Slack / Telegram channels
- **Symbols**: upload and delete symbol files
- **Discarded events**: what the project's SDKs reported dropping
  client-side over the last week — by reason (sample rate, `before_send`,
  a full offline queue, …) and category — so a lower-than-expected event
  count has an answer other than "something is broken"

Everything the viewer can do, the [API](/reference/api) can do too; the
viewer is a client of the same store.

## Theme

Light and dark follow the OS setting; toggle from the header. The
preference is kept in the browser.
