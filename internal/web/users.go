package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
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
}

// usersView is the users page.
type usersView struct {
	Users    []store.User
	Tokens   []store.Token
	Admins   int
	Dialog   string // "add" or "password"
	Form     string // the name typed, or whose password is set
	FormRole string
	Err      string
	Saved    string
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
	h.render(w, r, http.StatusOK, loginPage(h.bareNav(r), loginView{On: on, Next: r.URL.Query().Get("next")}))
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
		h.render(w, r, status, loginPage(h.bareNav(r), loginView{On: true, Next: next, Name: name, Err: msg}))
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
	id, _, err := h.auth.StartSession(ctx, u)
	if err != nil {
		h.fail(w, "login", err)
		return
	}
	http.SetCookie(w, access.Cookie(id, h.prefix, h.secure(r)))
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
	if _, err := h.auth.Login(r.Context(), id.Name, r.PostFormValue("current")); err != nil {
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
	v.Users, v.Tokens = us, ts
	for _, u := range us {
		if u.Role == store.RoleAdmin {
			v.Admins++
		}
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
	if v.Dialog == "password" {
		if _, err := h.store.UserByName(r.Context(), v.Form); err != nil {
			v.Dialog = ""
		}
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
	u, err := api.AddUser(ctx, h.store, h.auth, name, role, r.PostFormValue("password"))
	if e, ok := access.IsInvalid(err); ok {
		h.users(w, r, http.StatusUnprocessableEntity, usersView{Dialog: "add", Form: name, FormRole: role, Err: e.Message + "."})
		return
	}
	if errors.Is(err, store.ErrExists) {
		h.users(w, r, http.StatusUnprocessableEntity, usersView{Dialog: "add", Form: name, FormRole: role, Err: "a user with this name exists."})
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
	http.Redirect(w, r, h.prefix+"/users?saved=revoked", http.StatusSeeOther)
}
