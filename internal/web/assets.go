package web

import (
	"embed"
	"hash/fnv"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// Static assets: compiled Tailwind+shadless stylesheet, vendored htmx, the
// shadless runtimes and the viewer's own app.js.
//
//go:embed assets/*
var assetFS embed.FS

// assetVersions maps file name → content hash: the ETag and the cache-bust
// query so a deploy is visible immediately despite long max-age caching.
var assetVersions = func() map[string]string {
	out := map[string]string{}
	entries, _ := assetFS.ReadDir("assets")
	for _, e := range entries {
		b, err := assetFS.ReadFile("assets/" + e.Name())
		if err != nil {
			continue
		}
		h := fnv.New64a()
		_, _ = h.Write(b)
		out[e.Name()] = strconv.FormatUint(h.Sum64(), 36)
	}
	return out
}()

// assetURL is "/static/<name>?v=<hash>".
func assetURL(name string) string { return "/static/" + name + "?v=" + assetVersions[name] }

// serveAsset handles GET /static/{file}.
func serveAsset(w http.ResponseWriter, r *http.Request) {
	name := path.Base(r.PathValue("file"))
	v, ok := assetVersions[name]
	if !ok || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	etag := `"` + v + `"`
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
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
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(b)
}
