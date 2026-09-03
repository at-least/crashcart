// Package config reads the process configuration from the environment.
//
// Vars is the one definition of every variable — name, default and
// meaning; Load reads the defaults from it and docs/deploy/configuration.md
// is generated from it (cmd/gendocs), so the two cannot drift.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/at-least/crashcart/internal/blob"
)

// Config is everything the binary needs; loaded once in main.
type Config struct {
	Addr        string
	DatabaseURL string
	PublicURL   string // externally visible base URL (DSN display); "" = derive from request

	CORSOrigin    string // SDK ingest endpoints (browser SDKs); "*" by default
	APICORSOrigin string // /api/* JSON API; "" = no CORS headers (same-origin / non-browser callers)
	RateLimit     int    // requests / minute / credential; 0 = off
	TrustProxy    bool   // X-Forwarded-For names the client (behind a reverse proxy)

	RetentionDays         int
	SymbolicateCacheDir   string // crashcart symbolicate: where the sidecar keeps dSYMs
	SymbolicateCacheMB    int    // … and its bound
	AlertInterval         time.Duration
	Workers               int
	SymbolicateURL        string // dSYM sidecar
	TelegramBotToken      string
	FCMServiceAccountJSON string // push notifications for the mobile companion apps (Firebase Cloud Messaging HTTP v1)
	WebhookAllowPrivate   bool   // webhooks may target RFC 1918 / ULA addresses (a service on the LAN)
	PIIRedact             bool
	CustomTags            []string // tag keys the viewer shows as filters

	BlobStore string        // where symbol files and event payloads are stored: "postgres" (default) or "s3"
	S3        blob.S3Config // BLOB_STORE=s3
}

// Group is a section of the configuration reference.
type Group struct {
	Name  string // heading
	Intro string // one paragraph after the group's table ("" = none)
}

// Groups in the order the reference lists them.
var Groups = []Group{
	{"Required", "See [The database](./postgres).\n\nAccess is not configured here: the viewer uses user accounts and the API\nuses API keys, both managed in the viewer (**Account**) or with\n`crashcart user` / `crashcart apikey` — see [Security](./security)."},
	{"Recommended", ""},
	{"Data", "Changing a data setting and restarting is enough — it applies to existing data too."},
	{"Optional features", ""},
	{"Tuning", ""},
}

// Var is one environment variable.
type Var struct {
	Name    string
	Group   string // one of Groups
	Default string // what Load uses when the variable is unset or blank
	Shown   string // how the reference shows the default when Default is not literal ("" = show Default, or "—" when empty)
	Doc     string // meaning, one sentence or two
}

// Vars is every variable the binary reads, in reference order.
var Vars = []Var{
	{Name: "DATABASE_URL", Group: "Required", Doc: "Postgres connection URL"},

	{Name: "PUBLIC_URL", Group: "Recommended", Shown: "derived from the request",
		Doc: "The address your apps use. Shown in DSNs, used in alert links and by `sentry-cli`. Set it when CrashCart is behind a proxy or domain"},
	{Name: "CORS_ORIGIN", Group: "Recommended", Default: "*",
		Doc: "Which web origins may send events (the SDK endpoints). Set to your site for browser SDKs"},
	{Name: "API_CORS_ORIGIN", Group: "Recommended", Shown: "empty (no CORS)",
		Doc: "Web origin allowed to call `/api/*` from a browser. Leave empty unless a browser app talks to the JSON API"},

	{Name: "RETENTION_DAYS", Group: "Data", Default: "30",
		Doc: "Days to keep raw events and sessions (whole weekly partitions are dropped, so up to a week longer). Symbol files are kept twice as long; issues and statistics longer still"},
	{Name: "PII_REDACT", Group: "Data", Default: "false",
		Doc: "Scrub emails, phone numbers, tokens and user ids before storing events"},

	{Name: "SYMBOLICATE_URL", Group: "Optional features", Shown: "off",
		Doc: "Address of the dSYM sidecar, e.g. `http://symbolicate:8080`"},
	{Name: "SYMBOLICATE_CACHE_DIR", Group: "Optional features", Shown: "`$TMPDIR/crashcart-symbols`",
		Doc: "`crashcart symbolicate` only: where the sidecar keeps the dSYMs it has used"},
	{Name: "SYMBOLICATE_CACHE_MAX_MB", Group: "Optional features", Default: "4096",
		Doc: "`crashcart symbolicate` only: bound of that cache; least recently used dSYMs are dropped"},
	{Name: "TELEGRAM_BOT_TOKEN", Group: "Optional features", Doc: "Bot token for Telegram alerts"},
	{Name: "FCM_SERVICE_ACCOUNT_JSON", Group: "Optional features", Shown: "off",
		Doc: "Firebase service account key (the whole JSON document, not a file path) for push notifications to the iOS/Android companion apps. The project id is read from the document itself"},
	{Name: "WEBHOOK_ALLOW_PRIVATE", Group: "Optional features", Default: "false",
		Doc: "Let webhooks target private addresses (10/8, 172.16/12, 192.168/16, fc00::/7) — a service on your LAN. Loopback, link-local (cloud metadata) and redirects are always refused"},
	{Name: "CUSTOM_TAGS", Group: "Optional features",
		Doc: "Comma-separated tag keys to offer as filters in the viewer, e.g. `tenant,feature_flag`"},
	{Name: "BLOB_STORE", Group: "Optional features", Default: "postgres",
		Doc: "Where the big bytes go — uploaded symbol files (ProGuard mappings, dSYMs, source maps) and raw event payloads: `postgres` — in the database, nothing else to run; `s3` — an S3-compatible bucket, which keeps the database to metadata (a fiftieth of the size) so backups, replication and exports stay small. Rows already written stay where they are"},
	{Name: "S3_BUCKET", Group: "Optional features", Doc: "`BLOB_STORE=s3`: the bucket (must exist)"},
	{Name: "S3_ENDPOINT", Group: "Optional features", Shown: "AWS",
		Doc: "`BLOB_STORE=s3`: host[:port] of an S3-compatible store — MinIO, R2, Backblaze, Ceph — e.g. `minio:9000` or `https://<account>.r2.cloudflarestorage.com` (`http://` for a plain-HTTP MinIO on the LAN). Empty means AWS S3"},
	{Name: "S3_REGION", Group: "Optional features", Shown: "`us-east-1`", Doc: "`BLOB_STORE=s3`: the bucket's region (AWS); other stores ignore it"},
	{Name: "S3_ACCESS_KEY", Group: "Optional features",
		Doc: "`BLOB_STORE=s3`: static credentials, with `S3_SECRET_KEY`. Leave both empty to use the usual chain: `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`, the shared credentials file, or an instance / task / pod role"},
	{Name: "S3_SECRET_KEY", Group: "Optional features", Doc: "`BLOB_STORE=s3`: see `S3_ACCESS_KEY`"},
	{Name: "S3_PREFIX", Group: "Optional features", Doc: "`BLOB_STORE=s3`: key prefix inside the bucket, e.g. `crashcart/`"},

	{Name: "LISTEN_ADDR", Group: "Tuning", Default: ":8080", Doc: "Port to listen on"},
	{Name: "TRUST_PROXY", Group: "Tuning", Default: "false",
		Doc: "Behind a reverse proxy: take the client address from `X-Forwarded-For` (rate limits per IP on the sign-in pages). Leave off when clients reach CrashCart directly — the header is then theirs to forge"},
	{Name: "RATE_LIMIT", Group: "Tuning", Default: "6000",
		Doc: "Requests per minute allowed per DSN key or API key (100/s — a burst of cached crashes after an outage fits), counted in memory per process (each replica enforces it on its own traffic). `0` disables"},
	{Name: "ALERT_INTERVAL", Group: "Tuning", Default: "10m", Doc: "How often to check for unhandled-error spikes"},
	{Name: "WORKERS", Group: "Tuning", Default: "4", Doc: "Parallelism for symbolication and alert delivery"},
}

// def is the declared default of a variable; unknown names panic (a
// variable read by Load must be declared in Vars, or the reference would
// not list it).
func def(name string) string {
	for _, v := range Vars {
		if v.Name == name {
			return v.Default
		}
	}
	panic("config: undeclared variable " + name)
}

// Load reads the environment. It fails only on unparseable values; missing
// values fall back to defaults and DATABASE_URL is validated by the caller.
func Load() (Config, error) {
	c := Config{
		Addr:                  get("LISTEN_ADDR"),
		DatabaseURL:           get("DATABASE_URL"),
		PublicURL:             strings.TrimSuffix(get("PUBLIC_URL"), "/"),
		CORSOrigin:            get("CORS_ORIGIN"),
		APICORSOrigin:         get("API_CORS_ORIGIN"),
		SymbolicateURL:        strings.TrimSuffix(get("SYMBOLICATE_URL"), "/"),
		SymbolicateCacheDir:   get("SYMBOLICATE_CACHE_DIR"),
		TelegramBotToken:      get("TELEGRAM_BOT_TOKEN"),
		FCMServiceAccountJSON: get("FCM_SERVICE_ACCOUNT_JSON"),
		PIIRedact:             get("PII_REDACT") == "true",
		TrustProxy:            get("TRUST_PROXY") == "true",
		WebhookAllowPrivate:   get("WEBHOOK_ALLOW_PRIVATE") == "true",
		CustomTags:            SplitCSV(get("CUSTOM_TAGS")),
		BlobStore:             get("BLOB_STORE"),
		S3: blob.S3Config{
			Bucket: get("S3_BUCKET"), Endpoint: get("S3_ENDPOINT"), Region: get("S3_REGION"),
			AccessKey: get("S3_ACCESS_KEY"), SecretKey: get("S3_SECRET_KEY"), Prefix: get("S3_PREFIX"),
		},
	}
	if c.SymbolicateCacheDir == "" {
		c.SymbolicateCacheDir = filepath.Join(os.TempDir(), "crashcart-symbols")
	}
	switch c.BlobStore {
	case "postgres":
	case "s3":
		if c.S3.Bucket == "" {
			return c, fmt.Errorf("BLOB_STORE=s3 needs S3_BUCKET")
		}
		if (c.S3.AccessKey == "") != (c.S3.SecretKey == "") {
			return c, fmt.Errorf("S3_ACCESS_KEY and S3_SECRET_KEY must be set together")
		}
	default:
		return c, fmt.Errorf("BLOB_STORE must be postgres or s3, not %q", c.BlobStore)
	}
	var err error
	if c.RateLimit, err = intEnv("RATE_LIMIT"); err != nil {
		return c, err
	}
	if c.RetentionDays, err = intEnv("RETENTION_DAYS"); err != nil {
		return c, err
	}
	if c.SymbolicateCacheMB, err = intEnv("SYMBOLICATE_CACHE_MAX_MB"); err != nil {
		return c, err
	}
	if c.SymbolicateCacheMB < 1 {
		return c, fmt.Errorf("SYMBOLICATE_CACHE_MAX_MB must be >= 1")
	}
	if c.Workers, err = intEnv("WORKERS"); err != nil {
		return c, err
	}
	if c.AlertInterval, err = durEnv("ALERT_INTERVAL"); err != nil {
		return c, err
	}
	if c.RetentionDays < 1 {
		return c, fmt.Errorf("RETENTION_DAYS must be >= 1")
	}
	if c.AlertInterval <= 0 {
		return c, fmt.Errorf("ALERT_INTERVAL must be > 0 (disable unhandled-spike alerts per project instead)")
	}
	return c, nil
}

// Retention is RETENTION_DAYS as a duration: the raw-row window, and what
// ingest treats as the oldest plausible device clock. Load guarantees
// RetentionDays >= 1; a zero Config (tests) means the declared default.
func (c Config) Retention() time.Duration {
	d := c.RetentionDays
	if d < 1 {
		d, _ = strconv.Atoi(def("RETENTION_DAYS"))
	}
	return time.Duration(d) * 24 * time.Hour
}

// get is the variable's value, or its declared default when unset or blank.
func get(k string) string {
	if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def(k)
}

func intEnv(k string) (int, error) {
	n, err := strconv.Atoi(get(k))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return n, nil
}

func durEnv(k string) (time.Duration, error) {
	d, err := time.ParseDuration(get(k))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return d, nil
}

// SplitCSV splits a comma-separated list, trimming blanks.
func SplitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
