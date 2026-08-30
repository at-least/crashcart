package web

import (
	"errors"
	"github.com/crashcartapp/crashcart/internal/metrics"
	"net/http"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"github.com/jackc/pgx/v5"

	"github.com/crashcartapp/crashcart/internal/auth"
	"github.com/crashcartapp/crashcart/internal/db/sqlc"
)

// ── sign in ──

func (w *Web) loginPage(rw http.ResponseWriter, r *http.Request) {
	if n, _ := w.Store.CountUsers(r.Context()); n == 0 {
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
	u, err := w.Store.GetUserByEmail(r.Context(), email)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		w.fail(rw, r, err)
		return
	}
	// The same failure for an unknown email and a wrong password.
	if err != nil || !auth.CheckPassword(u.PasswordHash, r.PostForm.Get("password")) {
		LoginFailures.Inc()
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

// LoginFailures counts wrong email / password attempts (brute force shows
// here before it shows anywhere else).
var LoginFailures = metrics.NewCounter("crashcart_web_login_failures_total", "Sign-in attempts refused (unknown email or wrong password).")

func (w *Web) logout(rw http.ResponseWriter, r *http.Request) {
	http.SetCookie(rw, w.access.Logout(r.Context(), r))
	redirect(rw, r, "/login")
}

// safeNext keeps the post-login target on this site.
func safeNext(s string) string {
	if s == "" || !strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") {
		return "/"
	}
	return s
}

// ── first user ──

func (w *Web) setupPage(rw http.ResponseWriter, r *http.Request) {
	if n, _ := w.Store.CountUsers(r.Context()); n > 0 {
		redirect(rw, r, "/login")
		return
	}
	w.render(rw, r, withChildren(Layout("Set up · CrashCart"), Setup("")))
}

func (w *Web) setup(rw http.ResponseWriter, r *http.Request) {
	if n, _ := w.Store.CountUsers(r.Context()); n > 0 {
		redirect(rw, r, "/login")
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(rw, "bad form", http.StatusBadRequest)
		return
	}
	u, msg, err := w.createUser(r, r.PostForm.Get("email"), r.PostForm.Get("name"), r.PostForm.Get("password"))
	if err != nil {
		w.fail(rw, r, err)
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
func (w *Web) createUser(r *http.Request, email, name, password string) (sqlc.User, string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") || len(email) > 254 {
		return sqlc.User{}, "A valid email is required.", nil
	}
	if len(password) < 10 {
		return sqlc.User{}, "The password needs at least 10 characters.", nil
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return sqlc.User{}, "", err
	}
	u, err := w.Store.CreateUser(r.Context(), sqlc.CreateUserParams{Email: email, Name: strings.TrimSpace(name), PasswordHash: hash})
	if err != nil && strings.Contains(err.Error(), "users_email_key") {
		return sqlc.User{}, "That email already has an account.", nil
	}
	return u, "", err
}

// ── account: users and API keys ──

// AccountData feeds the account page.
type AccountData struct {
	Users     []sqlc.User
	Keys      []sqlc.ListAPIKeysRow
	NewSecret string // an API key just created: shown once
	Error     string
}

func (w *Web) account(rw http.ResponseWriter, r *http.Request) {
	w.accountPage(rw, r, AccountData{})
}

func (w *Web) accountPage(rw http.ResponseWriter, r *http.Request, d AccountData) {
	var err error
	if d.Users, err = w.Store.ListUsers(r.Context()); err != nil {
		w.fail(rw, r, err)
		return
	}
	if d.Keys, err = w.Store.ListAPIKeys(r.Context()); err != nil {
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
	_, msg, err := w.createUser(r, r.PostForm.Get("email"), r.PostForm.Get("name"), r.PostForm.Get("password"))
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
	if _, err := w.Store.DeleteUser(r.Context(), id); err != nil {
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
	if _, err := w.Store.RevokeAPIKey(r.Context(), id); err != nil {
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
