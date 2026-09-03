package auth

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/at-least/crashcart/internal/store"
	"github.com/at-least/crashcart/internal/testdb"
)

// TestSessionStorage: the cookie carries the token, the row holds only its
// sha256; a session past expires_at is refused; Logout deletes the row.
func TestSessionStorage(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	hash, _ := HashPassword("correct horse battery")
	u, err := store.CreateUser(ctx, st.Pool, "a@example.com", "", hash)
	if err != nil {
		t.Fatal(err)
	}
	a := &Access{Store: st}
	c, err := a.Login(ctx, httptest.NewRequest("GET", "/", nil), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != SessionCookie || len(c.Value) != 64 || !c.HttpOnly || c.MaxAge != int(SessionTTL/time.Second) {
		t.Fatalf("cookie = %+v", c)
	}
	var stored []byte
	var expires time.Time
	if err := st.Pool.QueryRow(ctx, "SELECT token_hash, expires_at FROM user_sessions WHERE user_id = $1", u.ID).Scan(&stored, &expires); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, HashToken(c.Value)) {
		t.Errorf("token_hash = %x, want sha256 of the cookie value", stored)
	}
	if d := time.Until(expires); d < SessionTTL-time.Minute || d > SessionTTL+time.Minute {
		t.Errorf("expires in %v, want %v", d, SessionTTL)
	}

	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(ActorFrom(r.Context()).Name))
	})
	serve := func(h http.Handler, cookie *http.Cookie, hx bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/p/x", nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		if hx {
			req.Header.Set("HX-Request", "true")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := serve(a.Session(ok), c, false); rec.Code != 200 || rec.Body.String() != "a@example.com" {
		t.Fatalf("live session: %d %q", rec.Code, rec.Body.String())
	}
	// A forged cookie of the right shape is not a session.
	if rec := serve(a.Session(ok), &http.Cookie{Name: SessionCookie, Value: strings.Repeat("0", 64)}, false); rec.Code != 303 || rec.Header().Get("Location") != "/login?next=%2Fp%2Fx" {
		t.Errorf("forged: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec := serve(a.Identify(ok), &http.Cookie{Name: SessionCookie, Value: strings.Repeat("0", 64)}, false); rec.Code != 200 || rec.Body.String() != "" {
		t.Errorf("Identify with a forged cookie must pass through anonymous: %d %q", rec.Code, rec.Body.String())
	}
	// Expired: refused, with HX-Redirect for htmx.
	if _, err := st.Pool.Exec(ctx, "UPDATE user_sessions SET expires_at = now() - INTERVAL '1 second' WHERE user_id = $1", u.ID); err != nil {
		t.Fatal(err)
	}
	if rec := serve(a.Session(ok), c, false); rec.Code != 303 {
		t.Errorf("expired session accepted: %d", rec.Code)
	}
	if rec := serve(a.Session(ok), c, true); rec.Code != 401 || rec.Header().Get("HX-Redirect") != "/login?next=%2Fp%2Fx" {
		t.Errorf("expired session (htmx): %d %v", rec.Code, rec.Header())
	}
	if _, err := st.Pool.Exec(ctx, "UPDATE user_sessions SET expires_at = now() + INTERVAL '1 day' WHERE user_id = $1", u.ID); err != nil {
		t.Fatal(err)
	}
	if rec := serve(a.Session(ok), c, false); rec.Code != 200 {
		t.Fatalf("session back in date: %d", rec.Code)
	}
	// Logout deletes the row and clears the cookie.
	req := httptest.NewRequest("POST", "/logout", nil)
	req.AddCookie(c)
	if clear := a.Logout(ctx, req); clear.MaxAge != -1 || clear.Value != "" {
		t.Errorf("clearing cookie = %+v", clear)
	}
	var n int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM user_sessions WHERE user_id = $1", u.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("sessions after logout = %d %v", n, err)
	}
	if rec := serve(a.Session(ok), c, false); rec.Code != 303 {
		t.Errorf("cookie still works after logout: %d", rec.Code)
	}
	// Password helpers.
	if !CheckPassword(hash, "correct horse battery") || CheckPassword(hash, "wrong") || CheckPassword("not a hash", "x") {
		t.Error("CheckPassword")
	}
}

// TestAPIKeyStorage: the secret is shown once and stored as its sha256
// next to a display prefix; last_used_at is written at most once a minute;
// a revoked key is refused.
func TestAPIKeyStorage(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	a := &Access{Store: st}
	row, secret, err := a.CreateAPIKey(ctx, "ci", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, KeyPrefix) || len(secret) != len(KeyPrefix)+64 {
		t.Fatalf("secret = %q", secret)
	}
	if row.Prefix != secret[:len(KeyPrefix)+8] {
		t.Errorf("prefix = %q, want the first %d characters of the secret", row.Prefix, len(KeyPrefix)+8)
	}
	var stored []byte
	if err := st.Pool.QueryRow(ctx, "SELECT key_hash FROM api_keys WHERE id = $1", row.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, HashToken(secret)) {
		t.Errorf("key_hash = %x, want sha256 of the secret", stored)
	}
	var cols string
	if err := st.Pool.QueryRow(ctx, "SELECT row_to_json(k)::text FROM api_keys k WHERE id = $1", row.ID).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cols, secret[len(KeyPrefix)+8:]) {
		t.Error("the secret must not be stored in any column")
	}

	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(ActorFrom(r.Context()).Name)) })
	call := func(tok string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/projects", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		rec := httptest.NewRecorder()
		a.APIKey(ok).ServeHTTP(rec, req)
		return rec
	}
	if rec := call(""); rec.Code != 401 || rec.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("no key: %d %v", rec.Code, rec.Header())
	}
	if rec := call(KeyPrefix + strings.Repeat("0", 64)); rec.Code != 401 {
		t.Errorf("unknown key: %d", rec.Code)
	}
	if rec := call(secret); rec.Code != 200 || rec.Body.String() != "ci" {
		t.Fatalf("key: %d %q", rec.Code, rec.Body.String())
	}
	lastUsed := func() *time.Time {
		var t0 *time.Time
		if err := st.Pool.QueryRow(ctx, "SELECT last_used_at FROM api_keys WHERE id = $1", row.ID).Scan(&t0); err != nil {
			t.Fatal(err)
		}
		return t0
	}
	first := lastUsed()
	if first == nil {
		t.Fatal("last_used_at not written on first use")
	}
	call(secret)
	if second := lastUsed(); !second.Equal(*first) {
		t.Errorf("last_used_at rewritten within a minute: %v → %v", first, second)
	}
	if _, err := st.Pool.Exec(ctx, "UPDATE api_keys SET last_used_at = now() - INTERVAL '2 minutes' WHERE id = $1", row.ID); err != nil {
		t.Fatal(err)
	}
	stale := lastUsed()
	call(secret)
	if third := lastUsed(); !third.After(*stale) {
		t.Errorf("last_used_at not refreshed after a minute: %v → %v", stale, third)
	}
	if n, err := store.RevokeAPIKey(ctx, st.Pool, row.ID); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if rec := call(secret); rec.Code != 401 {
		t.Errorf("revoked key accepted: %d", rec.Code)
	}
	if n, _ := store.RevokeAPIKey(ctx, st.Pool, row.ID); n != 0 {
		t.Errorf("revoking twice touched %d rows", n)
	}
}
