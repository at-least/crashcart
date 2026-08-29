package api

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

func sha(b []byte) string {
	s := sha1.Sum(b)
	return hex.EncodeToString(s[:])
}

// TestChunkUpload walks the protocol the way sentry-cli 3 does: options →
// assemble (not_found + missing) → chunk POST → assemble (ok).
func TestChunkUpload(t *testing.T) {
	e := newEnv(t)
	e.createProject("app")

	_, opts := e.do("GET", "/api/0/organizations/o/chunk-upload/", nil)
	if opts["hashAlgorithm"] != "sha1" || opts["url"] != "http://crash.example.com/api/0/organizations/o/chunk-upload/" {
		t.Fatalf("options = %v", opts)
	}

	mapping := []byte("com.example.Foo -> a.b:\n    void bar() -> c\n")
	c1, c2 := mapping[:20], mapping[20:]
	file := sha(mapping)
	req := map[string]assembleRequest{file: {Name: "mapping.txt", Chunks: []string{sha(c1), sha(c2)}, DebugID: "564CA29D-9553-5CDA-B46B-135303369724"}}

	rec, _ := e.do("POST", "/api/0/projects/o/app/files/difs/assemble/", req)
	var res map[string]assembleResponse
	json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != 200 || res[file].State != "not_found" || len(res[file].MissingChunks) != 2 {
		t.Fatalf("assemble before upload: %d %s", rec.Code, rec.Body.String())
	}

	// Upload the chunks (one bad checksum must be rejected).
	post := func(parts map[string][]byte) int {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for name, data := range parts {
			fw, _ := mw.CreateFormFile("file", name)
			fw.Write(data)
		}
		mw.Close()
		r := httptest.NewRequest("POST", "/api/0/organizations/o/chunk-upload/", &buf)
		r.Header.Set("Authorization", "Bearer "+e.key)
		r.Header.Set("Content-Type", mw.FormDataContentType())
		w := httptest.NewRecorder()
		e.mux.ServeHTTP(w, r)
		return w.Code
	}
	if code := post(map[string][]byte{sha(c1): c2}); code != http.StatusBadRequest {
		t.Fatalf("mismatched chunk accepted: %d", code)
	}
	if code := post(map[string][]byte{sha(c1): c1, sha(c2): c2}); code != http.StatusOK {
		t.Fatalf("chunk upload: %d", code)
	}

	rec, _ = e.do("POST", "/api/0/projects/o/app/files/difs/assemble/", req)
	json.Unmarshal(rec.Body.Bytes(), &res)
	if res[file].State != "ok" || res[file].Dif == nil || res[file].Dif.DebugID != "564ca29d-9553-5cda-b46b-135303369724" || res[file].Dif.SymbolType != "proguard" {
		t.Fatalf("assemble: %s", rec.Body.String())
	}
	if n, _ := e.st.CountJobs(t.Context()); n != 0 { // release "" → no resymbolicate job
		t.Fatalf("jobs = %d", n)
	}
	syms := e.get("/api/projects/app/symbols", 200)["symbols"].([]any)
	if len(syms) != 1 || syms[0].(map[string]any)["debug_id"] != "564ca29d-9553-5cda-b46b-135303369724" {
		t.Fatalf("stored = %v", syms)
	}
	var chunks int
	e.st.Pool.QueryRow(t.Context(), "SELECT count(*) FROM upload_chunks").Scan(&chunks)
	if chunks != 0 {
		t.Fatalf("chunks not cleaned up: %d", chunks)
	}
	// The Gradle plugin's follow-up association tags the release.
	rec, _ = e.do("POST", "/api/0/projects/o/app/files/dsyms/associate/", map[string]any{"checksums": []string{file}, "appId": "com.example", "version": "2.4.1", "build": "9"})
	if rec.Code != 200 {
		t.Fatalf("associate: %d %s", rec.Code, rec.Body.String())
	}
	if syms := e.get("/api/projects/app/symbols", 200)["symbols"].([]any); syms[0].(map[string]any)["release"] != "com.example@2.4.1+9" {
		t.Fatalf("release after associate = %v", syms[0])
	}
	// sentry-cli's pre-check by debug id finds it, so it is not re-uploaded.
	rec, _ = e.do("GET", "/api/0/projects/o/app/files/dsyms/?debug_id=564ca29d-9553-5cda-b46b-135303369724", nil)
	if rec.Code != 200 || !bytes.Contains(rec.Body.Bytes(), []byte(`"objectName":"mapping.txt"`)) {
		t.Fatalf("dsyms lookup: %d %s", rec.Code, rec.Body.String())
	}
}
