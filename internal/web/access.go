package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/petroladkin/hookwitness/internal/access"
	"github.com/petroladkin/hookwitness/internal/api"
)

// The admin's gate. Once login is on, every admin request needs an
// identity: the login cookie, or a token (Authorization: Bearer) for the
// API, /mcp and downloads. Credentials are read before the host check: a
// valid token skips it (a DNS-rebinding page cannot set the header), a
// cookie does not.

// identify reads the request's credentials. It answers itself and returns
// false when they are wrong (a bad token) or cannot be checked.
func (h *Handler) identify(w http.ResponseWriter, r *http.Request, isAPI bool) (*access.Identity, bool) {
	if h.auth == nil {
		return nil, true
	}
	id, present, err := h.auth.Bearer(r.Context(), r.Header.Get("Authorization"))
	if present {
		switch {
		case err == nil:
			return id, true
		case errors.Is(err, access.ErrInvalidToken):
			// Never a fallback to the cookie: a script with a wrong token
			// should learn so at once.
			if isAPI {
				api.Unauthorized(w, "invalid_token", "the token is not valid")
			} else {
				w.Header().Set("WWW-Authenticate", `Bearer realm="hookwitness"`)
				http.Error(w, "The token is not valid.", http.StatusUnauthorized)
			}
		case errors.Is(err, access.ErrBusy):
			h.busy(w, isAPI)
		default:
			h.failGate(w, isAPI, err)
		}
		return nil, false
	}
	c, err := r.Cookie(access.CookieName)
	if err != nil {
		return nil, true
	}
	id, err = h.auth.Session(r.Context(), c.Value)
	switch {
	case err == nil:
		return id, true
	case errors.Is(err, access.ErrNoSession):
		// An ended or unknown session: the browser forgets the cookie.
		http.SetCookie(w, access.Cookie("", h.prefix, h.secure(r)))
		return nil, true
	}
	h.failGate(w, isAPI, err)
	return nil, false
}

func (h *Handler) busy(w http.ResponseWriter, isAPI bool) {
	w.Header().Set("Retry-After", "1")
	if isAPI {
		api.WriteError(w, http.StatusServiceUnavailable, "busy", "too many password or token checks at once; try again in a second", "")
		return
	}
	http.Error(w, "Too many logins at once; try again in a second.", http.StatusServiceUnavailable)
}

func (h *Handler) failGate(w http.ResponseWriter, isAPI bool, err error) {
	h.log.Error("web: access", "err", err)
	if isAPI {
		api.WriteError(w, http.StatusInternalServerError, "internal", "something went wrong on the server; the log has the details", "")
		return
	}
	http.Error(w, "Something went wrong on the server; the log has the details.", http.StatusInternalServerError)
}

// secure reports whether the login cookie should be Secure: the request
// came over TLS, or the public URL is https (a TLS proxy in front).
func (h *Handler) secure(r *http.Request) bool {
	return r.TLS != nil || strings.HasPrefix(h.catch.Public, "https://")
}

// uiLoginOn reports whether the UI requires a login: some user exists.
func (h *Handler) uiLoginOn(ctx context.Context) (bool, error) {
	if h.auth == nil {
		return false, nil
	}
	return h.auth.LoginOn(ctx)
}

// apiLoginOn reports whether the API and /mcp require credentials.
func (h *Handler) apiLoginOn(ctx context.Context) (bool, error) {
	if h.auth == nil {
		return false, nil
	}
	return h.auth.APILoginOn(ctx)
}

// openUI reports the UI routes anyone may reach: the login page and the
// static files.
func (h *Handler) openUI(r *http.Request) bool {
	p := r.URL.Path
	return p == h.prefix+"/login" || p == h.prefix+"/logout" || strings.HasPrefix(p, h.prefix+"/static/")
}

// gateUI applies the UI's rules; it answers itself and returns false when
// the request may not go on.
func (h *Handler) gateUI(w http.ResponseWriter, r *http.Request, id *access.Identity) bool {
	if h.openUI(r) {
		return true
	}
	on, err := h.uiLoginOn(r.Context())
	if err != nil {
		h.failGate(w, false, err)
		return false
	}
	if !on {
		return true
	}
	if id == nil {
		h.toLogin(w, r)
		return false
	}
	p := r.URL.Path
	// Users and everyone's tokens: /users, /users/<name>/…, /tokens/<id>/….
	if p == h.prefix+"/users" || strings.HasPrefix(p, h.prefix+"/users/") || strings.HasPrefix(p, h.prefix+"/tokens/") {
		if !id.Admin() || id.Via != access.ViaSession {
			h.forbidden(w, r, "Only an admin can manage users.")
			return false
		}
	}
	if (p == h.prefix+"/account" || strings.HasPrefix(p, h.prefix+"/account/")) && id.Via != access.ViaSession {
		h.forbidden(w, r, "Your password and tokens are managed after logging in, not with a token.")
		return false
	}
	return true
}

// toLogin sends someone without a login to the login page: a page load is
// redirected (coming back afterwards), an HTMX request is told to load it,
// anything else is refused.
func (h *Handler) toLogin(w http.ResponseWriter, r *http.Request) {
	login := h.prefix + "/login"
	switch {
	case r.Header.Get("HX-Request") == "true":
		w.Header().Set("HX-Redirect", login)
		http.Error(w, "Log in first.", http.StatusUnauthorized)
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		next := r.URL.Path
		if r.URL.RawQuery != "" {
			next += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, login+"?next="+url.QueryEscape(next), http.StatusSeeOther)
	default:
		http.Error(w, "Log in first.", http.StatusUnauthorized)
	}
}

func (h *Handler) forbidden(w http.ResponseWriter, r *http.Request, msg string) {
	nav, err := h.nav(r, -1, false)
	if err != nil {
		h.fail(w, "forbidden", err)
		return
	}
	h.render(w, r, http.StatusForbidden, messagePage(nav, "Not allowed", msg))
}

// nextPage is where a login goes on to: a page of the admin, else the
// endpoints page.
func (h *Handler) nextPage(next string) string {
	if strings.HasPrefix(next, h.prefix+"/") && !strings.HasPrefix(next, "//") && !strings.ContainsAny(next, "\\\r\n") &&
		!strings.HasPrefix(next, h.prefix+"/login") && !strings.HasPrefix(next, h.prefix+"/logout") {
		return next
	}
	return h.prefix + "/"
}

// gateAPI checks that the API and /mcp get an identity once login is on.
func (h *Handler) gateAPI(w http.ResponseWriter, r *http.Request, id *access.Identity, isMCP bool) bool {
	on, err := h.apiLoginOn(r.Context())
	if err != nil {
		h.failGate(w, true, err)
		return false
	}
	if !on {
		return true
	}
	if isMCP && (id == nil || id.Via == access.ViaSession) {
		api.Unauthorized(w, "unauthorized", "MCP needs a token: send Authorization: Bearer <token> (hookwitness mcp -token-file)")
		return false
	}
	if id == nil {
		api.Unauthorized(w, "unauthorized", "log in, or send Authorization: Bearer <token>")
		return false
	}
	return true
}

// mcpUser names the identity for the MCP SDK, which binds each MCP session
// to one user and refuses its id from another.
func mcpUser(id *access.Identity) string {
	if id.Via == access.ViaMachine {
		return access.MachineName
	}
	return "user:" + strconv.FormatInt(id.UserID, 10)
}

// withMCPIdentity hands the request's identity to the MCP SDK (as its
// TokenInfo), from where the tools carry it into their API calls.
func withMCPIdentity(next http.Handler) http.Handler {
	verify := func(ctx context.Context, _ string, r *http.Request) (*mcpauth.TokenInfo, error) {
		id := access.From(r.Context())
		if id == nil {
			return nil, mcpauth.ErrInvalidToken
		}
		return &mcpauth.TokenInfo{UserID: mcpUser(id), Extra: map[string]any{access.MCPExtra: id}}, nil
	}
	return mcpauth.RequireBearerToken(verify, &mcpauth.RequireBearerTokenOptions{AllowMissingExpiration: true})(next)
}

// sessionEnded reports whether the session of a live feed has ended since
// it connected (logout, expiry, a password or role change, removal).
func (h *Handler) sessionEnded(r *http.Request) bool {
	id := access.From(r.Context())
	if h.auth == nil || id == nil || id.Via != access.ViaSession {
		return false
	}
	c, err := r.Cookie(access.CookieName)
	if err != nil {
		return true
	}
	_, err = h.auth.Session(r.Context(), c.Value)
	return errors.Is(err, access.ErrNoSession)
}

// sessionCheck is how often an open live feed checks its session.
var sessionCheck = heartbeat

// loginAttempt waits this long after a failed login (tests set 0): it slows
// a guesser, not a person.
var loginAttempt = 300 * time.Millisecond
