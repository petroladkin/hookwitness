package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/petroladkin/hookwitness/internal/access"
	"github.com/petroladkin/hookwitness/internal/api"
	"github.com/petroladkin/hookwitness/internal/store"
)

// loginView is the login page.
type loginView struct {
	On   bool // login is on; off: the page says so and links on
	Next string
	Name string
	Err  string
	// Providers to sign in with (login on); Elsewhere, when the page was
	// opened on another host than the admin's origin, is that origin's login
	// page, which the buttons link to; Moved says a sign-in was sent here.
	Providers []providerButton
	Elsewhere string
	Moved     string
}

type providerButton struct{ Name, Label string }

// loginView is the login page's view with the providers' buttons.
func (h *Handler) loginView(r *http.Request, next, name, msg string) loginView {
	on, _ := h.uiLoginOn(r.Context())
	v := loginView{On: on, Next: next, Name: name, Err: msg}
	if !on {
		return v
	}
	for _, p := range h.oidc {
		v.Providers = append(v.Providers, providerButton{Name: p.Name, Label: p.Label})
	}
	if h.adminHost != "" && hostName(r.Host) != h.adminHost {
		v.Elsewhere = h.adminOrigin + h.prefix + "/login?next=" + url.QueryEscape(next)
	}
	if r.URL.Query().Get("moved") != "" && len(v.Providers) > 0 {
		v.Moved = "Sign in here, at " + h.adminOrigin + ": a sign-in through a provider starts on the admin's own address."
	}
	return v
}

// accountView is the account page: one's tokens and password.
type accountView struct {
	Tokens   []store.Token
	Dialog   string // "token" or "password"
	Form     string // the token name typed
	Err      string
	Saved    string
	NewToken string // a token just made, shown once
	NewName  string
	// Sign-in: the email an admin set, whether a password is set, the
	// linked identities; Fresh: the session is young enough to set a first
	// password.
	Email       string
	HasPassword bool
	Fresh       bool
	Identities  []identityRow
}

// identityRow is a linked identity as the pages show it.
type identityRow struct {
	Issuer, Label, Email string
	LastLogin            time.Time
	Unlink               bool   // may be unlinked here
	Why                  string // why not
}

// label names an identity's provider: the configured label of its issuer,
// else the name it was signed in with, else the issuer.
func (h *Handler) label(id store.Identity) string {
	for _, p := range h.oidc {
		if p.Issuer == id.Issuer {
			return p.Label
		}
	}
	if id.Provider != "" {
		return id.Provider
	}
	return id.Issuer
}

// identityRows: own identities (self) may be unlinked only when another way
// in is left; an admin may unlink anyone else's.
func (h *Handler) identityRows(u store.User, ids []store.Identity, self bool) []identityRow {
	var out []identityRow
	for _, id := range ids {
		row := identityRow{Issuer: id.Issuer, Label: h.label(id), Email: id.Email, LastLogin: id.LastLoginAt, Unlink: true}
		if self && !u.HasPassword() && len(ids) == 1 {
			row.Unlink, row.Why = false, "your only way to log in"
		}
		out = append(out, row)
	}
	return out
}

// freshSession reports whether a session started within the first
// password window.
func (h *Handler) freshSession(ctx context.Context, id *access.Identity) bool {
	if id.Via != access.ViaSession {
		return false
	}
	at, err := h.store.SessionCreatedAt(ctx, id.SessionID)
	return err == nil && time.Since(at) < access.FirstPasswordWindow
}

// usersView is the users page.
type usersView struct {
	Users      []store.User
	Tokens     []store.Token
	Admins     int
	Identities map[int64][]identityRow
	Providers  bool   // provider sign-in is configured: a password is optional then
	Dialog     string // "add", "password" or "email"
	Form       string // the name typed, or whose password or email is set
	FormRole   string
	FormEmail  string
	Err        string
	Saved      string
}

// lastAdmin reports whether u is the only admin: its role and removal
// controls are left out (the server refuses anyway).
func (v usersView) lastAdmin(u store.User) bool { return u.Role == store.RoleAdmin && v.Admins == 1 }

// bareNav is the masthead of the login page: nothing about the server.
func (h *Handler) bareNav(r *http.Request) navView {
	return navView{Prefix: h.prefix, Asset: h.assetURL, Build: "", Bare: true, Current: -1, Theme: theme(r)}
}

func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	on, err := h.uiLoginOn(r.Context())
	if err != nil {
		h.fail(w, "login", err)
		return
	}
	if id := access.From(r.Context()); on && id != nil && id.Via == access.ViaSession {
		http.Redirect(w, r, h.nextPage(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	h.render(w, r, http.StatusOK, loginPage(h.bareNav(r), h.loginView(r, r.URL.Query().Get("next"), "", "")))
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	on, err := h.uiLoginOn(ctx)
	if err != nil {
		h.fail(w, "login", err)
		return
	}
	if !on {
		http.Redirect(w, r, h.prefix+"/", http.StatusSeeOther)
		return
	}
	addr := access.ClientAddr(r, h.proxies)
	name, next := r.PostFormValue("name"), r.PostFormValue("next")
	again := func(status int, msg string) {
		h.render(w, r, status, loginPage(h.bareNav(r), h.loginView(r, next, name, msg)))
	}
	if ok, wait := h.limiter.Allow(addr); !ok {
		mins := int((wait + time.Minute - 1) / time.Minute)
		h.log.Warn("login refused: too many failures", "addr", addr)
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		again(http.StatusTooManyRequests, fmt.Sprintf("Too many failed logins from your address; try again in %d minutes.", mins))
		return
	}
	u, err := h.auth.Login(ctx, name, r.PostFormValue("password"))
	switch {
	case errors.Is(err, access.ErrWrongLogin):
		h.limiter.Fail(addr)
		attrs := []any{"addr", addr}
		if n, err := access.CheckName(name); err == nil {
			if _, err := h.store.UserByName(ctx, n); err == nil {
				attrs = append(attrs, "user", n) // only names that exist reach the log
			}
		}
		h.log.Warn("login failed", attrs...)
		time.Sleep(loginAttempt)
		again(http.StatusUnauthorized, "Wrong name or password.")
		return
	case errors.Is(err, access.ErrBusy):
		h.limiter.Release(addr)
		h.busy(w, false)
		return
	case err != nil:
		h.limiter.Release(addr)
		h.fail(w, "login", err)
		return
	}
	h.limiter.Success(addr)
	if err := h.startSession(w, r, u); err != nil {
		h.fail(w, "login", err)
		return
	}
	h.log.Info("user logged in", "user", u.Name, "addr", addr)
	http.Redirect(w, r, h.nextPage(next), http.StatusSeeOther)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if id := access.From(r.Context()); id != nil && id.Via == access.ViaSession {
		if err := h.store.EndSession(r.Context(), id.SessionID); err != nil {
			h.fail(w, "logout", err)
			return
		}
		h.log.Info("user logged out", "user", id.Name)
	}
	http.SetCookie(w, access.Cookie("", h.prefix, h.secure(r)))
	http.Redirect(w, r, h.prefix+"/login", http.StatusSeeOther)
}

// account renders the account page; the login is on (the gate saw to it)
// unless there are no users, when there is no account to show.
func (h *Handler) account(w http.ResponseWriter, r *http.Request, status int, v accountView) {
	id := access.From(r.Context())
	if id == nil {
		http.Redirect(w, r, h.prefix+"/users", http.StatusSeeOther)
		return
	}
	ts, err := h.store.Tokens(r.Context(), id.UserID)
	if err != nil {
		h.fail(w, "account", err)
		return
	}
	v.Tokens = ts
	u, err := h.store.UserByID(r.Context(), id.UserID)
	if err != nil {
		h.fail(w, "account", err)
		return
	}
	ids, err := h.store.Identities(r.Context(), u.ID)
	if err != nil {
		h.fail(w, "account", err)
		return
	}
	v.Email, v.HasPassword, v.Identities = u.Email, u.HasPassword(), h.identityRows(u, ids, true)
	v.Fresh = h.freshSession(r.Context(), id)
	nav, err := h.nav(r, -1, false)
	if err != nil {
		h.fail(w, "account", err)
		return
	}
	h.render(w, r, status, accountPage(nav, v))
}

func (h *Handler) accountPage(w http.ResponseWriter, r *http.Request) {
	v := accountView{Dialog: r.URL.Query().Get("dialog")}
	switch r.URL.Query().Get("saved") {
	case "password":
		v.Saved = "Password changed. Your other logins have ended."
	case "revoked":
		v.Saved = "Token revoked."
	}
	h.account(w, r, http.StatusOK, v)
}

func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	id := access.From(r.Context())
	if id == nil {
		http.Redirect(w, r, h.prefix+"/users", http.StatusSeeOther)
		return
	}
	bad := func(msg string) {
		h.account(w, r, http.StatusUnprocessableEntity, accountView{Dialog: "password", Err: msg})
	}
	nw := r.PostFormValue("new")
	if nw != r.PostFormValue("again") {
		bad("the two new passwords differ.")
		return
	}
	if err := access.CheckPassword(nw); err != nil {
		bad(err.Error() + ".")
		return
	}
	u, err := h.store.UserByID(r.Context(), id.UserID)
	if err != nil {
		h.fail(w, "change password", err)
		return
	}
	if !u.HasPassword() {
		// A first password: no current one to give, so a fresh sign-in.
		if !h.freshSession(r.Context(), id) {
			bad("sign in again to set a password: a first password needs a sign-in from the last 10 minutes.")
			return
		}
	} else if _, err := h.auth.Login(r.Context(), id.Name, r.PostFormValue("current")); err != nil {
		switch {
		case errors.Is(err, access.ErrWrongLogin):
			bad("the current password is wrong.")
		case errors.Is(err, access.ErrBusy):
			h.busy(w, false)
		default:
			h.fail(w, "change password", err)
		}
		return
	}
	if _, err := h.store.SetPassword(r.Context(), id.Name, h.auth.HashPassword(nw), id.SessionID); err != nil {
		h.fail(w, "change password", err)
		return
	}
	h.log.Info("password changed", "user", id.Name)
	http.Redirect(w, r, h.prefix+"/account?saved=password", http.StatusSeeOther)
}

func (h *Handler) createToken(w http.ResponseWriter, r *http.Request) {
	id := access.From(r.Context())
	if id == nil {
		http.Redirect(w, r, h.prefix+"/users", http.StatusSeeOther)
		return
	}
	raw := r.PostFormValue("name")
	name, err := access.CheckTokenName(raw)
	if err != nil {
		h.account(w, r, http.StatusUnprocessableEntity, accountView{Dialog: "token", Form: raw, Err: err.Error() + "."})
		return
	}
	value, t, err := h.auth.MintFor(r.Context(), id.UserID, name)
	if errors.Is(err, store.ErrExists) {
		h.account(w, r, http.StatusUnprocessableEntity, accountView{Dialog: "token", Form: raw, Err: "you already have a token with this name."})
		return
	}
	if err != nil {
		h.fail(w, "create token", err)
		return
	}
	h.log.Info("token created", "user", id.Name, "token", name, "token_id", t.PublicID)
	// The value is on this one page only: never stored, never in a URL.
	h.account(w, r, http.StatusOK, accountView{NewToken: value, NewName: name})
}

func (h *Handler) revokeOwnToken(w http.ResponseWriter, r *http.Request) {
	id := access.From(r.Context())
	if id == nil {
		http.NotFound(w, r)
		return
	}
	t, err := h.store.RevokeToken(r.Context(), r.PathValue("id"), id.UserID)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, "revoke token", err)
		return
	}
	h.log.Info("token revoked", "user", id.Name, "owner", t.Owner, "token", t.Name, "token_id", t.PublicID)
	h.auth.Changed()
	http.Redirect(w, r, h.prefix+"/account?saved=revoked", http.StatusSeeOther)
}

// users renders the users page.
func (h *Handler) users(w http.ResponseWriter, r *http.Request, status int, v usersView) {
	ctx := r.Context()
	us, err := h.store.Users(ctx)
	if err != nil {
		h.fail(w, "users", err)
		return
	}
	ts, err := h.store.Tokens(ctx, 0)
	if err != nil {
		h.fail(w, "users", err)
		return
	}
	all, err := h.store.AllIdentities(ctx)
	if err != nil {
		h.fail(w, "users", err)
		return
	}
	v.Users, v.Tokens, v.Providers, v.Identities = us, ts, len(h.oidc) > 0, map[int64][]identityRow{}
	me := access.From(ctx)
	for _, u := range us {
		if u.Role == store.RoleAdmin {
			v.Admins++
		}
		v.Identities[u.ID] = h.identityRows(u, all[u.ID], me != nil && me.UserID == u.ID)
	}
	nav, err := h.nav(r, -1, false)
	if err != nil {
		h.fail(w, "users", err)
		return
	}
	h.render(w, r, status, usersPage(nav, v))
}

func (h *Handler) usersPage(w http.ResponseWriter, r *http.Request) {
	if h.auth == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	v := usersView{Dialog: q.Get("dialog"), Form: q.Get("name")}
	if v.Dialog == "password" || v.Dialog == "email" {
		u, err := h.store.UserByName(r.Context(), v.Form)
		if err != nil {
			v.Dialog = ""
		}
		v.FormEmail = u.Email
	}
	if s := q.Get("saved"); s != "" {
		v.Saved = savedUsers[s]
	}
	h.users(w, r, http.StatusOK, v)
}

var savedUsers = map[string]string{
	"added":    "User added.",
	"role":     "Role changed; their logins have ended.",
	"password": "Password set; their logins have ended.",
	"removed":  "User removed with their tokens.",
	"revoked":  "Token revoked.",
	"email":    "Email saved.",
	"unlinked": "Identity unlinked; their logins have ended.",
}

// who is the log attribute of the user making a change, if any.
func who(r *http.Request) []any { return access.Who(r.Context()) }

func (h *Handler) addUser(w http.ResponseWriter, r *http.Request) {
	if h.auth == nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	first, err := h.store.HasUsers(ctx)
	if err != nil {
		h.fail(w, "add user", err)
		return
	}
	first = !first
	name, role := r.PostFormValue("name"), r.PostFormValue("role")
	email, pw := strings.TrimSpace(r.PostFormValue("email")), r.PostFormValue("password")
	again := func(status int, msg string) {
		h.users(w, r, status, usersView{Dialog: "add", Form: name, FormRole: role, FormEmail: email, Err: msg})
	}
	if pw == "" && len(h.oidc) == 0 {
		again(http.StatusUnprocessableEntity, "a password is needed.")
		return
	}
	u, err := api.AddUser(ctx, h.store, h.auth, name, role, pw, email)
	if e, ok := access.IsInvalid(err); ok {
		again(http.StatusUnprocessableEntity, e.Message+".")
		return
	}
	if errors.Is(err, store.ErrExists) {
		again(http.StatusUnprocessableEntity, "a user with this name exists.")
		return
	}
	if errors.Is(err, store.ErrEmailTaken) {
		again(http.StatusConflict, "another user has this email.")
		return
	}
	if err != nil {
		h.fail(w, "add user", err)
		return
	}
	h.log.Info("user added", append([]any{"name", u.Name, "role", u.Role}, who(r)...)...)
	if first {
		// Login is on from now: whoever added the first user logs in.
		http.Redirect(w, r, h.prefix+"/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, h.prefix+"/users?saved=added", http.StatusSeeOther)
}

// setEmail sets or clears a user's email (an admin's).
func (h *Handler) setEmail(w http.ResponseWriter, r *http.Request) {
	name := pathUser(r)
	email := strings.TrimSpace(r.PostFormValue("email"))
	again := func(status int, msg string) {
		h.users(w, r, status, usersView{Dialog: "email", Form: name, FormEmail: email, Err: msg})
	}
	if email != "" {
		e, err := access.CheckEmail(email)
		if err != nil {
			again(http.StatusUnprocessableEntity, "an email is an address such as ann@example.com.")
			return
		}
		email = e
	}
	u, err := h.store.SetEmail(r.Context(), name, email)
	if errors.Is(err, store.ErrEmailTaken) {
		again(http.StatusConflict, "another user has this email.")
		return
	}
	if err != nil {
		h.userError(w, r, "set email", err)
		return
	}
	h.log.Info("user email set", append([]any{"name", u.Name, "email", email}, who(r)...)...)
	http.Redirect(w, r, h.prefix+"/users?saved=email", http.StatusSeeOther)
}

// unlinkUser removes a user's identity at an issuer (an admin's): that
// user's logins end; an admin's own last way in is refused.
func (h *Handler) unlinkUser(w http.ResponseWriter, r *http.Request) {
	name := pathUser(r)
	me := access.From(r.Context())
	gone, err := h.store.Unlink(r.Context(), name, r.PostFormValue("issuer"), me != nil && me.Name == name)
	if errors.Is(err, store.ErrLastWayIn) {
		h.users(w, r, http.StatusConflict, usersView{Err: "Not unlinked: you would have no way to log in. Set a password first."})
		return
	}
	if err != nil {
		h.userError(w, r, "unlink", err)
		return
	}
	h.log.Info("oidc identity unlinked", append([]any{"name", name, "provider", gone.Provider, "issuer", gone.Issuer}, who(r)...)...)
	if me != nil && me.Name == name {
		http.SetCookie(w, access.Cookie("", h.prefix, h.secure(r)))
		http.Redirect(w, r, h.prefix+"/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, h.prefix+"/users?saved=unlinked", http.StatusSeeOther)
}

// unlinkOwn removes one's own identity at an issuer, unless it is one's
// only way in; one's logins end, this one too.
func (h *Handler) unlinkOwn(w http.ResponseWriter, r *http.Request) {
	id := access.From(r.Context())
	if id == nil || id.Via != access.ViaSession {
		http.Redirect(w, r, h.prefix+"/users", http.StatusSeeOther)
		return
	}
	gone, err := h.store.Unlink(r.Context(), id.Name, r.PostFormValue("issuer"), true)
	switch {
	case errors.Is(err, store.ErrLastWayIn):
		h.account(w, r, http.StatusConflict, accountView{Err: "Not unlinked: it is your only way to log in. Set a password first."})
		return
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		h.fail(w, "unlink", err)
		return
	}
	h.log.Info("oidc identity unlinked", "name", id.Name, "provider", gone.Provider, "issuer", gone.Issuer, "user", id.Name)
	http.SetCookie(w, access.Cookie("", h.prefix, h.secure(r)))
	http.Redirect(w, r, h.prefix+"/login", http.StatusSeeOther)
}

// userError answers a failed change of a user on the users page.
func (h *Handler) userError(w http.ResponseWriter, r *http.Request, what string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)
	case errors.Is(err, store.ErrLastAdmin):
		h.users(w, r, http.StatusConflict, usersView{Err: "That is the last admin: make another user an admin first."})
	default:
		h.fail(w, what, err)
	}
}

// pathUser reads the {name} of a users route.
func pathUser(r *http.Request) string {
	n, err := access.CheckName(r.PathValue("name"))
	if err != nil {
		return ""
	}
	return n
}

func (h *Handler) setRole(w http.ResponseWriter, r *http.Request) {
	if h.auth == nil {
		http.NotFound(w, r)
		return
	}
	role := r.PostFormValue("role")
	if role != store.RoleUser && role != store.RoleAdmin {
		http.Error(w, "A role is user or admin.", http.StatusBadRequest)
		return
	}
	if id := access.From(r.Context()); id != nil && role == store.RoleUser && id.Name == pathUser(r) {
		h.users(w, r, http.StatusConflict, usersView{Err: "You cannot make yourself a user: another admin can."})
		return
	}
	u, err := h.store.SetRole(r.Context(), pathUser(r), role)
	if err != nil {
		h.userError(w, r, "set role", err)
		return
	}
	h.log.Info("user role changed", append([]any{"name", u.Name, "role", u.Role}, who(r)...)...)
	h.auth.Changed()
	h.afterOwnChange(w, r, u.Name, "role")
}

// afterOwnChange redirects after a change that ended the user's sessions:
// an admin who changed themselves logs in again.
func (h *Handler) afterOwnChange(w http.ResponseWriter, r *http.Request, name, saved string) {
	if id := access.From(r.Context()); id != nil && id.Name == name {
		http.SetCookie(w, access.Cookie("", h.prefix, h.secure(r)))
		http.Redirect(w, r, h.prefix+"/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, h.prefix+"/users?saved="+saved, http.StatusSeeOther)
}

func (h *Handler) resetPassword(w http.ResponseWriter, r *http.Request) {
	if h.auth == nil {
		http.NotFound(w, r)
		return
	}
	name, pw := pathUser(r), r.PostFormValue("password")
	if err := access.CheckPassword(pw); err != nil {
		h.users(w, r, http.StatusUnprocessableEntity, usersView{Dialog: "password", Form: name, Err: err.Error() + "."})
		return
	}
	u, err := h.store.SetPassword(r.Context(), name, h.auth.HashPassword(pw), 0)
	if err != nil {
		h.userError(w, r, "set password", err)
		return
	}
	h.log.Info("user password reset", append([]any{"name", u.Name}, who(r)...)...)
	h.afterOwnChange(w, r, u.Name, "password")
}

func (h *Handler) removeUser(w http.ResponseWriter, r *http.Request) {
	if h.auth == nil {
		http.NotFound(w, r)
		return
	}
	if id := access.From(r.Context()); id != nil && id.Name == pathUser(r) {
		h.users(w, r, http.StatusConflict, usersView{Err: "You cannot remove yourself: another admin can."})
		return
	}
	u, err := h.store.RemoveUser(r.Context(), pathUser(r))
	if err != nil {
		h.userError(w, r, "remove user", err)
		return
	}
	h.log.Info("user removed", append([]any{"name", u.Name}, who(r)...)...)
	h.auth.Changed()
	h.afterOwnChange(w, r, u.Name, "removed")
}

func (h *Handler) revokeAnyToken(w http.ResponseWriter, r *http.Request) {
	if h.auth == nil {
		http.NotFound(w, r)
		return
	}
	t, err := h.store.RevokeToken(r.Context(), r.PathValue("id"), 0)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, "revoke token", err)
		return
	}
	h.log.Info("token revoked", append([]any{"owner", t.Owner, "token", t.Name, "token_id", t.PublicID}, who(r)...)...)
	h.auth.Changed()
	http.Redirect(w, r, h.prefix+"/users?saved=revoked", http.StatusSeeOther)
}

// signInWays says how a user logs in: "password", the providers linked, or
// both.
func signInWays(u store.User, ids []identityRow) string {
	var ways []string
	if u.HasPassword() {
		ways = append(ways, "password")
	}
	for _, id := range ids {
		ways = append(ways, id.Label)
	}
	if len(ways) == 0 {
		return "none yet"
	}
	return strings.Join(ways, ", ")
}
