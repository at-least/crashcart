# CrashCart — Architecture (Go + Postgres)

## Use cases

1. **Dashboard** — stat cards (Crashes / Errors / Issues), crash timeline, error volume,
   event table. Reads `hourly_stats` (O(hours)), `issues`, `events` (PK range walk).
2. **Device debug** — `device_id` → `events` PK range scan over the window, filtered by device.
3. **User debug** — `user_id` → `user_devices` → `device_id IN (…)` OR `user_id =`.
4. **Issue drill-down** — `fingerprint` → events of that issue; exact, not by error type.

## Tables

| Table | PK | Notes |
|---|---|---|
| `events` | `id` = unix_ms×1000+rnd | Time lives in the PK (`internal/pk`); range-partitioned by UTC day on it (`events_pYYYYMMDD` + `events_default`); columns extracted from the envelope, `tags JSONB`, `breadcrumbs JSONB`, `payload JSONB`, `fingerprint` |
| `user_devices` | `(user_id, device_id)` | `last_seen`, refreshed ≤ once/day/pair |
| `hourly_stats` | `(hour, level)` | `crash_count`, `fatal_count`, `error_count` for error + fatal levels |
| `issues` | `fingerprint` | title/level/type/screen/platform, status lifecycle, counts, first/last seen + release |
| `releases` | `version` | crash/error/total counters, first/last seen |
| `release_health` | `(release, day)` | total/crashed/errored sessions |
| `alert_types` | `type` | enabled, last_triggered, cooldown_until |
| `symbol_files` | `(platform, release, filename)` | `data bytea`, `uploaded_at` |
| `schema_migrations` | `version` | applied migration files |

**Indexes: none beyond primary keys.** A write to `events` is one heap row + one btree
entry. Reads are PK range scans bounded by the query window:

```
dashboard list   events WHERE id >= lower(since) AND id < upper(until) AND <filters>
                 ORDER BY id DESC LIMIT 50      -- walks the PK backwards, stops at 50
issue filters    SELECT DISTINCT fingerprint FROM events WHERE id range AND <filters>
                 → issues WHERE fingerprint = ANY(…)
crash spike      COUNT(*) FROM events WHERE id >= lower(now-10m) AND crash
retention        DROP TABLE events_pYYYYMMDD for whole days past the cutoff;
                 DELETE … WHERE id < lower(cutoff) LIMIT 5000 on events_default (strays)
user → devices   user_devices PK (user_id, device_id)
```

Cost per event ≈ 1 row + 1 PK entry + the folded aggregate upserts (≈ 0.5 rows/event).

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
  events                     → events WHERE id in window ORDER BY id DESC LIMIT 50 OFFSET n
  release picker             → releases active in window

Alerts (every ALERT_INTERVAL, default 10 min):
  crash_spike: crashes in last 10 min vs 3× (weekly daily avg / 144), excluding last 2 days
  new_error:   issues first_seen since last trigger (≤ 24 h back, ≥ 10 min)
  regression:  issues with status = regression seen since last trigger
  20 min cooldown per type; channels: Telegram, webhooks, SMTP

Retention (every RETENTION_INTERVAL, default 1 h):
  drop event partitions past the cutoff day, ensure partitions for the next 3 days,
  trim events_default in 5000-row batches, then hourly_stats, issues, user_devices, release_health
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
