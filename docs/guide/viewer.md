# The viewer

The viewer is the web UI at `/`. It is server-rendered (templ + htmx), keeps
all state in the URL, and is issue-centric: you move from an overview to a
list of issues, into one issue, then into individual events.

Protect it with [`VIEWER_PASSWORD`](/deploy/configuration) (HTTP basic
auth, any username) before exposing it.

## Home

`/` lists projects with their event counts and lets you create one.

## Overview

`/p/{slug}` — totals for the selected window, an events timeline, the
latest release with its crash-free rate, and the top issues. Charts are
inline SVG rendered by the server.

Every page carries the same filter bar: **time window**, **release**,
**environment**, and any [`CUSTOM_TAGS`](/deploy/configuration) you
configured. Filters live in the query string, so a filtered view is a
shareable link.

## Issues

`/p/{slug}/issues` — the issue list. Columns: title (error type + location),
level, event count, affected users, first and last seen, a sparkline of the
last 24 hours, and the first/last release.

Filter by **status** (`unresolved`, `triaged`, `resolved`, `regression`,
`ignored`), release, and free-text search (`q=` matches title and
location). Select rows to change status in bulk.

### Keyboard triage

| Key | Action |
|---|---|
| `j` / `k` | Next / previous issue |
| `r` | Mark resolved |
| `i` | Ignore |
| `t` | Mark triaged |
| `x` | Toggle selection |

A banner appears when new issues arrive while the page is open (server-sent
events from `/p/{slug}/stream`); it does not reload the list under you.

## Issue

`/p/{slug}/issues/{fingerprint}` — one issue:

- **Stack trace** of the latest event, symbolicated when symbol files are
  available; in-app frames highlighted, SDK and system frames collapsed.
- **Breakdown** by release, device model, OS version, environment.
- **Events** belonging to the issue, paged, with the same filters.
- **Status** control and the release the issue was resolved on.

Statuses and what they mean are in [Issues & grouping](./issues#lifecycle).

## Events

`/p/{slug}/events` — every stored event regardless of grouping, including
messages and non-exception events that are not part of any issue. Filters:
level, release, environment, platform, `user_id`, tags.

`/p/{slug}/events/{id}` — the full event: exception chain, symbolicated and
original frames, breadcrumbs, tags, user, contexts, and the raw payload.

## Releases

`/p/{slug}/releases` — releases seen in the window with sessions,
crash-free rate, new issues introduced, and first/last seen.
`/p/{slug}/releases/{version}` drills into one. See
[Releases & release health](./releases).

## Settings

`/p/{slug}/settings`:

- **DSN** and **Rotate key**
- **Platform** label
- **Sampling**: `sample_keep_first`, `sample_rate`, `daily_quota`
- **Alerts**: enable each detector, add webhook / Telegram channels
- **Symbols**: upload and delete symbol files

Everything the viewer can do, the [API](/reference/api) can do too; the
viewer is a client of the same store.

## Theme

Light and dark follow the OS setting; toggle from the header. The
preference is kept in the browser.
