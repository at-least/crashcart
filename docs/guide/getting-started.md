# Getting started

This page takes you from nothing to a crash in the viewer on your own
machine. For a real server, follow [Docker Compose on a VPS](/deploy/docker)
or [Go binary + systemd](/deploy/binary) instead.

## 1. Run CrashCart

```sh
git clone https://github.com/crashcartapp/crashcart
cd crashcart
docker compose up -d
```

This starts TimescaleDB and CrashCart on `http://localhost:8080`.
Migrations run automatically at startup.

## 2. Create a project

A project is one DSN. Create one per app *and* platform:

```sh
docker compose exec crashcart /crashcart project shop-ios "Shop app (iOS)" ios
# project shop-ios (id 1)
# DSN: http://<key>@localhost:8080/1
```

The platform argument is optional and one of `ios`, `android`, `flutter`,
`react-native`, `web`, `backend`, `other`. It is a label for the viewer,
not a filter — see [Projects & DSNs](./projects).

You can also create projects from the viewer's home page.

## 3. Point an SDK at it

Paste the DSN into the SDK exactly as you would a Sentry DSN:

::: code-group

```swift [iOS]
SentrySDK.start { options in
    options.dsn = "http://<key>@localhost:8080/1"
}
```

```kotlin [Android]
// AndroidManifest.xml
<meta-data android:name="io.sentry.dsn" android:value="http://<key>@localhost:8080/1" />
```

```js [Browser / Node]
Sentry.init({ dsn: "http://<key>@localhost:8080/1" });
```

```python [Python]
sentry_sdk.init(dsn="http://<key>@localhost:8080/1")
```

:::

More platforms in [Connect an SDK](./sdks).

## 4. Send something

Throw an error, or use the SDK's `captureMessage`. Then open
<a href="http://localhost:8080" target="_blank">localhost:8080</a> — the
event appears as an issue within a second.

No SDK handy? Seed a week of demo data into a project called `demo`:

```sh
docker compose exec crashcart /crashcart seed
```

## What next

- [The viewer](./viewer) — overview, issues, events, releases, settings.
- [Symbolication](./symbolication) — upload ProGuard mappings, source maps
  or dSYMs so stack traces show real file names and lines.
- [Alerts](./alerts) — get told about new issues and crash spikes.
- [Which edition?](/deploy/which-edition) — then install on a server or
  on Cloudflare, with HTTPS, API keys and a viewer password.
