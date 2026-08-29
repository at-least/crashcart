# Connect an SDK

CrashCart speaks the Sentry envelope protocol, so setup is the Sentry SDK's
own setup with a CrashCart DSN. Nothing CrashCart-specific is needed in the
client.

The snippets below show the minimum. Two options are worth setting on every
platform:

- **`release`** — CrashCart groups release health and symbol files by
  release. Most SDKs read it from the app bundle automatically; set it
  explicitly when they don't.
- **`environment`** — `production`, `staging`, … Shown as a filter in the
  viewer.

Replace `DSN` with the project's DSN
(`http://<key>@crashcart.example.com/<id>`).

## Mobile

::: code-group

```swift [iOS (sentry-cocoa)]
import Sentry

SentrySDK.start { options in
    options.dsn = "DSN"
    options.environment = "production"
    // release defaults to bundle id @ version + build
}
```

```kotlin [Android (sentry-android)]
// AndroidManifest.xml
<application>
  <meta-data android:name="io.sentry.dsn" android:value="DSN" />
  <meta-data android:name="io.sentry.environment" android:value="production" />
</application>
```

```dart [Flutter (sentry-dart)]
await SentryFlutter.init(
  (options) {
    options.dsn = 'DSN';
    options.environment = 'production';
  },
  appRunner: () => runApp(const MyApp()),
);
```

```js [React Native]
import * as Sentry from '@sentry/react-native';

Sentry.init({ dsn: 'DSN', environment: 'production' });
```

:::

iOS note: ingest and grouping are verified from captured sentry-cocoa
envelopes, not yet with a real device end to end — see
[SDK compatibility](/reference/sdks).

For Android, the
[Sentry Android Gradle plugin](https://docs.sentry.io/platforms/android/configuration/gradle/)
uploads ProGuard / R8 mappings automatically — see
[Symbolication](./symbolication#proguard--r8-android) for the `sentry.properties`
it needs.

## Web

```js
import * as Sentry from '@sentry/browser';

Sentry.init({
  dsn: 'DSN',
  release: 'shop-web@2.4.1',   // must match the source map upload
  environment: 'production',
});
```

CrashCart answers CORS preflight for ingest; set
[`CORS_ORIGIN`](/deploy/configuration) to your site's origin instead of the
default `*` in production.

## Backend

::: code-group

```js [Node / Bun]
import * as Sentry from '@sentry/node';   // or @sentry/bun

Sentry.init({ dsn: 'DSN', release: process.env.GIT_SHA });
```

```python [Python]
import sentry_sdk

sentry_sdk.init(dsn="DSN", release="api@2.4.1", environment="production")
```

```go [Go]
import "github.com/getsentry/sentry-go"

sentry.Init(sentry.ClientOptions{Dsn: "DSN", Release: "api@2.4.1"})
defer sentry.Flush(2 * time.Second)
```

```rust [Rust]
let _guard = sentry::init(("DSN", sentry::ClientOptions {
    release: sentry::release_name!(),
    ..Default::default()
}));
```

```java [Java]
Sentry.init(options -> {
  options.setDsn("DSN");
  options.setRelease("api@2.4.1");
});
```

```csharp [.NET]
SentrySdk.Init(o => {
  o.Dsn = "DSN";
  o.Release = "api@2.4.1";
});
```

:::

## What the SDK sends that CrashCart uses

| Field | SDK source | Shown as |
|---|---|---|
| `level` | `event.level` | fatal / error / warning / info / debug |
| `message` | `logentry.formatted` or exception value | Issue title |
| `release` | `event.release` / `contexts.app.app_version` | Release, release health |
| `environment` | `event.environment` | Filter |
| `error_type` | `exception.values[0].type` | Issue title |
| `error_location` | computed: deepest in-app frame | `CartFragment.java:142` |
| `device_model`, `os_version` | `contexts.device`, `contexts.os` | Breakdown |
| `user.id` | `user.id` | Filter, affected users |
| `handled` | `exception.mechanism.handled` | `false` = crash |
| breadcrumbs, tags, extra | as sent | Event detail |

Sessions (`session` / `sessions` items) feed
[release health](./releases). Transactions, profiles, replays and client
reports are accepted and discarded.

Set [`PII_REDACT=true`](/deploy/configuration) to scrub emails, phone
numbers, tokens and user ids before events are stored.

## Verifying the connection

1. Send a test message: `Sentry.captureMessage("hello crashcart")` (every
   SDK has an equivalent).
2. Open the project in the viewer. A message event shows up under
   **Events**; exceptions also appear under **Issues**.
3. If nothing arrives, check the SDK's debug output for the HTTP status
   from `/api/<id>/envelope/`: `401` is a wrong key, `404` a wrong project
   id, `429` the [rate limit](/deploy/configuration).

The [compatibility matrix](/reference/sdks) lists the SDK versions
exercised end to end with real clients.
