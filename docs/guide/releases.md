# Releases & release health

A **release** is the app version the SDK reports. You never create
releases in CrashCart — one appears when the first event or session for
it arrives.

Set the release in the SDK if it doesn't pick it up automatically (mobile
SDKs read the app version; web and backend SDKs usually need
`release: "shop-web@2.4.1"`). The same string is used when you
[upload source maps](./symbolication), so keep it consistent.

## Crash-free rate

Sentry SDKs report **sessions** and whether each ended normally or in a
crash. What counts as one session depends on the SDK: mobile and browser
SDKs report one per app launch or page load, live for as long as the app
runs; backend SDKs typically report one per request-response cycle
instead, batched into periodic count buckets rather than one envelope
per request. CrashCart treats both the same way — from these it shows:

- the **crash-free rate** of the latest release on the overview
- every release in the window under **Releases**, with sessions, crash-free
  rate and how many issues it introduced
- one release's health day by day on its own page

For a backend project this reads as *the share of requests that did not
end in an unhandled exception reaching the framework's error handler* —
not whether the server process itself stayed alive. That is a far more
common event than a process crash, so don't expect it to sit at 100%: an
occasional uncaught exception on one endpoint is exactly what this
number is meant to surface, release over release.

Whether your SDK sends sessions by default, and how to turn request-mode
tracking on or off, varies by language and framework integration — check
your SDK's own release health / session tracking docs.

## What did this release break?

The release page lists **issues introduced** — issues first seen on this
release — and **issues present** — everything still occurring on it.
That's the quickest answer after a rollout.

## Regressions

Resolving an issue records the releases it had been seen on. If the
issue is seen again on a release *outside* that set — one that should
have carried the fix — it comes back as a [regression](./issues#status)
and, if enabled, triggers a [regression alert](./alerts). Old builds
still crashing on a known release are not a regression.
