# Projects & DSNs

A **project** is one DSN. Every SDK holding that DSN reports into it.

## Creating a project

From the viewer's home page, or from the command line:

```sh
crashcart project shop-ios "Shop app (iOS)" ios
# project shop-ios (id 1)
# DSN: https://<key>@crashcart.example.com/1
```

The last argument is the platform: `ios`, `android`, `flutter`,
`react-native`, `web`, `backend` or `other`. It is a label shown in the
viewer, not a filter.

The **slug** (`shop-ios`) is what you'll see in URLs and use in the CLI and
API.

## The DSN

```
https://<key>@crashcart.example.com/1
```

Paste it into the SDK exactly as you would a Sentry DSN. The key lets an
app send events to this project — it is fine to ship inside an app binary,
but don't publish it.

If the DSN shows the wrong host (for example `localhost` when CrashCart is
behind a domain), set [`PUBLIC_URL`](/deploy/configuration).

### Rotating the key

**Settings → Rotate key** in the viewer, or `crashcart rotate-key shop-ios`.
This issues a new *current* key without invalidating the old one — the
old key keeps authenticating, listed under **Previous keys** in
Settings (`crashcart project-keys list shop-ios` on the CLI), until you
delete it. Ship the new DSN with the next release, let old builds phase
out, then delete the old key once its "last used" stops advancing —
deletion itself takes effect within a few seconds (the ingest cache),
not instantly.

## One project per platform

Create one project per app **and** platform — `shop-ios`, `shop-android`,
`shop-web` — rather than one for the whole app. The "latest release" and
crash-spike detection are computed per project, and iOS `2.4.1` and
Android `2.4.1` are different builds with different crash rates. Mixing
them blurs both numbers.

## Sampling

A single bug can produce millions of identical events. Two settings under
**Settings → Sampling** keep storage under control without losing the
count (the page shows the current values and the defaults):

| Setting | Effect |
|---|---|
| Keep first | The first N events of every issue are always stored — more of them when they are unhandled (`exception.mechanism.handled = false`: crashes, uncaught exceptions) |
| Sample rate | After that, this fraction of the issue's events is stored (`1` = everything); events with nothing to group by use it from the start. Lower it on a busy project: what is stored then grows with the number of issues, not events, and the counts stay exact |

The issue's event count stays exact whether or not an event was stored.

A runaway client is bounded by `RATE_LIMIT` (requests per minute per DSN
key; see [Configuration](/deploy/configuration)) rather than a daily cap —
sampling is what keeps storage bounded.
