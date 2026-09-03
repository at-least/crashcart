// Command gendocs keeps the reference docs that mirror Go data in sync
// with it.
//
// Two pages are fully generated, like templ generate code:
// docs/deploy/configuration.md from config.Vars/Groups, and
// docs/reference/cli.md from cli.Commands.
//
// Two pages are hand-written prose too detailed to template, so they are
// only checked against the code: docs/reference/api.md must mention every
// route api.Register mounts (and no others), and
// docs/reference/export-format.md's format number and table order must
// match internal/export. GLOSSARY.md's enum value lists are checked
// against schema.sql the same way.
//
// Default: write the generated pages, then run the checks; a check
// failure exits 1 (the pages were still written). -check: write nothing,
// and fail if a generated page would change on disk or a check fails —
// this is `make test`'s drift gate, so it must not mutate the tree.
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/at-least/crashcart/internal/api"
	"github.com/at-least/crashcart/internal/cli"
	"github.com/at-least/crashcart/internal/config"
	"github.com/at-least/crashcart/internal/db"
	"github.com/at-least/crashcart/internal/export"
	"github.com/at-least/crashcart/internal/server"
)

const (
	configurationPath = "docs/deploy/configuration.md"
	cliPath           = "docs/reference/cli.md"
	apiPath           = "docs/reference/api.md"
	exportFormatPath  = "docs/reference/export-format.md"
	glossaryPath      = "GLOSSARY.md"
)

func main() {
	check := flag.Bool("check", false, "verify without writing; exit non-zero on drift")
	flag.Parse()

	var fail []string
	type page struct{ path, want string }
	pages := []page{
		{configurationPath, renderConfiguration()},
		{cliPath, renderCLI()},
	}
	for _, p := range pages {
		path, want := p.path, p.want
		got, err := os.ReadFile(path)
		if err != nil {
			fail = append(fail, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		if string(got) == want {
			continue
		}
		if *check {
			fail = append(fail, fmt.Sprintf("%s is out of date (run `make generate`)", path))
			continue
		}
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			fail = append(fail, fmt.Sprintf("%s: %v", path, err))
		}
	}

	fail = append(fail, checkAPIRoutes()...)
	fail = append(fail, checkExportFormat()...)
	fail = append(fail, checkGlossaryEnums()...)

	if len(fail) > 0 {
		for _, f := range fail {
			fmt.Fprintln(os.Stderr, "gendocs:", f)
		}
		os.Exit(1)
	}
}

// renderConfiguration builds docs/deploy/configuration.md from
// config.Vars/Groups; the prose that isn't a per-variable fact (the page
// intro and the "Per project" close) is fixed here, not in config.Vars.
func renderConfiguration() string {
	var b strings.Builder
	b.WriteString("# Configuration\n\nEverything is set with environment variables.\n\n")
	for _, g := range config.Groups {
		fmt.Fprintf(&b, "## %s\n\n", g.Name)
		var vars []config.Var
		for _, v := range config.Vars {
			if v.Group == g.Name {
				vars = append(vars, v)
			}
		}
		if g.Name == "Required" {
			b.WriteString("| Variable | Meaning |\n|---|---|\n")
			for _, v := range vars {
				fmt.Fprintf(&b, "| `%s` | %s |\n", v.Name, v.Doc)
			}
		} else {
			b.WriteString("| Variable | Default | Meaning |\n|---|---|---|\n")
			for _, v := range vars {
				fmt.Fprintf(&b, "| `%s` | %s | %s |\n", v.Name, shownDefault(v), v.Doc)
			}
		}
		if g.Intro != "" {
			fmt.Fprintf(&b, "\n%s\n", wrap(g.Intro))
		}
		b.WriteString("\n")
	}
	b.WriteString("## Per project\n\nSampling is set per project in the viewer (Settings → Sampling). See\n[Projects & DSNs](/guide/projects#sampling).\n")
	return b.String()
}

// shownDefault is how a variable's default is shown in the reference
// table: Shown verbatim when set, else the literal Default backtick-quoted,
// else "—" for a variable with no meaningful default.
func shownDefault(v config.Var) string {
	if v.Shown != "" {
		return v.Shown
	}
	if v.Default != "" {
		return "`" + v.Default + "`"
	}
	return "—"
}

// renderCLI builds docs/reference/cli.md from cli.Commands; the invocation
// table and the per-command sections are generated, the surrounding prose
// is fixed here.
func renderCLI() string {
	var b strings.Builder
	b.WriteString("# CLI\n\nThe `crashcart` binary is both the server and the admin tool. Every\n" +
		"subcommand reads [`DATABASE_URL`](/deploy/configuration) and the other\n" +
		"environment variables; most connect to the database and exit.\n\n```\n")
	width := 0
	for _, c := range cli.Commands {
		width = max(width, len(c.Invocation()))
	}
	for _, c := range cli.Commands {
		fmt.Fprintf(&b, "%-*s  %s\n", width, c.Invocation(), c.Summary)
	}
	b.WriteString("```\n\nWith Docker Compose, prefix with `docker compose exec crashcart /crashcart`.\n")
	for _, c := range cli.Commands {
		if c.Doc == "" {
			continue
		}
		heading := c.Name
		if c.Args != "" {
			heading += " " + c.Args
		}
		fmt.Fprintf(&b, "\n## `%s`\n\n%s\n", heading, wrap(c.Doc))
	}
	b.WriteString("\n## Exit status\n\n`0` on success. Any error is logged and exits `1`.\n")
	return b.String()
}

// wrapWidth matches this repo's hand-wrapped prose (docs/guide/*, etc).
const wrapWidth = 76

// wrap word-wraps every paragraph of text to wrapWidth, leaving fenced
// code blocks (paragraphs starting with "```") untouched.
func wrap(text string) string {
	blocks := strings.Split(text, "\n\n")
	for i, block := range blocks {
		if strings.HasPrefix(block, "```") {
			continue
		}
		blocks[i] = wrapParagraph(block)
	}
	return strings.Join(blocks, "\n\n")
}

func wrapParagraph(p string) string {
	words := strings.Fields(p)
	if len(words) == 0 {
		return p
	}
	var b strings.Builder
	lineLen := 0
	for i, w := range words {
		wl := utf8.RuneCountInString(w)
		if i == 0 {
			b.WriteString(w)
			lineLen = wl
			continue
		}
		if lineLen+1+wl > wrapWidth {
			b.WriteByte('\n')
			lineLen = 0
		} else {
			b.WriteByte(' ')
			lineLen++
		}
		b.WriteString(w)
		lineLen += wl
	}
	return b.String()
}

var routeLine = regexp.MustCompile(`^(GET|POST|PATCH|DELETE|PUT)(\|(GET|POST|PATCH|DELETE|PUT))?\s+(\S+)`)

// checkAPIRoutes verifies docs/reference/api.md mentions exactly the
// routes Register mounts (both directions: an undocumented route and a
// stale documented one are both drift).
func checkAPIRoutes() []string {
	code := map[string]bool{}
	add := func(pattern string) {
		if strings.HasPrefix(pattern, "OPTIONS ") {
			return // CORS preflight, never reached in practice, not documented
		}
		code[normalizeRoute(pattern)] = true
	}
	for _, p := range api.RoutePatterns() {
		add(p)
	}
	for _, p := range server.IngestPatterns {
		add(p)
	}
	add(server.HealthPattern)

	doc, err := os.ReadFile(apiPath)
	if err != nil {
		return []string{fmt.Sprintf("%s: %v", apiPath, err)}
	}
	docRoutes := map[string]bool{}
	for _, line := range strings.Split(string(doc), "\n") {
		m := routeLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		methods := []string{m[1]}
		if m[3] != "" {
			methods = append(methods, m[3])
		}
		for _, meth := range methods {
			docRoutes[normalizeRoute(meth+" "+m[4])] = true
		}
	}

	var fail []string
	for r := range code {
		if !docRoutes[r] {
			fail = append(fail, fmt.Sprintf("%s: route not documented: %s", apiPath, r))
		}
	}
	for r := range docRoutes {
		if !code[r] {
			fail = append(fail, fmt.Sprintf("%s: documents a route that does not exist: %s", apiPath, r))
		}
	}
	sort.Strings(fail)
	return fail
}

// normalizeRoute masks the differences that don't matter for the doc
// check: a query string, the mux-only "{$}" exact-match marker, a
// trailing slash, and a path parameter's name (the doc is free to call it
// something more readable than the code does).
func normalizeRoute(s string) string {
	parts := strings.SplitN(strings.TrimSpace(s), " ", 2)
	if len(parts) != 2 {
		return s
	}
	method, path := parts[0], parts[1]
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	path = strings.TrimSuffix(path, "/{$}")
	path = strings.TrimSuffix(path, "/")
	segs := strings.Split(path, "/")
	for i, seg := range segs {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			segs[i] = "{}"
		}
	}
	return method + " " + strings.Join(segs, "/")
}

var (
	titleFormat = regexp.MustCompile(`format (\d+)\)`)
	metaFormat  = regexp.MustCompile(`"format":(\d+)`)
)

// checkExportFormat verifies docs/reference/export-format.md's format
// number (title and _meta example) and its ## Order table match
// internal/export's Format and Tables.
func checkExportFormat() []string {
	doc, err := os.ReadFile(exportFormatPath)
	if err != nil {
		return []string{fmt.Sprintf("%s: %v", exportFormatPath, err)}
	}
	text := string(doc)
	var fail []string
	if m := titleFormat.FindStringSubmatch(text); m == nil {
		fail = append(fail, exportFormatPath+": title does not state a format number")
	} else if n, _ := strconv.Atoi(m[1]); n != export.Format {
		fail = append(fail, fmt.Sprintf("%s: title says format %d, export.Format = %d", exportFormatPath, n, export.Format))
	}
	if m := metaFormat.FindStringSubmatch(text); m == nil {
		fail = append(fail, exportFormatPath+": no _meta example with a format number")
	} else if n, _ := strconv.Atoi(m[1]); n != export.Format {
		fail = append(fail, fmt.Sprintf("%s: _meta example says format %d, export.Format = %d", exportFormatPath, n, export.Format))
	}
	switch order := orderTables(text); {
	case order == nil:
		fail = append(fail, exportFormatPath+": no ## Order code block found")
	case !slices.Equal(order, export.Tables):
		fail = append(fail, fmt.Sprintf("%s: ## Order lists %v, export.Tables = %v", exportFormatPath, order, export.Tables))
	}
	return fail
}

// orderTables extracts the table names from the ## Order fenced code
// block (each line is "name*  (comment)"; "_meta" is not a table).
func orderTables(text string) []string {
	i := strings.Index(text, "## Order")
	if i < 0 {
		return nil
	}
	rest := text[i:]
	start := strings.Index(rest, "```")
	if start < 0 {
		return nil
	}
	rest = rest[start+3:]
	end := strings.Index(rest, "```")
	if end < 0 {
		return nil
	}
	var tables []string
	for _, line := range strings.Split(rest[:end], "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "_meta" {
			continue
		}
		tables = append(tables, strings.TrimSuffix(strings.Fields(line)[0], "*"))
	}
	return tables
}

var (
	enumDecl  = regexp.MustCompile(`(?m)^CREATE TYPE\s+(\w+)\s+AS ENUM \(([^)]*)\);`)
	enumAlter = regexp.MustCompile(`(?m)^ALTER TYPE\s+(\w+)\s+ADD VALUE\s+(?:IF NOT EXISTS\s+)?'([^']*)'`)
)

// enumValues walks every migration file in order (CREATE TYPE ... AS ENUM
// declares a type's initial values; a later ALTER TYPE ... ADD VALUE
// appends to it) and returns each enum's accumulated value list.
func enumValues() (map[string][]string, error) {
	var names []string
	if err := fs.WalkDir(db.Migrations(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		names = append(names, path)
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(names)

	values := map[string][]string{}
	for _, name := range names {
		b, err := fs.ReadFile(db.Migrations(), name)
		if err != nil {
			return nil, err
		}
		text := string(b)
		for _, m := range enumDecl.FindAllStringSubmatch(text, -1) {
			enumName, rawValues := m[1], m[2]
			for _, v := range strings.Split(rawValues, ",") {
				values[enumName] = append(values[enumName], strings.Trim(strings.TrimSpace(v), "'"))
			}
		}
		for _, m := range enumAlter.FindAllStringSubmatch(text, -1) {
			values[m[1]] = append(values[m[1]], m[2])
		}
	}
	return values, nil
}

// checkGlossaryEnums verifies that every value of an enum GLOSSARY.md
// discusses by name is actually mentioned there — catches a renamed or
// removed enum value left stale in the vocabulary doc. An enum never
// named in the glossary (an implementation detail, not user vocabulary)
// is not checked.
func checkGlossaryEnums() []string {
	doc, err := os.ReadFile(glossaryPath)
	if err != nil {
		return []string{fmt.Sprintf("%s: %v", glossaryPath, err)}
	}
	text := string(doc)
	values, err := enumValues()
	if err != nil {
		return []string{fmt.Sprintf("internal/db/migrations: %v", err)}
	}
	var fail []string
	for name, vs := range values {
		if !wordIn(text, name) {
			continue
		}
		for _, v := range vs {
			if !wordIn(text, v) {
				fail = append(fail, fmt.Sprintf("%s: %s enum value %q not mentioned", glossaryPath, name, v))
			}
		}
	}
	sort.Strings(fail)
	return fail
}

func wordIn(text, word string) bool {
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(word) + `\b`).MatchString(text)
}
