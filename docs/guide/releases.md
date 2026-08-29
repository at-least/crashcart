# Releases & release health

A **release** is the app version or deployment identifier the SDK sends
(`event.release`, or `contexts.app.app_version` on mobile). CrashCart never
requires you to "create" a release — it appears when the first event or
session for it arrives.

## Release health

Sentry SDKs send **sessions**: one per app launch (mobile) or page load /
request batch, marked as exited, crashed, errored or abnormal. CrashCart
stores them and aggregates per release and day:

- **Crash-free sessions** — sessions that did not end in a crash
- **Errored sessions** — sessions with at least one handled error

The overview shows the **latest release** and its crash-free rate;
`/p/{slug}/releases` lists every release in the window;
`/p/{slug}/releases/{version}` shows one release's health over time, the
issues first seen on it, and its events.

Release health needs session tracking enabled in the SDK. It is on by
default in the mobile SDKs and `@sentry/browser`; backend SDKs usually have
it off (`auto_session_tracking` / `autoSessionTracking`).

## Releases and issues

- An issue records `first_release` and `last_release`.
- Resolving an issue records `resolved_release`; an event on any *other*
  release turns it into a [regression](./issues#lifecycle).
- The release page lists **new issues** — issues whose `first_release` is
  this one — which is the quickest answer to "what did this build break".

## Releases and symbol files

Source maps and ProGuard mappings uploaded through the API are tied to a
release and matched to events by it, so the `release` string in the SDK
must be exactly the one you upload under. Files uploaded through
`sentry-cli` are matched by `debug_id` instead and carry no release. See
[Symbolication](./symbolication).

## Crash spikes

The [crash-spike alert](./alerts#crash-spike) uses the project's crashes in
the last hour against the mean of the 24 hours before. Because that
baseline is per project, keep [one project per platform](./projects#one-project-per-platform)
so an Android incident is not diluted by healthy iOS traffic.
