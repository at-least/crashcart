package web

import (
	"embed"
	"hash/fnv"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// Static assets: compiled Tailwind+shadless stylesheet, vendored htmx,
// Chart.js, the shadless runtimes and the viewer's own app.js.
//
//go:embed assets/*
var assetFS embed.FS

// assetVersions maps file name → content hash, used as a cache-busting
// query so a deploy is visible immediately despite max-age caching.
var assetVersions = func() map[string]string {
	out := map[string]string{}
	entries, _ := assetFS.ReadDir("assets")
	for _, e := range entries {
		b, err := assetFS.ReadFile("assets/" + e.Name())
		if err != nil {
			continue
		}
		h := fnv.New32a()
		_, _ = h.Write(b)
		out[e.Name()] = strconv.FormatUint(uint64(h.Sum32()), 36)
	}
	return out
}()

// assetURL is "/static/<name>?v=<hash>".
func assetURL(name string) string {
	return "/static/" + name + "?v=" + assetVersions[name]
}

// serveAsset handles GET /static/{name}.
func serveAsset(w http.ResponseWriter, r *http.Request) {
	name := path.Base(r.URL.Path)
	if _, ok := assetVersions[name]; !ok || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	b, err := assetFS.ReadFile("assets/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := "application/octet-stream"
	switch {
	case strings.HasSuffix(name, ".css"):
		ct = "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		ct = "application/javascript; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(b)
}
