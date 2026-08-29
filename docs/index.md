---
layout: home

hero:
  name: CrashCart
  text: Open-source error tracking
  tagline: Point any Sentry SDK at CrashCart and it receives crashes, errors, messages and release-health sessions. One Go binary, one Postgres, nothing else.
  actions:
    - theme: brand
      text: Get started
      link: /guide/getting-started
    - theme: alt
      text: What is CrashCart?
      link: /guide/introduction
    - theme: alt
      text: GitHub
      link: https://github.com/crashcartapp/crashcart

features:
  - icon: 🔌
    title: Sentry SDK compatible
    details: Keep the SDKs you already ship — iOS, Android, Flutter, React Native, web and backend. CrashCart speaks the envelope protocol; the viewer, API and data model are its own.
  - icon: 📦
    title: One binary, one database
    details: A single stateless Go binary and Postgres. TimescaleDB is optional; Neon, Supabase or RDS work too. Run it with Docker Compose in a minute.
  - icon: 🧭
    title: Issue-centric viewer
    details: Overview, issues grouped by fingerprint, stack traces, breakdowns by release / device / OS, release health with crash-free rates. Server-rendered, keyboard triage.
  - icon: 🧩
    title: Symbolication built in
    details: ProGuard / R8 and source maps resolve in-process. dSYMs through an optional sidecar. Upload with curl or sentry-cli.
  - icon: 🔔
    title: Alerts
    details: New issue, regression and crash-spike detectors per project, delivered to webhooks or Telegram.
  - icon: 🔁
    title: Portable data
    details: A versioned NDJSON export format shared by every CrashCart implementation. Back up, restore, or move between the Go edition and the serverless edition on Cloudflare Workers + D1.
---
