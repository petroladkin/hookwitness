package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/petroladkin/hookwitness/internal/access"
	"github.com/petroladkin/hookwitness/internal/oidc"
	"github.com/petroladkin/hookwitness/internal/store"
)

// Signing in through OpenID Connect providers: POST <prefix>/oidc/{name}/start
// sends the browser to the provider; GET <prefix>/oidc/{name}/callback
// brings it back, verified, into an ordinary session. Every refusal shows
// the login page with one message, counts as a failed login, and logs one
// line with its reason; nothing a provider hands out is logged.

// provider is the configured provider of that name, or nil.
func (h *Handler) provider(name string) *oidc.Provider {
	for _, p := range h.oidc {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// refusal is one row of the refusals table: a reason for the log and a
// message for the page.
type refusal struct {
	reason string
	status int
	msg    string
	attrs  []any
}

func (h *Handler) refuseSignIn(w http.ResponseWriter, r *http.Request, p *oidc.Provider, addr string, f refusal) {
	h.limiter.Fail(addr)
	h.log.Warn("oidc sign-in refused", append([]any{"provider", p.Name, "reason", f.reason, "addr", addr}, f.attrs...)...)
	h.render(w, r, f.status, loginPage(h.bareNav(r), h.loginView(r, r.URL.Query().Get("next"), "", f.msg)))
}

func loginOffRefusal() refusal {
	return refusal{reason: "login_off", status: http.StatusForbidden, msg: "Signing in with a provider starts once the first admin exists."}
}

func unreachable(p *oidc.Provider, err error) refusal {
	return refusal{reason: "unreachable", status: http.StatusBadGateway,
		msg: p.Label + " cannot be reached right now; try again, or log in with a password.", attrs: []any{"err", err.Error()}}
}

// oidcStart begins a sign-in: a flow kept here, its id in a cookie bound
// to this browser, and the browser sent to the provider. A POST, so that a
// page of another site cannot start one (cross-origin protection).
func (h *Handler) oidcStart(w http.ResponseWriter, r *http.Request) {
	p := h.provider(r.PathValue("name"))
	if p == nil {
		http.NotFound(w, r)
		return
	}
	next := h.nextPage(r.PostFormValue("next"))
	// The flow cookie must be set on the admin's own origin, where the
	// provider sends the browser back.
	if h.adminHost != "" && hostName(r.Host) != h.adminHost {
		http.Redirect(w, r, h.adminOrigin+h.prefix+"/login?moved=1&next="+url.QueryEscape(next), http.StatusSeeOther)
		return
	}
	addr := access.ClientAddr(r, h.proxies)
	on, err := h.uiLoginOn(r.Context())
	if err != nil {
		h.fail(w, "sign-in", err)
		return
	}
	if !on {
		h.refuseSignIn(w, r, p, addr, loginOffRefusal())
		return
	}
	id, f := h.flows.Start(p.Name, next)
	to, err := p.AuthURL(r.Context(), f)
	if err != nil {
		h.flows.Take(id, p.Name, f.State) // forget it
		h.refuseSignIn(w, r, p, addr, unreachable(p, err))
		return
	}
	http.SetCookie(w, access.FlowCookie(id, h.prefix, h.secure(r)))
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// oidcCallback finishes a sign-in: the flow (once), the provider's answer,
// the ID token, then who it is here (store.SignIn) and a session.
func (h *Handler) oidcCallback(w http.ResponseWriter, r *http.Request) {
	p := h.provider(r.PathValue("name"))
	if p == nil {
		http.NotFound(w, r)
		return
	}
	ctx, q := r.Context(), r.URL.Query()
	addr := access.ClientAddr(r, h.proxies)
	if ok, wait := h.limiter.Allow(addr); !ok {
		mins := int((wait + time.Minute - 1) / time.Minute)
		h.log.Warn("oidc sign-in refused", "provider", p.Name, "reason", "too_many", "addr", addr)
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		h.render(w, r, http.StatusTooManyRequests, loginPage(h.bareNav(r), h.loginView(r, "", "",
			fmt.Sprintf("Too many failed logins from your address; try again in %d minutes.", mins))))
		return
	}
	flowID := ""
	if c, err := r.Cookie(access.FlowCookieName); err == nil {
		flowID = c.Value
	}
	http.SetCookie(w, access.FlowCookie("", h.prefix, h.secure(r)))
	f, err := h.flows.Take(flowID, p.Name, q.Get("state"))
	if err != nil {
		h.refuseSignIn(w, r, p, addr, refusal{reason: "expired", status: http.StatusUnauthorized,
			msg: "The sign-in took too long or was started in another tab; try again."})
		return
	}
	if code := q.Get("error"); code != "" {
		h.refuseSignIn(w, r, p, addr, providerRefused(p, &oidc.ProviderError{Code: code, Description: q.Get("error_description")}))
		return
	}
	if on, err := h.uiLoginOn(ctx); err != nil {
		h.limiter.Release(addr)
		h.fail(w, "sign-in", err)
		return
	} else if !on {
		h.refuseSignIn(w, r, p, addr, loginOffRefusal())
		return
	}
	claims, err := p.Exchange(ctx, f, q.Get("code"), q.Get("iss"))
	var pe *oidc.ProviderError
	switch {
	case errors.As(err, &pe):
		h.refuseSignIn(w, r, p, addr, providerRefused(p, pe))
		return
	case errors.Is(err, oidc.ErrUnreachable):
		h.refuseSignIn(w, r, p, addr, unreachable(p, err))
		return
	case err != nil:
		h.refuseSignIn(w, r, p, addr, refusal{reason: "invalid_token", status: http.StatusUnauthorized,
			msg: p.Label + "'s answer could not be verified; the server log has the details.", attrs: []any{"err", err.Error()}})
		return
	}
	email, err := access.CheckEmail(claims.Email)
	if err != nil {
		email, claims.Verified = "", false
	}
	res, err := h.store.SignIn(ctx, store.SignInInput{Issuer: claims.Issuer, Subject: claims.Subject, Provider: p.Name, Email: email,
		Verified: claims.Verified, AllowCreate: p.AllowsDomain(email), Name: access.NameFromEmail(email)})
	shown := email
	if shown == "" {
		shown = "this address"
	}
	switch {
	case errors.Is(err, store.ErrLoginOff):
		h.refuseSignIn(w, r, p, addr, loginOffRefusal())
		return
	case errors.Is(err, store.ErrUnverified):
		h.refuseSignIn(w, r, p, addr, refusal{reason: "unverified_email", status: http.StatusForbidden,
			msg: p.Label + " did not confirm that " + shown + " is yours, so it cannot be matched to an account.", attrs: []any{"email", email}})
		return
	case errors.Is(err, store.ErrNoAccount):
		h.refuseSignIn(w, r, p, addr, refusal{reason: "no_account", status: http.StatusForbidden,
			msg: "There is no account here for " + email + ". Ask an admin to add you.", attrs: []any{"email", email}})
		return
	case errors.Is(err, store.ErrLinkedElsewhere):
		h.refuseSignIn(w, r, p, addr, refusal{reason: "linked_elsewhere", status: http.StatusForbidden,
			msg:   email + " belongs to an account already signed in with another " + p.Label + " identity. Ask an admin to unlink it.",
			attrs: []any{"email", email, "user", res.User.Name}})
		return
	case err != nil:
		h.limiter.Release(addr)
		h.fail(w, "sign-in", err)
		return
	}
	h.limiter.Success(addr)
	if res.Created {
		h.log.Info("user added", "name", res.User.Name, "role", res.User.Role, "via", "oidc", "provider", p.Name, "email", email)
	} else if res.Linked {
		h.log.Info("oidc identity linked", "user", res.User.Name, "provider", p.Name, "email", email)
	}
	if err := h.startSession(w, r, res.User); err != nil {
		h.fail(w, "sign-in", err)
		return
	}
	h.log.Info("user logged in", "user", res.User.Name, "via", "oidc", "provider", p.Name, "addr", addr)
	http.Redirect(w, r, f.Next, http.StatusSeeOther)
}

func providerRefused(p *oidc.Provider, pe *oidc.ProviderError) refusal {
	desc := pe.Description
	if len(desc) > 200 {
		desc = desc[:200]
	}
	return refusal{reason: "provider_error", status: http.StatusUnauthorized,
		msg: p.Label + " refused the sign-in (" + pe.Code + ").", attrs: []any{"error", pe.Code, "error_description", desc}}
}

// startSession starts a session for u, as a password login does.
func (h *Handler) startSession(w http.ResponseWriter, r *http.Request, u store.User) error {
	id, _, err := h.auth.StartSession(r.Context(), u)
	if err != nil {
		return err
	}
	http.SetCookie(w, access.Cookie(id, h.prefix, h.secure(r)))
	return nil
}

// DiscoverProviders fetches each provider's discovery document in the
// background (the server starts while a provider is down; a sign-in tries
// again later).
func (h *Handler) DiscoverProviders(ctx context.Context) {
	for _, p := range h.oidc {
		go func() {
			if err := p.Discover(ctx); err != nil && ctx.Err() == nil {
				h.log.Warn("oidc provider unreachable", "name", p.Name, "err", err.Error())
			}
		}()
	}
}
