# CrashCart — Architecture (Go + Postgres)

## Use cases

1. **Dashboard** — stat cards (Crashes / Errors / Issues), crash timeline, error volume,
   event table. Reads `hourly_stats` (O(hours)), `issues`, `events` (index range walk).
2. **Device debug** — `device_id` → `events` via the partial index `(device_id, occurred_at)`.
3. **User debug** — `user_id` → `user_devices` → `device_id IN (…)` OR `user_id =`.
4. **Issue drill-down** — `fingerprint` → events of that issue; exact, not by error type.

## Tables

| Table | PK | Notes |
|---|---|---|
| `events` | `id` identity | `occurred_at`, columns extracted from the envelope, `tags JSONB`, `breadcrumbs JSONB`, `payload JSONB`, `fingerprint` |
| `user_devices` | `(user_id, device_id)` | `last_seen`, refreshed ≤ once/day/pair |
| `hourly_stats` | `(hour, level)` | `crash_count`, `fatal_count`, `error_count` for error + fatal levels |
| `issues` | `fingerprint` | title/level/type/screen/platform, status lifecycle, counts, first/last seen + release |
| `releases` | `version` | crash/error/total counters, first/last seen |
| `release_health` | `(release, day)` | total/crashed/errored sessions |
| `alert_types` | `type` | enabled, last_triggered, cooldown_until |
| `symbol_files` | `id` | `(platform, release, filename)` unique, `data bytea` |
| `schema_migrations` | `version` | applied migration files |

Indexes: `events(occurred_at DESC, id DESC)`, partial btrees on device/user/error_type/
release/fingerprint (`… , occurred_at DESC) WHERE col IS NOT NULL`), GIN on `tags`.

## Data flow

```
Ingest (one transaction per envelope):
  parse envelope (events + sessions)
  sample: error/fatal always, warning ≥ 50%, info/debug SAMPLE_RATE
  redact (PII_REDACT): message, tags, user_id
  per event: fingerprint, error_location (deepest in-app frame)
  COPY events
  upsert user_devices (last_seen if older than 1 day)
  upsert hourly_stats  (error|fatal only)      — folded per (hour, level)
  upsert releases                                — folded per version
  upsert issues (+ regression detection)         — folded per fingerprint
  upsert release_health                           — folded per (release, day)

Dashboard:
  stats / timeline / volume  → hourly_stats
  issues                     → issues (last_seen in window)
  events                     → events ORDER BY occurred_at DESC LIMIT 50 OFFSET n
  release picker             → releases active in window

Alerts (every ALERT_INTERVAL, default 10 min):
  crash_spike: crashes in last 10 min vs 3× (weekly daily avg / 144), excluding last 2 days
  new_error:   issues first_seen since last trigger (≤ 24 h back, ≥ 10 min)
  regression:  issues with status = regression seen since last trigger
  20 min cooldown per type; channels: Telegram, webhooks, SMTP

Retention (every RETENTION_INTERVAL, default 1 h):
  events (5000-row batches), hourly_stats, issues, user_devices, release_health
```

## Envelope handling

- Item headers with `length` are honored (bodies may contain newlines); otherwise the
  body is the next line. Non-JSON lines resync one line at a time.
- Timestamps: RFC3339 with any offset, unix seconds or milliseconds → UTC.
- `event_id` from the item, else the envelope header, else `ts-<ms>-<sha256[:6]>`.
- Message: `logentry.formatted` → `logentry.message` → `message` → `"Type: value"`.
- Level normalized (`warn` → `warning`, `critical` → `fatal`, empty → `error`).
- Tags accept `{k: v}` or `[[k, v], …]`; values stringified.
- Payload stored verbatim (NUL escapes stripped for JSONB).

## Multi-instance portal

`DEPLOYMENTS="iOS|https://ios.example.com|key,Android|https://android.example.com|key"`
turns `/` into a portal: one card row per instance (this instance in-process, the others
over `/api/stats*` + `/api/issues` with the key), and `/{slug}/dashboard` serves the
instance whose origin matches the request; other slugs redirect to their instance.

## Symbolication

- `.txt` → ProGuard/R8 mapping parsed in-process (class + method + line-range lines).
- `.map` / platform `javascript`|`node` → Source Map v3 VLQ decoder, binary search by
  (line, column).
- dSYM / platform `ios` → streamed to `SYMBOLICATE_URL/symbolicate` with frames in
  `X-Frames` (see `container/symbolicate`).
