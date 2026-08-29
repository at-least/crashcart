---
layout: home

hero:
  name: CrashCart
  text: Open-source error tracking
  tagline: Point any Sentry SDK at CrashCart and it receives crashes, errors, messages and release-health sessions. One Go binary and a Postgres — free for small apps, cheap at scale.
  actions:
    - theme: brand
      text: Get started
      link: /guide/getting-started
    - theme: alt
      text: Compared to Sentry
      link: /guide/compared-to-sentry
    - theme: alt
      text: Install
      link: /deploy/docker
    - theme: alt
      text: GitHub
      link: https://github.com/crashcartapp/crashcart

features:
  - icon: 🔌
    title: Sentry SDK compatible
    details: Keep the SDKs you already ship — iOS, Android, Flutter, React Native, web and backend. CrashCart speaks the envelope protocol; the viewer, API and data model are its own.
  - icon: 📦
    title: Run it your way
    details: One Go binary and any Postgres — Docker Compose on a VPS, Kubernetes, or your cloud's managed database. Nothing else to run.
  - icon: 🧭
    title: Issue-centric viewer
    details: Overview, issues grouped by fingerprint, stack traces, breakdowns by release / device / OS, release health with crash-free rates. Server-rendered, keyboard triage.
  - icon: 🧩
    title: Symbolication built in
    details: ProGuard / R8 and source maps resolve in-process. dSYMs through an optional sidecar. Upload with curl or sentry-cli.
  - icon: 🔔
    title: Alerts
    details: New issue, regression and crash-spike detectors per project, delivered to webhooks or Telegram.
  - icon: 🔒
    title: Your data, your server
    details: Events stay in your Postgres. No telemetry, no outbound calls. Optional PII scrubbing, retention you set, and a plain-text export that restores into any CrashCart.
---
