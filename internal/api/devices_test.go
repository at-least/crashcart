package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/crashcartapp/crashcart/internal/auth"
)

// doAs is e.do with an explicit key, for cross-key ownership checks.
func (e *env) doAs(key, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Host = "crash.example.com"
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 && strings.HasPrefix(rec.Body.String(), "{") {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			e.t.Fatalf("%s %s: bad JSON %q", method, path, rec.Body.String())
		}
	}
	return rec, out
}

func TestDevices(t *testing.T) {
	e := newEnv(t)
	e.createProject("demo")
	_, otherKey, err := (&auth.Access{Store: e.st}).CreateAPIKey(context.Background(), "other", nil)
	if err != nil {
		t.Fatal(err)
	}

	if rec, _ := e.do("POST", "/api/devices", map[string]any{"token": "", "platform": "ios"}); rec.Code != 400 {
		t.Errorf("empty token: %d", rec.Code)
	}
	if rec, _ := e.do("POST", "/api/devices", map[string]any{"token": "tok-1", "platform": "windows"}); rec.Code != 400 {
		t.Errorf("bad platform: %d", rec.Code)
	}
	rec, out := e.do("POST", "/api/devices", map[string]any{"token": "tok-1", "platform": "ios"})
	if rec.Code != 201 || out["platform"] != "ios" {
		t.Fatalf("register: %d %v", rec.Code, out)
	}
	id := int64(out["id"].(float64))
	if _, ok := out["token"]; ok {
		t.Errorf("token leaked back in the response: %v", out)
	}

	// A different key cannot subscribe, unsubscribe or delete this device.
	if rec, _ := e.doAs(otherKey, "POST", "/api/projects/demo/devices/"+itoa(id), nil); rec.Code != 404 {
		t.Errorf("subscribe by another key: %d", rec.Code)
	}
	if rec, _ := e.doAs(otherKey, "DELETE", "/api/projects/demo/devices/"+itoa(id), nil); rec.Code != 404 {
		t.Errorf("unsubscribe by another key: %d", rec.Code)
	}
	if rec, _ := e.doAs(otherKey, "DELETE", "/api/devices/"+itoa(id), nil); rec.Code != 404 {
		t.Errorf("delete by another key: %d", rec.Code)
	}

	if rec, _ := e.do("POST", "/api/projects/demo/devices/"+itoa(id), nil); rec.Code != 204 {
		t.Errorf("subscribe: %d", rec.Code)
	}
	if rec, _ := e.do("POST", "/api/projects/bogus/devices/"+itoa(id), nil); rec.Code != 404 {
		t.Errorf("subscribe to a missing project: %d", rec.Code)
	}
	if rec, _ := e.do("DELETE", "/api/projects/demo/devices/"+itoa(id), nil); rec.Code != 204 {
		t.Errorf("unsubscribe: %d", rec.Code)
	}
	if rec, _ := e.do("DELETE", "/api/projects/demo/devices/"+itoa(id), nil); rec.Code != 404 {
		t.Errorf("unsubscribe again: %d", rec.Code)
	}
	if rec, _ := e.do("DELETE", "/api/devices/"+itoa(id), nil); rec.Code != 204 {
		t.Errorf("delete: %d", rec.Code)
	}
	if rec, _ := e.do("DELETE", "/api/devices/"+itoa(id), nil); rec.Code != 404 {
		t.Errorf("delete again: %d", rec.Code)
	}

	// Re-registering the same token updates the existing row (no
	// duplicate device, no double-send).
	rec, out = e.do("POST", "/api/devices", map[string]any{"token": "tok-1", "platform": "ios"})
	rec2, out2 := e.do("POST", "/api/devices", map[string]any{"token": "tok-1", "platform": "android"})
	if rec.Code != 201 || rec2.Code != 201 || out2["id"] != out["id"] || out2["platform"] != "android" {
		t.Errorf("re-register: first=%v second=%v", out, out2)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
