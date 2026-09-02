# Symbolication

Production builds ship obfuscated (Android), minified (JavaScript) or
stripped (iOS) code. Upload the matching symbol file and CrashCart turns
`a.b.c.d` into `CartFragment.onResume`, with file names and line numbers.

| Symbol file | Platform | Needs |
|---|---|---|
| ProGuard / R8 mapping | Android | Nothing extra |
| Source map | JavaScript, React Native | Nothing extra |
| dSYM | iOS, macOS | The optional symbolication sidecar |

## Uploading

**In the viewer**: Settings → Symbols.

**With curl**:

```sh
curl -H "Authorization: Bearer $API_KEY" \
     -F kind=proguard -F release=2.4.1 -F file=@mapping.txt \
     https://crashcart.example.com/api/projects/shop-android/symbols
```

`kind` is `proguard`, `sourcemap` or `dsym`. `release` must be the release
string your SDK sends.

**With sentry-cli**, which most CI setups already use:

```sh
export SENTRY_URL=https://crashcart.example.com
export SENTRY_AUTH_TOKEN=$API_KEY
export SENTRY_ORG=any            # ignored
export SENTRY_PROJECT=shop-ios   # the CrashCart slug

sentry-cli debug-files upload path/to/App.dSYM
sentry-cli upload-proguard mapping.txt
```

Files uploaded through `sentry-cli` don't need a release — they are matched
to events by debug id, which is more robust. Make sure
[`PUBLIC_URL`](/deploy/configuration) is set so `sentry-cli` can reach the
upload URL from your CI.

## Android

The Sentry Android Gradle plugin uploads the mapping on every build. Point
it at CrashCart in `sentry.properties`:

```properties
defaults.url=https://crashcart.example.com
defaults.org=any
defaults.project=shop-android
auth.token=<API key>
```

## JavaScript / React Native

Upload each bundle's `.map` under the release the SDK reports:

```sh
curl -H "Authorization: Bearer $API_KEY" \
     -F kind=sourcemap -F release=shop-web@2.4.1 -F file=@dist/app.js.map \
     https://crashcart.example.com/api/projects/shop-web/symbols
```

Keep `release` identical in `Sentry.init()` and the upload.

## iOS / macOS

dSYM symbolication needs the sidecar: the same `crashcart` binary run as
`crashcart symbolicate` in a container that has `llvm-symbolizer`
(`container/symbolicate/Dockerfile`). With Docker Compose, uncomment the
`symbolicate` service in `docker-compose.yml` and set
`SYMBOLICATE_URL=http://symbolicate:8080` in `.env`; other installs run
that image next to CrashCart and set `SYMBOLICATE_URL` to its address.
Then upload dSYMs with `sentry-cli debug-files upload`.

The sidecar keeps the dSYMs it has used on disk (`SYMBOLICATE_CACHE_DIR`,
bounded by `SYMBOLICATE_CACHE_MAX_MB` — see
[Configuration](/deploy/configuration)), so a dSYM is fetched from the
database once, not per crash. The first crash of a build may show up
unsymbolicated for a moment and is fixed up in the background.

Without the sidecar, iOS crashes are still collected and grouped — the
frames just stay as addresses.

## Where symbol files are stored

In the database, by default — nothing else to run, one thing to back up.
A ProGuard mapping from a large app can run to hundreds of MB (uploads
are accepted up to 500 MB), and a file that size is a poor fit for a
database row: it costs write-ahead log on every upload and makes every
backup as heavy as the largest file. So symbol files alone can go
elsewhere:

| `BLOB_STORE` | Where | When |
|---|---|---|
| `postgres` (default) | the `symbol_files` table | small mappings, nothing extra to run |
| `s3` | an S3-compatible bucket (`S3_BUCKET`, `S3_ENDPOINT` for MinIO / R2 / Backblaze, `S3_ACCESS_KEY` + `S3_SECRET_KEY` or the usual AWS credential chain) | large mappings, or more than one replica |
| `fs` | a directory (`BLOB_DIR`) | one instance on one machine |

Variables: [Configuration](/deploy/configuration). The bucket must exist;
CrashCart checks it can reach it at startup and refuses to start
otherwise, rather than failing at the first upload.

Switching is safe at any time: each file is read from wherever it was
written, so files uploaded before the change stay where they are and new
ones go to the new place. To move the existing ones, `crashcart export`
then `crashcart import` into the new backend — the export file always
carries the bytes inline ([Operations](/deploy/operations#backups)).
Deleting a symbol file, a project, or expiring old files removes the
object with the row. Events, attachments and everything else stay in
Postgres whatever `BLOB_STORE` says.

## Already-collected events

Uploading a symbol file also symbolicates the release's most recent
stored events, which may regroup them into the right issue.
