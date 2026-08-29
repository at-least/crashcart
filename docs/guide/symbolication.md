# Symbolication

Production builds ship obfuscated (Android), minified (JavaScript) or
stripped (iOS) code. Symbolication maps the frames the SDK reports back to
source file, function and line.

| Symbol kind | Platforms | How CrashCart resolves it |
|---|---|---|
| ProGuard / R8 mapping | Android | In-process. Inline at ingest when the mapping is cached, otherwise by the job worker |
| Source map | JavaScript, React Native | In-process, matched by release |
| dSYM | iOS, macOS | Optional sidecar container running `llvm-symbolizer`; enable with `SYMBOLICATE_URL` |

Symbolicated frames are stored **beside** the original payload, which is
never rewritten; the viewer shows both. Symbolication also updates the
issue [fingerprint](./issues#symbolication-changes-fingerprints) and
`error_location`.

## Uploading

### With the API

```sh
curl -H "Authorization: Bearer $API_KEY" \
     -F kind=proguard -F release=2.4.1 -F file=@mapping.txt \
     https://crashcart.example.com/api/projects/shop-android/symbols
```

`kind` is `proguard`, `sourcemap` or `dsym`; `release` must equal the
release string the SDK sends. Files also upload from **Settings → Symbols**
in the viewer.

### With sentry-cli

CrashCart implements the debug-file upload endpoints `sentry-cli` uses (the
chunked upload protocol, plus the legacy `files/dsyms/` route older versions
fall back to). The organization is ignored; the project is the CrashCart
slug or numeric id.

```sh
export SENTRY_URL=https://crashcart.example.com
export SENTRY_AUTH_TOKEN=$API_KEY
export SENTRY_ORG=any
export SENTRY_PROJECT=shop-ios

sentry-cli debug-files upload path/to/App.dSYM
sentry-cli upload-proguard --write-properties sentry-debug-meta.properties mapping.txt
```

Files uploaded this way carry no release. They are matched by **debug id**
(`debug_meta.images` in the event: the dSYM's `LC_UUID`, or the id the
Gradle plugin writes into `sentry-debug-meta.properties`), which is more
robust than release matching.

Set [`PUBLIC_URL`](/deploy/configuration): the chunk-upload URL CrashCart
hands back to `sentry-cli` is derived from it and must be reachable from
where `sentry-cli` runs (your CI).

## ProGuard / R8 (Android)

The [Sentry Android Gradle plugin](https://docs.sentry.io/platforms/android/configuration/gradle/)
uploads the mapping at build time. Point it at CrashCart:

```properties
# sentry.properties
defaults.url=https://crashcart.example.com
defaults.org=any
defaults.project=shop-android
auth.token=<API key>
```

The plugin also injects the debug id into the APK, so events and mappings
match without a release string. Inlined frames from R8 are resolved.

## Source maps (JavaScript, React Native)

Upload each bundle's `.map` under the release the SDK reports:

```sh
curl -H "Authorization: Bearer $API_KEY" \
     -F kind=sourcemap -F release=shop-web@2.4.1 -F file=@dist/app.js.map \
     https://crashcart.example.com/api/projects/shop-web/symbols
```

Matching is by release and file name, so keep `release` identical in
`Sentry.init()` and the upload.

## dSYM (iOS, macOS)

dSYM symbolication runs in a sidecar container (`container/symbolicate`,
Python + `llvm-symbolizer`) because the tooling is heavy and not portable
into a Go binary.

Enable it in `docker-compose.yml` by uncommenting the `symbolicate` service
and setting `SYMBOLICATE_URL: http://symbolicate:8080` on `crashcart`.
Without the sidecar, iOS crashes are stored and grouped by image + offset;
they just stay unsymbolicated.

Upload with `sentry-cli debug-files upload` (above) or the API with
`kind=dsym`. For fat binaries the debug id is the arm64 slice's UUID.

## Re-symbolication

Uploading a symbol file re-queues the release's unsymbolicated events from
the last [`COMPRESS_AFTER`](/deploy/configuration) (48 h by default) — the
window before TimescaleDB compresses the chunk. Older events are not
touched; on plain Postgres there is no compression, but the same window
applies. The job worker processes the queue in the background.
