// Package cli is the one definition of the crashcart subcommands: the
// usage text the binary prints and docs/reference/cli.md (cmd/gendocs)
// are both rendered from Commands.
package cli

import (
	"fmt"
	"strings"
)

// Command is one subcommand.
type Command struct {
	Name    string // "serve", "user add", …
	Args    string // "[slug]", "<email> [name]", … ("" = none)
	Summary string // one line, for the usage listing
	Doc     string // paragraphs for the reference ("" = the summary is all there is)
}

// Commands in the order they are listed.
var Commands = []Command{
	{Name: "serve", Summary: "HTTP server + job worker + schedulers (default)",
		Doc: "Runs everything in one process: HTTP on `LISTEN_ADDR`, `WORKERS` job goroutines, and the schedulers — the stats rollup and the ignored-issue check every minute, the retention sweep hourly, the unhandled-spike check every `ALERT_INTERVAL` (each on one replica at a time). The schema is created first. This is the default when no subcommand is given."},
	{Name: "init", Summary: "create the schema and exit",
		Doc: "Creates the schema in an empty database and exits (every command does this on start; `init` is for a deploy pipeline step, or to prepare a database before `import`). On a database that already has a schema it checks the schema version and exits non-zero on a mismatch (see [Upgrading](/deploy/operations#upgrading))."},
	{Name: "retention", Summary: "create partitions, run one sweep and roll the stats up",
		Doc: "Creates the coming weeks' partitions, runs one retention sweep and rolls the statistics up. Exits when done — for cron."},
	{Name: "alerts", Summary: "run one unhandled-spike check",
		Doc: "Evaluates the unhandled-spike rule once for every project and queues alerts. Exits when done — for cron."},
	{Name: "seed", Args: "[slug]", Summary: `write a week of demo data (default project "demo")`,
		Doc: "Writes a week of realistic demo data (issues, events, sessions across a few releases) into the project `slug`, creating it if needed. Default `demo`."},
	{Name: "export", Args: "[slug]", Summary: "stream NDJSON to stdout (all projects, or one)",
		Doc: "Writes everything to stdout as NDJSON in the [export format](./export-format): all projects, or just `slug`.\n\n```sh\ncrashcart export > backup.ndjson\ncrashcart export shop-ios | gzip > shop-ios.ndjson.gz\n```"},
	{Name: "import", Summary: "load NDJSON from stdin (idempotent)",
		Doc: "Reads NDJSON from stdin and upserts. Safe to run twice or against a live database; unknown project slugs are created; lines with an unknown `\"t\"` are counted as `skipped`. The report (rows per table, lines committed) goes to stderr as JSON.\n\n```sh\ncrashcart import < backup.ndjson\n```"},
	{Name: "project", Args: "<slug> <name> [platform]", Summary: "create a project and print its DSN",
		Doc: "Creates a project and prints its id and DSN (using `PUBLIC_URL` when set). `platform`, if given, must be one of the SDK families listed under [Glossary](./glossary#core-concepts)."},
	{Name: "rotate-key", Args: "<slug>", Summary: "issue a new DSN key (the old one keeps working until deleted)",
		Doc: "Generates a new current DSN key and prints the new DSN. The old key is not invalidated — it keeps authenticating, listed under `project-keys list`, until explicitly deleted with `project-keys delete`."},
	{Name: "project-keys list", Args: "<slug>", Summary: "list a project's retired-but-still-valid DSN keys"},
	{Name: "project-keys delete", Args: "<slug> <id>", Summary: "delete a retired DSN key (stops it within the ingest cache TTL)"},
	{Name: "user add", Args: "<email> [name]", Summary: "create a viewer account (password from CRASHCART_PASSWORD, else prompted)"},
	{Name: "user passwd", Args: "<email>", Summary: "set a viewer account's password (same source)"},
	{Name: "apikey create", Args: "<name>", Summary: "create an API key and print its secret (shown once)"},
	{Name: "apikey list", Summary: "list API keys with their state and last use"},
	{Name: "apikey revoke", Args: "<id>", Summary: "revoke an API key"},
	{Name: "symbolicate", Summary: "dSYM symbolication sidecar (needs llvm-symbolizer, no database)",
		Doc: "Runs the dSYM symbolication sidecar: HTTP on `LISTEN_ADDR`, `llvm-symbolizer` from `PATH`, a disk cache of the dSYMs it has used (`SYMBOLICATE_CACHE_DIR`, `SYMBOLICATE_CACHE_MAX_MB`). The only subcommand that does not read `DATABASE_URL`; the server reaches it through `SYMBOLICATE_URL`. See [Symbolication](/guide/symbolication#ios-macos)."},
	{Name: "version", Summary: "print the version and exit"},
}

// Invocation is "crashcart <name> <args>" padded for a listing.
func (c Command) Invocation() string {
	s := "crashcart " + c.Name
	if c.Args != "" {
		s += " " + c.Args
	}
	return s
}

// Usage is the text `crashcart help` prints.
func Usage() string {
	var b strings.Builder
	b.WriteString("usage: crashcart <command>\n\n")
	width := 0
	for _, c := range Commands {
		width = max(width, len(c.Invocation()))
	}
	for _, c := range Commands {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, c.Invocation(), c.Summary)
	}
	return b.String()
}
