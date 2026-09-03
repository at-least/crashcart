package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"github.com/jackc/pgx/v5"

	"github.com/at-least/crashcart/internal/auth"
	"github.com/at-least/crashcart/internal/store"
)

// ── sign in ──

// LoginRateLimit bounds password posts (sign-in, setup) per client IP and
// minute: enough for a team behind one NAT to mistype, far too few to
// guess a password (each attempt is a bcrypt verification, too).
var LoginRateLimit = 30

func (w *Web) loginPage(rw http.ResponseWriter, r *http.Request) {
	n, err := store.CountUsers(r.Context(), w.Store.Pool)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	if n == 0 {
		redirect(rw, r, "/setup")
		return
	}
	if auth.ActorFrom(r.Context()).Name != "" {
		redirect(rw, r, "/")
		return
	}
	w.render(rw, r, withChildren(Layout("Sign in · CrashCart"), Login(safeNext(r.URL.Query().Get("next")), "")))
}

func (w *Web) login(rw http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(rw, "bad form", http.StatusBadRequest)
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.PostForm.Get("email")))
	next := safeNext(r.PostForm.Get("next"))
	u, err := store.GetUserByEmail(r.Context(), w.Store.Pool, email)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		w.fail(rw, r, err)
		return
	}
	// The same failure — and the same bcrypt cost — for an unknown email
	// and a wrong password, so neither the body nor the latency says
	// whether the address has an account.
	hash := dummyHash
	if err == nil {
		hash = u.PasswordHash
	}
	if !auth.CheckPassword(hash, r.PostForm.Get("password")) || err != nil {
		rw.WriteHeader(http.StatusUnauthorized)
		w.render(rw, r, withChildren(Layout("Sign in · CrashCart"), Login(next, "Wrong email or password.")))
		return
	}
	c, err := w.access.Login(r.Context(), r, u.ID)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	http.SetCookie(rw, c)
	redirect(rw, r, next)
}

// dummyHash is verified against when the email has no account: the same
// work as a real check, so timing does not reveal which emails exist.
var dummyHash = func() string {
	h, err := auth.HashPassword("not a real password")
	if err != nil {
		panic(err)
	}
	return h
}()

func (w *Web) logout(rw http.ResponseWriter, r *http.Request) {
	http.SetCookie(rw, w.access.Logout(r.Context(), r))
	redirect(rw, r, "/login")
}

// safeNext keeps the post-login target on this site: a path, not a URL.
// "//host" is scheme-relative and "/\host" is the same thing to a browser
// (the WHATWG parser treats a backslash as a slash), so both are refused.
func safeNext(s string) string {
	if !strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") || strings.HasPrefix(s, "/\\") {
		return "/"
	}
	if u, err := url.Parse(s); err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return s
}

// ── first user ──

func (w *Web) setupPage(rw http.ResponseWriter, r *http.Request) {
	n, err := store.CountUsers(r.Context(), w.Store.Pool)
	if err != nil { // fail closed: an error must not open the setup form
		w.fail(rw, r, err)
		return
	}
	if n > 0 {
		redirect(rw, r, "/login")
		return
	}
	w.render(rw, r, withChildren(Layout("Set up · CrashCart"), Setup("")))
}

// setup creates the first user. The "no user yet" check and the insert
// are one serialized transaction (store.CreateFirstUser), so two setup
// posts racing on a fresh install cannot both succeed.
func (w *Web) setup(rw http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(rw, "bad form", http.StatusBadRequest)
		return
	}
	u, msg, err := w.createUser(r, r.PostForm.Get("email"), r.PostForm.Get("name"), r.PostForm.Get("password"), true)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	if u.ID == 0 && msg == "" { // someone else got there first
		redirect(rw, r, "/login")
		return
	}
	if msg != "" {
		rw.WriteHeader(http.StatusBadRequest)
		w.render(rw, r, withChildren(Layout("Set up · CrashCart"), Setup(msg)))
		return
	}
	c, err := w.access.Login(r.Context(), r, u.ID)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	http.SetCookie(rw, c)
	redirect(rw, r, "/")
}

// createUser validates and creates a user; msg is the user-facing problem.
// With first set the user is created only while there is none (a zero
// user and no msg means one already existed).
func (w *Web) createUser(r *http.Request, email, name, password string, first bool) (store.User, string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") || len(email) > 254 {
		return store.User{}, "A valid email is required.", nil
	}
	if len(password) < 10 {
		return store.User{}, "The password needs at least 10 characters.", nil
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return store.User{}, "", err
	}
	trimmedName := strings.TrimSpace(name)
	var u store.User
	if first {
		u, _, err = w.Store.CreateFirstUser(r.Context(), email, trimmedName, hash)
	} else {
		u, err = store.CreateUser(r.Context(), w.Store.Pool, email, trimmedName, hash)
	}
	if err != nil && strings.Contains(err.Error(), "users_email_key") {
		return store.User{}, "That email already has an account.", nil
	}
	return u, "", err
}

// ── account: users and API keys ──

// AccountData feeds the account page.
type AccountData struct {
	Users     []store.User
	Keys      []store.APIKey
	NewSecret string // an API key just created: shown once
	Error     string
}

func (w *Web) account(rw http.ResponseWriter, r *http.Request) {
	w.accountPage(rw, r, AccountData{})
}

func (w *Web) accountPage(rw http.ResponseWriter, r *http.Request, d AccountData) {
	var err error
	if d.Users, err = store.ListUsers(r.Context(), w.Store.Pool); err != nil {
		w.fail(rw, r, err)
		return
	}
	if d.Keys, err = store.ListAPIKeys(r.Context(), w.Store.Pool); err != nil {
		w.fail(rw, r, err)
		return
	}
	pg := Page{S: ViewState{Filters: map[string]string{}}, Section: "account"}
	w.page(rw, r, pg, func(pg Page) templ.Component { return Account(pg, d) })
}

func (w *Web) accountUserAdd(rw http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(rw, "bad form", http.StatusBadRequest)
		return
	}
	_, msg, err := w.createUser(r, r.PostForm.Get("email"), r.PostForm.Get("name"), r.PostForm.Get("password"), false)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	if msg != "" {
		w.accountPage(rw, r, AccountData{Error: msg})
		return
	}
	redirect(rw, r, "/account")
}

func (w *Web) accountUserDelete(rw http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(rw, r)
		return
	}
	if id == auth.ActorFrom(r.Context()).UserID {
		http.Error(rw, "you cannot remove your own account", http.StatusBadRequest)
		return
	}
	if _, err := store.DeleteUser(r.Context(), w.Store.Pool, id); err != nil {
		w.fail(rw, r, err)
		return
	}
	redirect(rw, r, "/account")
}

func (w *Web) accountKeyCreate(rw http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(rw, "bad form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if name == "" || len(name) > 80 {
		w.accountPage(rw, r, AccountData{Error: "The key needs a name (what will use it)."})
		return
	}
	actor := auth.ActorFrom(r.Context())
	_, secret, err := w.access.CreateAPIKey(r.Context(), name, &actor.UserID)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	// The secret is shown on this response only; it is stored hashed.
	w.accountPage(rw, r, AccountData{NewSecret: secret})
}

func (w *Web) accountKeyRevoke(rw http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(rw, r)
		return
	}
	if _, err := store.RevokeAPIKey(r.Context(), w.Store.Pool, id); err != nil {
		w.fail(rw, r, err)
		return
	}
	redirect(rw, r, "/account")
}

// actorName is who is acting (the signed-in user's email), for issues.status_by.
func actorName(r *http.Request) *string {
	if a := auth.ActorFrom(r.Context()); a.Name != "" {
		return &a.Name
	}
	return nil
}
