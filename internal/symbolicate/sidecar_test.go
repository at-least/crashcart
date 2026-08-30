package symbolicate

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeSymbolizer writes a script that answers like llvm-symbolizer
// --output-style=JSON: it records its arguments and prints one entry per
// address, resolving 0x10 to a function and everything else to nothing.
func fakeSymbolizer(t *testing.T) (bin, argsFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	bin = filepath.Join(dir, "llvm-symbolizer")
	script := `#!/bin/sh
printf '%s\n' "$@" > ` + argsFile + `
shift 2
printf '['
sep=''
for a in "$@"; do
  if [ "$a" = "0x10" ] || [ "$a" = "0x100000010" ]; then
    printf '%s{"Address":"0x10","ModuleName":"App","Symbol":[{"FunctionName":"-[Cart load]","FileName":"/src/Cart.m","Line":7},{"FunctionName":"outer","FileName":"/src/Outer.m","Line":1}]}' "$sep"
  else
    printf '%s{"Address":"%s","ModuleName":"App","Symbol":[{"FunctionName":"","FileName":"","Line":0}]}' "$sep" "$a"
  fi
  sep=','
done
printf ']\n'
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsFile
}

func TestSidecarProtocol(t *testing.T) {
	bin, argsFile := fakeSymbolizer(t)
	sc := &Sidecar{Dir: filepath.Join(t.TempDir(), "cache"), Symbolizer: bin}
	srv := httptest.NewServer(sc.Handler())
	defer srv.Close()
	c := NewDSYMClient(srv.URL)
	addrs := []DSYMAddr{{Address: 0x10, Module: "App"}, {Address: 0x20, Module: "App"}}

	// Unknown key, no loader: ErrNotCached, nothing written.
	if _, err := c.Resolve(context.Background(), "7-100", nil, addrs); err != ErrNotCached {
		t.Fatalf("cold, no loader: %v", err)
	}
	// With a loader: the bytes are sent once, then resolved.
	loads := 0
	load := func(context.Context) ([]byte, error) { loads++; return []byte("MACHO"), nil }
	res, err := c.Resolve(context.Background(), "7-100", load, addrs)
	if err != nil || loads != 1 {
		t.Fatalf("cold, loader: %v loads=%d", err, loads)
	}
	if len(res) != 2 || res[0].Function != "-[Cart load]" || res[0].Filename != "/src/Cart.m" || res[0].Lineno != 7 || res[1].Resolved() {
		t.Fatalf("results = %+v", res)
	}
	if data, _ := os.ReadFile(filepath.Join(sc.Dir, "7-100")); string(data) != "MACHO" {
		t.Fatalf("cached file = %q", data)
	}
	args, _ := os.ReadFile(argsFile)
	if want := "--output-style=JSON\n--obj=" + filepath.Join(sc.Dir, "7-100") + "\n0x10\n0x20\n"; string(args) != want {
		t.Fatalf("llvm-symbolizer args = %q", args)
	}
	// Warm: no load.
	if _, err := c.Resolve(context.Background(), "7-100", load, addrs); err != nil || loads != 1 {
		t.Fatalf("warm: %v loads=%d", err, loads)
	}

	// A Mach-O file linked at 0x100000000: offsets are asked as linked
	// addresses (the fake resolves 0x100000010; llvm-symbolizer resolves
	// nothing for a bare offset in an iOS dSYM).
	img := fakeMachOText(0x0100000c, 0x100000000)
	res, err = c.Resolve(context.Background(), "8-200", func(context.Context) ([]byte, error) { return img, nil }, addrs)
	if err != nil {
		t.Fatal(err)
	}
	args, _ = os.ReadFile(argsFile)
	if !strings.HasSuffix(string(args), "\n0x100000010\n0x100000020\n") {
		t.Fatalf("llvm-symbolizer args for a Mach-O = %q", args)
	}
	if len(res) != 2 || res[0].Function != "-[Cart load]" || res[1].Resolved() {
		t.Fatalf("results = %+v", res)
	}

	// Bad keys are refused (a leading dot would name a temp file the eviction skips).
	for _, key := range []string{"../etc", "a b", "", ".hidden"} {
		req, _ := http.NewRequest(http.MethodPut, srv.URL+"/symbols/"+key, strings.NewReader("x"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNoContent {
			t.Errorf("PUT %q accepted", key)
		}
	}
	body, _ := json.Marshal(map[string]any{"symbol": "../x", "frames": []map[string]string{{"address": "0x10"}}})
	resp, err := http.Post(srv.URL+"/symbolicate", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad key: %d", resp.StatusCode)
	}
}

func TestSidecarEviction(t *testing.T) {
	bin, _ := fakeSymbolizer(t)
	sc := &Sidecar{Dir: filepath.Join(t.TempDir(), "cache"), Symbolizer: bin, MaxBytes: 25}
	srv := httptest.NewServer(sc.Handler())
	defer srv.Close()
	put := func(key string, size int) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPut, srv.URL+"/symbols/"+key, bytes.NewReader(bytes.Repeat([]byte("x"), size)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusNoContent {
			t.Fatalf("PUT %s: %v %v", key, err, resp.Status)
		}
		resp.Body.Close()
	}
	put("a", 10)
	os.Chtimes(filepath.Join(sc.Dir, "a"), time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour))
	put("b", 10)
	os.Chtimes(filepath.Join(sc.Dir, "b"), time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	// Using "a" makes it the most recent; the next PUT evicts "b".
	c := NewDSYMClient(srv.URL)
	if _, err := c.Resolve(context.Background(), "a", nil, []DSYMAddr{{Address: 0x10}}); err != nil {
		t.Fatal(err)
	}
	put("c", 10)
	entries, _ := os.ReadDir(sc.Dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "a,c" {
		t.Fatalf("cache after eviction = %v", names)
	}
}

func TestParseSymbolizerJSON(t *testing.T) {
	// One entry per line (older LLVM) with an error entry.
	out := `{"Address":"0x10","Symbol":[{"FunctionName":"f","FileName":"a.c","Line":3}]}
{"Address":"0x20","Error":{"Message":"no such address"}}
`
	res, err := parseSymbolizerJSON([]byte(out), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 || res[0].Function != "f" || res[0].Lineno != 3 || res[1].Resolved() || res[2].Resolved() {
		t.Fatalf("results = %+v", res)
	}
	if _, err := parseSymbolizerJSON([]byte("not json"), 1); err == nil {
		t.Fatal("garbage must be an error")
	}
}
