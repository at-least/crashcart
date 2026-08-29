# Security & privacy

What CrashCart stores, who can see it, and where it goes.

## Where the data lives

In your Postgres — all of it, raw crash payloads and symbol files
included. CrashCart makes no outbound connections of its own: no
telemetry, no update checks, no third-party services. The only outbound traffic is the alert channels you
configure — a webhook URL or Telegram — and the dSYM sidecar if you run
it, which is a container next to CrashCart, not a service.

## What is stored

Whatever the SDK sends in an event: the exception and stack trace,
breadcrumbs, tags, the `user` object, device model and OS version,
`extra` data, and the raw envelope payload. Sessions carry a release,
environment and a crash/normal outcome. Transactions, profiles and
replays are discarded on arrival.

- **`PII_REDACT=true`** scrubs emails, phone numbers, tokens and user
  ids from events before they are written. Do this if your apps might
  put personal data in messages, breadcrumbs or tags.
- **Retention.** Raw events and sessions are deleted after
  `RETENTION_DAYS` (30). Issues keep their title, counts and status —
  no user data — after their events are gone.
- **Deleting a project** (`DELETE /api/projects/{slug}`) removes its
  events, sessions, issues and symbol files.
- **Export** (`crashcart export`) produces the full data set as plain
  text, so you can audit exactly what is held.

## Who can access what

| | Protected by | Notes |
|---|---|---|
| Ingest (`/api/<id>/envelope/`) | The DSN key | Must be reachable by your apps, so usually public. The key is fine inside an app binary; anyone holding it can send events to that project, nothing more. Rotate it from **Settings** |
| Viewer (`/`) | User accounts: email + password (bcrypt), a session cookie (`HttpOnly`, `SameSite=Lax`, `Secure` behind HTTPS) that lasts 30 days | The first account is created on `/setup` (only while there are none) or with `crashcart user add`. Users are managed on **Account** in the viewer. No roles: every user can do everything |
| API (`/api/…`) and `sentry-cli` uploads | API keys: `Authorization: Bearer cc_…` | Created on **Account** or with `crashcart apikey create`; the secret is shown once and stored hashed. Revoke any time; `last_used_at` shows what is still in use. Without a key the API is closed |
| `/health` | none | Returns only status |

CrashCart does not terminate TLS. Put Caddy, nginx or a load balancer in
front; every [install guide](/deploy/docker) does this.

## Limits that protect you

- `RATE_LIMIT` requests per minute per DSN key and API key (600); SDKs
  honour `429` and resend later.
- Envelopes over 20 MB are rejected.
- Per-project **daily quota** and **sampling** cap how much one runaway
  bug can store, while the issue's count stays exact.

See [Before going live](./checklist) for the short version.
