package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestNormalizeRouteCollapsesVariants: the mux needs a "{$}" pattern and a
// bare one to match a path with and without a trailing slash; the doc
// shows one line for that endpoint, so both must normalize the same way.
// A path parameter's name (which the doc is free to spell differently) and
// a query string must not affect the comparison either.
func TestNormalizeRouteCollapsesVariants(t *testing.T) {
	want := "POST /api/{}/envelope"
	for _, in := range []string{
		"POST /api/{project}/envelope/{$}",
		"POST /api/{project}/envelope",
		"POST /api/{project_id}/envelope/",
	} {
		if got := normalizeRoute(in); got != want {
			t.Errorf("normalizeRoute(%q) = %q, want %q", in, got, want)
		}
	}
	if got := normalizeRoute("GET /api/projects/{slug}/overview?days=7"); got != "GET /api/projects/{}/overview" {
		t.Errorf("query string not stripped: %q", got)
	}
}

// TestWrapLeavesCodeFencesAlone: a Doc string mixing prose with a ```sh
// example (export's Doc does exactly this) must not have its code block
// reflowed — only the prose paragraphs wrap.
func TestWrapLeavesCodeFencesAlone(t *testing.T) {
	in := "This is a long enough sentence of ordinary prose that it must wrap onto a second line at the configured width.\n\n```sh\ncrashcart export > backup.ndjson\n```"
	out := wrap(in)
	if !strings.Contains(out, "```sh\ncrashcart export > backup.ndjson\n```") {
		t.Errorf("code fence was altered:\n%s", out)
	}
	lines := strings.Split(out, "\n")
	for _, l := range lines {
		if strings.HasPrefix(l, "crashcart") || strings.HasPrefix(l, "```") {
			continue
		}
		if len(l) > wrapWidth {
			t.Errorf("prose line exceeds wrapWidth: %q", l)
		}
	}
	if !strings.Contains(out, "\n") || strings.HasPrefix(out, "This is a long enough sentence of ordinary prose that it must wrap onto a second line") {
		t.Errorf("prose paragraph was not wrapped: %s", out)
	}
}

// TestAPIRoutesAndExportFormatMatchCode: the checks this binary runs on
// `make test` must actually pass against the committed docs and code — a
// regression here means the reference has drifted from the routes,
// export format or glossary enums.
func TestAPIRoutesAndExportFormatMatchCode(t *testing.T) {
	chdirRepoRoot(t)
	if fail := checkAPIRoutes(); len(fail) != 0 {
		t.Errorf("checkAPIRoutes: %v", fail)
	}
	if fail := checkExportFormat(); len(fail) != 0 {
		t.Errorf("checkExportFormat: %v", fail)
	}
	if fail := checkGlossaryEnums(); len(fail) != 0 {
		t.Errorf("checkGlossaryEnums: %v", fail)
	}
}

// TestGeneratedPagesMatchDisk: renderConfiguration/renderCLI must produce
// exactly what's committed — this is the drift gate `-check` runs; a
// change to config.Vars, cli.Commands or the templates without
// `make generate` fails here.
func TestGeneratedPagesMatchDisk(t *testing.T) {
	chdirRepoRoot(t)
	for path, render := range map[string]func() string{
		configurationPath: renderConfiguration,
		cliPath:           renderCLI,
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if want := render(); string(got) != want {
			t.Errorf("%s is out of date; run `make generate`", path)
		}
	}
}

func chdirRepoRoot(t *testing.T) {
	t.Helper()
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	t.Chdir(strings.TrimSpace(string(root)))
}
