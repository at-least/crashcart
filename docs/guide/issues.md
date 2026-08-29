# Issues & grouping

An **issue** is a group of events that share a **fingerprint**. Issues are
what you triage; events are the evidence.

## Fingerprinting

The fingerprint is a hash computed from the exception's stack trace:

1. Take the exception chain's frames.
2. Drop frames that carry no signal for grouping: SDK internals, async
   placeholders (Dart), panic-machinery frames (`sentry_panic` in Rust),
   and similar per-platform noise.
3. Hash the remaining frames' function + module (or, for frames with no
   symbol — native crashes before symbolication — image + offset).

Events without a stack trace group by error type and message.

The same algorithm runs in the Go and serverless editions, so an exported
dump groups identically after import.

### Symbolication changes fingerprints

An obfuscated Android stack (`a.b.c.d`) and its symbolicated form
(`CartFragment.onResume`) hash differently. When a
[symbol file](./symbolication) arrives, CrashCart re-symbolicates the
release's recent events and updates their fingerprint and `error_location`,
so issues converge on the readable stack. Upload mappings before or soon
after a release ships to avoid a transient split.

## Lifecycle

| Status | Meaning | Set by |
|---|---|---|
| `unresolved` | New, not yet looked at | Ingest |
| `triaged` | Acknowledged, someone is on it | You |
| `resolved` | Fixed | You; records `resolved_release` |
| `regression` | Was resolved, then seen again on a *different* release | Ingest |
| `ignored` | Known, won't fix; stays out of the default list | You |

**Regression** is precise: a resolved issue that receives an event whose
release differs from the release it was resolved on. Events on the same
release do not reopen it — a resolved bug that keeps crashing old builds in
the field is not a regression.

Change status from the issue page, with
[keyboard shortcuts](./viewer#keyboard-triage) on the list, in bulk, or via
`PATCH /api/projects/{slug}/issues/{fingerprint}`.

## Counts

Every issue keeps:

- `event_count` — exact number of events matched, including those dropped
  by sampling
- `stored_count` — events actually kept
- `users` — distinct `user.id`s
- `first_seen`, `last_seen`, `first_release`, `last_release`

## Sampling

Storage per issue is bounded by the project's sampling settings
([Projects & DSNs](./projects#sampling-and-quota)):

- The first `sample_keep_first` events of an issue are always stored.
- Afterwards each event is kept with probability `sample_rate`.
- `fatal` events are always stored.
- Dropped events still increment `event_count`.

The result: a noisy issue with a million occurrences costs you a few
hundred rows, its count still says a million, and crashes are never
sampled away.

## Retention

Raw events are dropped after [`RETENTION_DAYS`](/deploy/configuration).
Issues outlive their events: the issue row, its counts and its status stay,
so an old resolved issue can still turn into a regression. Hourly
statistics behind the sparklines and timelines are kept for 400 days.
