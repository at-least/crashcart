# Alerts

Each project has three detectors. Enable them under **Settings → Alerts**
or with `PATCH /api/projects/{slug}/alerts/{type}`, and add one or more
channels to deliver to.

| Type | Fires when |
|---|---|
| `new_issue` | An issue is seen for the first time |
| `regression` | A resolved issue receives an event on a different release |
| `crash_spike` | Crashes in the last hour ≥ 10 **and** ≥ 3 × the hourly mean of the previous 24 h |

`new_issue` and `regression` are evaluated at ingest. `crash_spike` runs on
a schedule ([`ALERT_INTERVAL`](/deploy/configuration), 10 minutes) and fires
at most once per spike, not every interval.

## Channels

```sh
# Webhook
curl -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
     -d '{"kind":"webhook","config":{"url":"https://hooks.example.com/crashcart"}}' \
     https://crashcart.example.com/api/projects/shop-ios/alerts/channels

# Telegram (needs TELEGRAM_BOT_TOKEN on the server)
curl -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
     -d '{"kind":"telegram","config":{"chat_id":"-1001234567890"}}' \
     https://crashcart.example.com/api/projects/shop-ios/alerts/channels
```

Every enabled detector delivers to every channel of the project.

### Webhook payload

`POST` with `Content-Type: application/json`:

```json
{
  "type": "crash_spike",
  "project": "Shop app (iOS)",
  "project_slug": "shop-ios",
  "title": "Crash spike: 42 crashes in the last hour (baseline 3.5/h)",
  "recent": 42,
  "baseline": 84,
  "url": "https://crashcart.example.com/p/shop-ios"
}
```

```json
{
  "type": "new_issue",
  "project": "Shop app (iOS)",
  "project_slug": "shop-ios",
  "title": "NSInvalidArgumentException at CartViewController.swift:88",
  "fingerprint": "5f1c…",
  "level": "fatal",
  "event_count": 1,
  "first_release": "2.4.1",
  "last_release": "2.4.1",
  "url": "https://crashcart.example.com/p/shop-ios/issues/5f1c…"
}
```

Fields not relevant to a type are omitted. `baseline` for a crash spike is
the total over the 24-hour window; divide by 24 for the hourly mean the
title quotes. `url` uses [`PUBLIC_URL`](/deploy/configuration).

Slack, Discord and most chat tools accept a generic webhook; put a small
relay in front if they need a specific shape.

### Telegram

Create a bot with @BotFather, set its token as `TELEGRAM_BOT_TOKEN` on the
server, add the bot to a group or channel, and use that chat's id in the
channel config. Messages are plain text with a link to the issue.

## Delivery

Alerts are jobs in the Postgres queue, processed by the worker goroutines
in `crashcart serve`. A failed delivery retries with backoff and is dropped
after 8 attempts. For cron-style operation without a long-running server,
`crashcart alerts` runs one crash-spike check and exits.
