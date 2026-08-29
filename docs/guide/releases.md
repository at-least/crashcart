# Releases & release health

A **release** is the app version the SDK reports. You never create
releases in CrashCart — one appears when the first event or session for
it arrives.

Set the release in the SDK if it doesn't pick it up automatically (mobile
SDKs read the app version; web and backend SDKs usually need
`release: "shop-web@2.4.1"`). The same string is used when you
[upload source maps](./symbolication), so keep it consistent.

## Crash-free rate

Sentry SDKs report **sessions** — one per app launch or page load — and
whether each ended normally or in a crash. From those CrashCart shows:

- the **crash-free rate** of the latest release on the overview
- every release in the window under **Releases**, with sessions, crash-free
  rate and how many issues it introduced
- one release's health day by day on its own page

Session tracking is on by default in the mobile and browser SDKs. Backend
SDKs usually have it off; enable it if you want release health there.

## What did this release break?

The release page lists **issues introduced** — issues first seen on this
release — and **issues present** — everything still occurring on it.
That's the quickest answer after a rollout.

## Regressions

Resolving an issue records the release. If the issue is seen again on a
*different* release, it comes back as a [regression](./issues#status) and,
if enabled, triggers a [regression alert](./alerts).
