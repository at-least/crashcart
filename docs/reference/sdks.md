# SDK compatibility

CrashCart is exercised end to end with real clients — not hand-built
envelopes. Each SDK below sent a message, a handled exception with user,
tags and breadcrumbs, and an unhandled crash through the SDK's own crash
path, and the result was checked in the viewer and the API.

| SDK | Version | Notes |
|---|---|---|
| sentry-python | 2.68 | gzip-compressed envelopes |
| @sentry/node | 10.72 | |
| @sentry/browser | 10.72 | Real Chromium, CORS + preflight |
| @sentry/bun | 10.72 | |
| sentry-go | 0.49 | Bare-array `exception` / `threads`; panics as message events |
| sentry-rust | 0.49 | `sentry_panic` frames filtered out of the location |
| sentry-java | 8.54 | Plain JVM, chained exceptions |
| sentry-android-core | 8.14 | API 35 emulator, crash-cache resend, R8 mapping with inlined frames |
| Sentry Android Gradle plugin | 4.14 | Automatic ProGuard mapping upload |
| sentry-dotnet | 6.9 | Exception without frames → thread stack |
| sentry-dart | 9.28 | Async placeholders and SDK frames excluded from grouping |
| sentry-native | master (inproc) | Address-only frames grouped by image + offset |
| sentry-cli | 3.7 | `debug-files upload`, `upload-proguard`, chunked upload |

## Expected to work, not yet verified

These speak the same envelope protocol and should work; they have not been
run against CrashCart with a real client yet:

- sentry-cocoa (iOS / macOS) — ingest and grouping work from captured
  envelopes; dSYM symbolication needs the sidecar, which needs a macOS
  build to test end to end
- sentry-react-native, sentry-flutter (built on the native SDKs above)
- Unity, Unreal, Godot
- Ruby, PHP, Elixir

If you run one of these, an issue or PR with what you saw is welcome.

## Protocol notes

- Envelopes and the legacy `/store/` endpoint are both accepted.
- `gzip` content encoding is supported.
- Envelopes up to 20 MB.
- Item types other than `event`, `session`, `sessions` are dropped without
  error, so enabling tracing or replay in the SDK is harmless — the data
  just isn't stored.
- Rate limiting answers `429`; SDKs honour it and resend cached crashes.
