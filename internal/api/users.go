package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/petroladkin/hookwitness/internal/access"
	"github.com/petroladkin/hookwitness/internal/store"
)

// need is what a route asks of the caller once login is on.
type need int

const (
	needAny       need = iota // anyone authenticated
	needSession               // a logged-in person, not a token: their own password and tokens
	needAdmin                 // an admin's session: users and everyone's tokens
	needAdminRole             // an admin, by login or token: sending replays
)

// loginOn reports whether the API wants credentials: a user exists, or a
// machine token is configured.
func (h *Handler) loginOn(ctx context.Context) (bool, error) {
	if h.o.Access == nil {
		return false, nil
	}
	return h.o.Access.APILoginOn(ctx)
}

// Unauthorized answers a request without credentials (or with an ended
// session) once login is on.
func Unauthorized(w http.ResponseWriter, code, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="hookwitness"`)
	WriteError(w, http.StatusUnauthorized, code, msg, "")
}

const loginFirst = "log in, or send Authorization: Bearer <token>"

// guard wraps a route with its rule.
func (h *Handler) guard(n need, fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		on, err := h.loginOn(r.Context())
		if err != nil {
			h.fail(w, "access", err)
			return
		}
		id := access.From(r.Context())
		switch {
		case (n == needSession || n == needAdmin) && h.o.Access == nil:
			WriteError(w, http.StatusNotFound, "not_found", "users and tokens are not available on this server", "")
			return
		case id == nil && on:
			Unauthorized(w, "unauthorized", loginFirst)
			return
		case id == nil && n == needSession:
			WriteError(w, http.StatusConflict, "login_off", "login is off: there are no users yet, so there is no password or token of yours; add the first user (an admin) to turn login on", "")
			return
		case id != nil && n == needAdminRole && !id.Admin():
			WriteError(w, http.StatusForbidden, "forbidden", "only an admin can replay requests: a replay reaches the server's own network", "")
			return
		case id != nil && n != needAny && n != needAdminRole && id.Via != access.ViaSession:
			WriteError(w, http.StatusForbidden, "session_required", "managing tokens and users needs a login, not a token: use the web UI or hookwitness user", "")
			return
		case id != nil && n == needAdmin && !id.Admin():
			WriteError(w, http.StatusForbidden, "forbidden", "only an admin can manage users and their tokens", "")
			return
		}
		fn(w, r)
	}
}

// userJSON is a user as the API shows it: never the password.
type userJSON struct {
	Name    string `json:"name"`
	Role    string `json:"role"`
	Created stamp  `json:"created_at"`
	Tokens  int    `json:"tokens"`
}

// tokenJSON is a token without its value; Token is set only when it was just
// made.
type tokenJSON struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Owner   string `json:"owner"`
	Shown   string `json:"shown"` // hkw_<id>_…
	Created stamp  `json:"created_at"`
	Used    *stamp `json:"last_used_at"`
	Token   string `json:"token,omitempty"`
}

func tokenView(t store.Token) tokenJSON {
	v := tokenJSON{ID: t.PublicID, Name: t.Name, Owner: t.Owner, Shown: access.Shown(t.PublicID), Created: stamp(t.CreatedAt)}
	if !t.LastUsedAt.IsZero() {
		u := stamp(t.LastUsedAt)
		v.Used = &u
	}
	return v
}

// logAttrs adds who made a change to a log line's attributes.
func logAttrs(r *http.Request, attrs ...any) []any {
	return append(attrs, access.Who(r.Context())...)
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	on, err := h.loginOn(r.Context())
	if err != nil {
		h.fail(w, "me", err)
		return
	}
	id := access.From(r.Context())
	if !on || id == nil {
		writeJSON(w, http.StatusOK, map[string]any{"login": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"login": true, "name": id.Name, "role": id.Role, "via": id.Via.String()})
}

func writeInvalid(w http.ResponseWriter, err error) bool {
	if e, ok := access.IsInvalid(err); ok {
		WriteError(w, http.StatusUnprocessableEntity, "invalid", e.Message, e.Field)
		return true
	}
	return false
}

// busy answers when too many password or token checks run at once.
func busy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	WriteError(w, http.StatusServiceUnavailable, "busy", "too many password or token checks at once; try again in a second", "")
}

func (h *Handler) changeOwnPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !decode(w, r, &in, false) {
		return
	}
	id := access.From(r.Context())
	if err := access.CheckPassword(in.New); err != nil {
		e, _ := access.IsInvalid(err)
		WriteError(w, http.StatusUnprocessableEntity, "invalid", e.Message, "new")
		return
	}
	if _, err := h.o.Access.Login(r.Context(), id.Name, in.Current); err != nil {
		switch {
		case errors.Is(err, access.ErrBusy):
			busy(w)
		case errors.Is(err, access.ErrWrongLogin):
			WriteError(w, http.StatusUnprocessableEntity, "invalid", "the current password is wrong", "current")
		default:
			h.fail(w, "change password", err)
		}
		return
	}
	if _, err := h.store.SetPassword(r.Context(), id.Name, h.o.Access.HashPassword(in.New), id.SessionID); err != nil {
		h.fail(w, "change password", err)
		return
	}
	h.log.Info("password changed", logAttrs(r)...)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listTokens(owner func(*http.Request) int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ts, err := h.store.Tokens(r.Context(), owner(r))
		if err != nil {
			h.fail(w, "list tokens", err)
			return
		}
		out := []tokenJSON{}
		for _, t := range ts {
			out = append(out, tokenView(t))
		}
		writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
	}
}

func ownID(r *http.Request) int64  { return access.From(r.Context()).UserID }
func everyone(*http.Request) int64 { return 0 }

func (h *Handler) createToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in, false) {
		return
	}
	name, err := access.CheckTokenName(in.Name)
	if writeInvalid(w, err) {
		return
	}
	id := access.From(r.Context())
	value, t, err := h.o.Access.MintFor(r.Context(), id.UserID, name)
	if errors.Is(err, store.ErrExists) {
		WriteError(w, http.StatusConflict, "exists", fmt.Sprintf("you already have a token named %q", name), "name")
		return
	}
	if err != nil {
		h.fail(w, "create token", err)
		return
	}
	t.Owner = id.Name
	h.log.Info("token created", logAttrs(r, "token", name, "token_id", t.PublicID)...)
	v := tokenView(t)
	v.Token = value
	writeJSON(w, http.StatusCreated, v)
}

func (h *Handler) revokeToken(owner func(*http.Request) int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, err := h.store.RevokeToken(r.Context(), r.PathValue("id"), owner(r))
		if errors.Is(err, store.ErrNotFound) {
			WriteError(w, http.StatusNotFound, "not_found", "no token "+r.PathValue("id"), "")
			return
		}
		if err != nil {
			h.fail(w, "revoke token", err)
			return
		}
		h.log.Info("token revoked", logAttrs(r, "owner", t.Owner, "token", t.Name, "token_id", t.PublicID)...)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *Handler) listUsers(w http.ResponseWriter, r *http.Request) {
	us, err := h.store.Users(r.Context())
	if err != nil {
		h.fail(w, "list users", err)
		return
	}
	out := []userJSON{}
	for _, u := range us {
		out = append(out, userJSON{Name: u.Name, Role: u.Role, Created: stamp(u.CreatedAt), Tokens: u.Tokens})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

func checkRole(role string) error {
	if role != store.RoleUser && role != store.RoleAdmin {
		return &access.ErrInvalid{Field: "role", Message: `a role is "user" or "admin"`}
	}
	return nil
}

// AddUser creates a user for the UI and the API: the first one must be an
// admin (or nobody could manage users).
func AddUser(ctx context.Context, st *store.Store, a *access.Authenticator, name, role, password string) (store.User, error) {
	n, err := access.CheckName(name)
	if err != nil {
		return store.User{}, err
	}
	if err := checkRole(role); err != nil {
		return store.User{}, err
	}
	if err := access.CheckPassword(password); err != nil {
		return store.User{}, err
	}
	if role != store.RoleAdmin {
		if on, err := st.HasUsers(ctx); err != nil {
			return store.User{}, err
		} else if !on {
			return store.User{}, &access.ErrInvalid{Field: "role", Message: "the first user must be an admin: otherwise nobody could manage users"}
		}
	}
	return st.AddUser(ctx, n, role, a.HashPassword(password))
}

func (h *Handler) addUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name     string `json:"name"`
		Role     string `json:"role"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in, false) {
		return
	}
	if in.Role == "" {
		in.Role = store.RoleUser
	}
	u, err := AddUser(r.Context(), h.store, h.o.Access, in.Name, in.Role, in.Password)
	if writeInvalid(w, err) {
		return
	}
	if errors.Is(err, store.ErrExists) {
		WriteError(w, http.StatusConflict, "exists", "a user with this name exists", "name")
		return
	}
	if err != nil {
		h.fail(w, "add user", err)
		return
	}
	h.log.Info("user added", logAttrs(r, "name", u.Name, "role", u.Role)...)
	writeJSON(w, http.StatusCreated, userJSON{Name: u.Name, Role: u.Role, Created: stamp(u.CreatedAt)})
}

func (h *Handler) userName(w http.ResponseWriter, r *http.Request) (string, bool) {
	n, err := access.CheckName(r.PathValue("name"))
	if err != nil {
		WriteError(w, http.StatusNotFound, "not_found", "no user "+r.PathValue("name"), "")
		return "", false
	}
	return n, true
}

func (h *Handler) userError(w http.ResponseWriter, what, name string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		WriteError(w, http.StatusNotFound, "not_found", "no user "+name, "")
	case errors.Is(err, store.ErrLastAdmin):
		WriteError(w, http.StatusConflict, "last_admin", name+" is the last admin: make another user an admin first", "")
	default:
		h.fail(w, what, err)
	}
}

func (h *Handler) patchUser(w http.ResponseWriter, r *http.Request) {
	name, ok := h.userName(w, r)
	if !ok {
		return
	}
	var in struct {
		Role     *string `json:"role"`
		Password *string `json:"password"`
	}
	if !decode(w, r, &in, false) {
		return
	}
	if in.Role != nil && writeInvalid(w, checkRole(*in.Role)) {
		return
	}
	if in.Password != nil && writeInvalid(w, access.CheckPassword(*in.Password)) {
		return
	}
	if id := access.From(r.Context()); id != nil && in.Role != nil && *in.Role == store.RoleUser && id.Name == name {
		WriteError(w, http.StatusConflict, "self_demotion", "you cannot make yourself a user: another admin can", "")
		return
	}
	if _, err := h.store.UserByName(r.Context(), name); err != nil {
		h.userError(w, "change user", name, err)
		return
	}
	if in.Role != nil {
		u, err := h.store.SetRole(r.Context(), name, *in.Role)
		if err != nil {
			h.userError(w, "change user", name, err)
			return
		}
		h.log.Info("user role changed", logAttrs(r, "name", u.Name, "role", u.Role)...)
	}
	if in.Password != nil {
		// A reset by an admin ends every session of that user.
		if _, err := h.store.SetPassword(r.Context(), name, h.o.Access.HashPassword(*in.Password), 0); err != nil {
			h.userError(w, "change user", name, err)
			return
		}
		h.log.Info("user password reset", logAttrs(r, "name", name)...)
	}
	u, err := h.store.UserByName(r.Context(), name)
	if err != nil {
		h.userError(w, "change user", name, err)
		return
	}
	ts, _ := h.store.Tokens(r.Context(), u.ID)
	writeJSON(w, http.StatusOK, userJSON{Name: u.Name, Role: u.Role, Created: stamp(u.CreatedAt), Tokens: len(ts)})
}

func (h *Handler) removeUser(w http.ResponseWriter, r *http.Request) {
	name, ok := h.userName(w, r)
	if !ok {
		return
	}
	if id := access.From(r.Context()); id != nil && id.Name == name {
		WriteError(w, http.StatusConflict, "self_removal", "you cannot remove yourself: another admin can", "")
		return
	}
	u, err := h.store.RemoveUser(r.Context(), name)
	if err != nil {
		h.userError(w, "remove user", name, err)
		return
	}
	h.log.Info("user removed", logAttrs(r, "name", u.Name)...)
	w.WriteHeader(http.StatusNoContent)
}

// Patterns lists the API's routes as "METHOD /path" under its base (tests
// walk every route).
func (h *Handler) Patterns() []string {
	var out []string
	for _, rt := range h.routes() {
		out = append(out, rt.method+" "+h.base+rt.path)
	}
	return append(out, "GET "+h.base+"/openapi.json")
}
