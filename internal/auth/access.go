package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/crashcartapp/crashcart/internal/store"
)

// Actor is who is making a request: a signed-in user (viewer) or an API
// key (/api/*). Name is what gets recorded (issues.status_by).
type Actor struct {
	UserID int64  // 0 for an API key
	KeyID  int64  // 0 for a user
	Name   string // user email, or the key's name
}

type actorKey struct{}

// ActorFrom returns the request's actor (zero when unauthenticated).
func ActorFrom(ctx context.Context) Actor {
	a, _ := ctx.Value(actorKey{}).(Actor)
	return a
}

// WithActor is for tests and internal callers.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// SessionCookie is the viewer's session cookie name.
const SessionCookie = "crashcart_session"

// SessionTTL is how long a login lasts.
const SessionTTL = 30 * 24 * time.Hour

// KeyPrefix starts every API key secret, so one is recognizable in logs and
// config files.
const KeyPrefix = "cc_"

// Access checks credentials against the database.
type Access struct {
	Store      *store.Store
	TrustProxy bool // TRUST_PROXY: X-Forwarded-Proto decides the cookie's Secure flag
}

// NewToken is a fresh random secret (hex).
func NewToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// HashToken is how session tokens and API key secrets are stored.
func HashToken(tok string) []byte {
	sum := sha256.Sum256([]byte(tok))
	return sum[:]
}

// HashPassword is bcrypt at the default cost.
func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h), err
}

// CheckPassword reports whether pw matches the stored hash.
func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// ── API keys ──

// CreateAPIKey makes a key and returns its row and the secret (shown once).
func (a *Access) CreateAPIKey(ctx context.Context, name string, createdBy *int64) (store.APIKey, string, error) {
	secret := KeyPrefix + NewToken()
	row, err := store.CreateAPIKey(ctx, a.Store.Pool, name, HashToken(secret), secret[:len(KeyPrefix)+8], createdBy)
	return row, secret, err
}

// APIKey requires `Authorization: Bearer <key>` naming a live api_keys row
// and puts the key on the context as the actor.
func (a *Access) APIKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
		if tok == "" {
			unauthorized(w)
			return
		}
		k, err := store.GetAPIKeyByHash(r.Context(), a.Store.Pool, HashToken(tok))
		if errors.Is(err, pgx.ErrNoRows) {
			unauthorized(w)
			return
		}
		if err != nil {
			http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
			return
		}
		store.TouchAPIKey(r.Context(), a.Store.Pool, k.ID)
		next.ServeHTTP(w, r.WithContext(WithActor(r.Context(), Actor{KeyID: k.ID, Name: k.Name})))
	})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="crashcart"`)
	http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
}

// ── viewer sessions ──

// Login creates a session for the user and returns the cookie to set.
func (a *Access) Login(ctx context.Context, r *http.Request, userID int64) (*http.Cookie, error) {
	tok := NewToken()
	if err := store.CreateUserSession(ctx, a.Store.Pool, HashToken(tok), userID, time.Now().Add(SessionTTL)); err != nil {
		return nil, err
	}
	return a.cookie(r, tok, int(SessionTTL/time.Second)), nil
}

// Logout deletes the request's session and returns the clearing cookie.
func (a *Access) Logout(ctx context.Context, r *http.Request) *http.Cookie {
	if c, err := r.Cookie(SessionCookie); err == nil && c.Value != "" {
		store.DeleteUserSession(ctx, a.Store.Pool, HashToken(c.Value))
	}
	return a.cookie(r, "", -1)
}

func (a *Access) cookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: SessionCookie, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: Scheme(r, a.TrustProxy) == "https",
	}
}

// Scheme is the scheme the client used: "https" behind TLS or — behind a
// trusted reverse proxy — when X-Forwarded-Proto says so; "http" otherwise.
// The header is honoured only with TRUST_PROXY, like X-Forwarded-For.
func Scheme(r *http.Request, trustProxy bool) string {
	if r.TLS != nil {
		return "https"
	}
	if fp := r.Header.Get("X-Forwarded-Proto"); trustProxy && fp != "" {
		if strings.EqualFold(strings.TrimSpace(strings.Split(fp, ",")[0]), "https") {
			return "https"
		}
	}
	return "http"
}

// BaseURL is the externally visible origin: publicURL when set, otherwise
// the request's scheme and host.
func BaseURL(r *http.Request, publicURL string, trustProxy bool) string {
	if publicURL != "" {
		return strings.TrimSuffix(publicURL, "/")
	}
	return Scheme(r, trustProxy) + "://" + r.Host
}

// DSN renders `<scheme>://<public_key>@<host>/<id>` on base (an origin,
// scheme included — an origin without one is taken as http).
func DSN(base, publicKey string, projectID int64) string {
	scheme, host, ok := strings.Cut(strings.TrimSuffix(base, "/"), "://")
	if !ok {
		scheme, host = "http", scheme
	}
	return scheme + "://" + publicKey + "@" + host + "/" + strconv.FormatInt(projectID, 10)
}

// NewProjectKey returns a random DSN public key (32 hex characters).
func NewProjectKey() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Identify puts the user on the context when a live session cookie is
// present and passes through otherwise — for the public pages, which
// behave differently for a signed-in user but never require one.
func (a *Access) Identify(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(SessionCookie); err == nil && c.Value != "" {
			if u, err := store.GetUserSession(r.Context(), a.Store.Pool, HashToken(c.Value)); err == nil {
				r = r.WithContext(WithActor(r.Context(), Actor{UserID: u.ID, Name: u.Email}))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Session requires a live session cookie and puts the user on the context.
// Without one, a browser is sent to /login (or /setup while no user exists);
// an htmx request gets 401 with HX-Redirect so the page navigates.
func (a *Access) Session(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(SessionCookie); err == nil && c.Value != "" {
			u, err := store.GetUserSession(r.Context(), a.Store.Pool, HashToken(c.Value))
			if err == nil {
				next.ServeHTTP(w, r.WithContext(WithActor(r.Context(), Actor{UserID: u.ID, Name: u.Email})))
				return
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
		to := "/login"
		if n, err := store.CountUsers(r.Context(), a.Store.Pool); err == nil && n == 0 {
			to = "/setup"
		} else if r.Method == http.MethodGet && r.URL.Path != "/" {
			to += "?next=" + url.QueryEscape(r.URL.RequestURI())
		}
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", to)
			http.Error(w, "sign in required", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, to, http.StatusSeeOther)
	})
}
