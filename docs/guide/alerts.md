# Alerts

Turn alerts on per project under **Settings → Alerts**, and add where they
should go.

| Alert | Fires when |
|---|---|
| **New issue** | An issue is seen for the first time |
| **Regression** | A resolved issue comes back on a different release |
| **Crash spike** | At least 10 crashes in the last hour, and 3× the usual hourly rate of the previous day |

Each alert type has a **cooldown** (minutes) so a noisy hour doesn't
produce a message every minute.

The cooldown is per project and alert type, so a deploy that breaks
several things produces one *new issue* alert; that alert carries
`more_since_last` (and the Telegram text says "+N more new issues since
the last alert"), so the ones it stands for are not invisible — the
issues list has them. An alert that could not be delivered to any channel
does not use up the cooldown: the next one goes out as soon as a channel
takes it.

## Where alerts go

**Webhook** — any URL. CrashCart POSTs JSON:

```json
{
  "type": "new_issue",
  "project": "Shop app (iOS)",
  "project_slug": "shop-ios",
  "title": "NSInvalidArgumentException at CartViewController.swift:88",
  "level": "fatal",
  "event_count": 1,
  "first_release": "2.4.1",
  "last_release": "2.4.1",
  "url": "https://crashcart.example.com/p/shop-ios/issues/5f1c…"
}
```

A crash-spike alert carries `recent` (crashes in the last hour) and
`baseline` (crashes in the 24 hours before) instead of the issue fields.
`url` links straight to the issue or project, using
[`PUBLIC_URL`](/deploy/configuration).

Slack, Discord and most chat tools accept incoming webhooks; put a small
relay in between if they need a specific message shape.

**Telegram** — create a bot with @BotFather, set its token as
`TELEGRAM_BOT_TOKEN` on the server, add the bot to a group, and enter that
group's chat id as the channel. Messages are plain text with a link.

Every enabled alert goes to every channel of the project. Failed
deliveries are retried automatically.

## Adding channels from the API

```sh
curl -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
     -d '{"kind":"webhook","config":{"url":"https://hooks.example.com/crashcart"}}' \
     https://crashcart.example.com/api/projects/shop-ios/alerts/channels
```

See the [API reference](/reference/api#alerts).
