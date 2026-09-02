# Alerts

Turn alerts on per project under **Settings → Alerts**, and add where they
should go.

| Alert | Fires when |
|---|---|
| **New issue** | An issue is seen for the first time |
| **Regression** | A resolved issue comes back on a different release |
| **Unhandled error spike** | Unhandled errors (`exception.mechanism.handled = false`) in the last hour are well above the project's usual hourly rate of the previous day — and not just a handful |
| **Escalating** | An issue you [ignored until escalating](./issues#ignoring-with-a-condition) is back: the same spike rule applied to that issue's own events, against its rate in the day before you ignored it |

Each alert type has a **cooldown** (minutes, set under Settings → Alerts)
so a noisy hour doesn't produce a message after message.

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
  "title": "NSInvalidArgumentException: -[__NSArrayI objectAtIndex:]: index 3 beyond bounds",
  "level": "fatal",
  "event_count": 1,
  "first_release": "2.4.1",
  "last_release": "2.4.1",
  "url": "https://crashcart.example.com/p/shop-ios/issues/5f1c…"
}
```

An unhandled-spike alert (`"type": "unhandled_spike"`) carries `recent` (unhandled errors in the
last hour) and `baseline` (in the 24 hours before) instead of the issue fields.
An escalating alert (`"type": "escalating"`) carries the issue fields plus
`recent` (the issue's events in the last hour) and `baseline` (its events
in the 24 hours before it was ignored).
`url` links straight to the issue or project, using
[`PUBLIC_URL`](/deploy/configuration). The exact fields are in the
[API reference](/reference/api#alerts).

Discord and most other chat tools accept incoming webhooks too; put a
small relay in between if they need a specific message shape.

**Slack** — add an Incoming Webhook to your workspace and use its URL as
the channel. Unlike the generic **Webhook** above, this posts a plain
`text` message (mrkdwn: bold title, a linked issue) instead of the raw
JSON, so nothing else is needed in between.

**Telegram** — create a bot with @BotFather, set its token as
`TELEGRAM_BOT_TOKEN` on the server, add the bot to a group, and enter that
group's chat id as the channel. Messages are plain text with a link.

Every enabled alert goes to every channel of the project. A delivery
that fails is not retried; if no channel took the alert at all, the
cooldown is not used up, so the next occurrence alerts again.

## Adding channels from the API

```sh
curl -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
     -d '{"kind":"webhook","config":{"url":"https://hooks.example.com/crashcart"}}' \
     https://crashcart.example.com/api/projects/shop-ios/alerts/channels
```

See the [API reference](/reference/api#alerts).
