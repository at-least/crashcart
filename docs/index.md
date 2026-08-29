---
layout: home

hero:
  name: CrashCart
  text: Open-source error tracking
  tagline: Point any Sentry SDK at CrashCart and it receives crashes, errors, messages and release-health sessions. One Go binary, one Postgres.
  actions:
    - theme: brand
      text: Get started
      link: /guide/getting-started
    - theme: alt
      text: View on GitHub
      link: https://github.com/shadlessdev/crashcart

features:
  - title: Sentry SDK compatible
    details: Reuse the SDKs you already ship. CrashCart speaks the envelope protocol; the viewer, API and data model are its own.
  - title: One binary, one database
    details: A single Go binary and Postgres (TimescaleDB optional). Nothing else to run.
  - title: Portable data
    details: Every implementation reads and writes the same NDJSON export format, so your data is never locked in.
---
