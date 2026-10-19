// Package web is the browser UI under the admin prefix: endpoints, the live
// register of caught requests, and each request as an exhibit.
package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"

	"github.com/petroladkin/hookwitness/internal/access"
	"github.com/petroladkin/hookwitness/internal/api"
	"github.com/petroladkin/hookwitness/internal/catch"
	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/live"
	"github.com/petroladkin/hookwitness/internal/mcp"
	"github.com/petroladkin/hookwitness/internal/replay"
	"github.com/petroladkin/hookwitness/internal/secret"
	"github.com/petroladkin/hookwitness/internal/store"
	"github.com/petroladkin/hookwitness/internal/version"
)

//go:embed static
var staticFS embed.FS

// pageSize is how many requests a register page lists before "Show older".
const pageSize = 100

// heartbeat keeps idle live feeds (and proxies in front of them) open.
const heartbeat = 25 * time.Second

// Handler serves the UI. Create it with New.
type Handler struct {
	// beforeSave runs between reading and saving a behaviour edit (tests
	// make a concurrent change there).
	beforeSave func()

	store   *store.Store
	live    *live.Broker
	log     *slog.Logger
	prefix  string
	reserve string       // what endpoint paths may not use (catch.CheckPath)
	catch   api.CatchURL // where senders reach the catch listener
	limit   int64        // the body limit, shown when a body was refused
	keep    int          // retention per register, so the page can drop pruned rows
	maxWait time.Duration
	hosts   map[string]bool
	keys    *secret.Key
	mux     http.Handler
	csrf    *http.CrossOriginProtection
	api     *api.Handler    // the JSON API under prefix/api/v1
	mcp     *mcp.Streamable // MCP over Streamable HTTP at prefix/mcp, its tools on the API
	// mcpAuthed is mcp for a request with an identity: the SDK gets it as
	// the session's user.
	mcpAuthed http.Handler
	auth      *access.Authenticator // nil: login is off for good (tests)
	limiter   *access.Limiter       // failed logins per client address
	proxies   access.Proxies        // reverse proxies whose X-Forwarded-For names the client
	assets    map[string]string     // asset path -> content hash for cache busting
	replay    *replay.Service       // sends caught requests to targets

	stopOnce sync.Once
	stop     chan struct{}
}

// Options configure the UI.
type Options struct {
	Prefix    string        // where the UI is mounted, e.g. /admin; "" = the root of its own listener
	Separate  bool          // the admin has its own listener: the catch side reserves nothing under Prefix
	PublicURL string        // base URL senders reach the catch listener at; "" = derived
	CatchAddr string        // the catch listener's address (a real port), for catch URLs when the admin is separate
	MaxBody   int64         // the catcher's body limit, shown when a body was refused
	Retention int           // requests kept per register
	MaxWait   time.Duration // longest delay an endpoint may configure
	Hosts     []string      // host names the UI answers to besides IP addresses and localhost
	Keys      *secret.Key   // seals and hashes endpoint secrets; verifies on re-check
	// Access checks logins and tokens; nil leaves the admin open (tests).
	Access  *access.Authenticator
	Proxies access.Proxies // reverse proxies whose X-Forwarded-For names the client
	// Replay sends caught requests to targets; nil: the default policy
	// without a -max-inflight budget (tests).
	Replay *replay.Service
	// MCPMaxSessions caps the MCP sessions open at once; 0: no limit.
	MCPMaxSessions int
}

// New returns the UI.
func New(st *store.Store, broker *live.Broker, log *slog.Logger, o Options) *Handler {
	h := &Handler{store: st, live: broker, log: log, prefix: o.Prefix, limit: o.MaxBody, keep: o.Retention, maxWait: o.MaxWait, keys: o.Keys,
		hosts: map[string]bool{}, stop: make(chan struct{}), auth: o.Access, limiter: access.NewLimiter(nil), proxies: o.Proxies}
	if h.maxWait == 0 {
		h.maxWait = 5 * time.Minute
	}
	for _, name := range o.Hosts {
		h.hosts[strings.ToLower(name)] = true
	}
	prefix := o.Prefix
	if !o.Separate {
		h.reserve = prefix
	}
	h.catch = api.CatchURL{Public: o.PublicURL, Addr: o.CatchAddr, Separate: o.Separate}
	h.assets = hashAssets()

	mux := http.NewServeMux()
	p := prefix
	if p != "" { // at the root of its own port the endpoints page is GET /
		mux.HandleFunc("GET "+p, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, p+"/", http.StatusMovedPermanently)
		})
	}
	mux.HandleFunc("GET "+p+"/{$}", h.endpointsPage)
	mux.HandleFunc("POST "+p+"/endpoints", h.createEndpoint)
	mux.HandleFunc("POST "+p+"/import", h.importSetup)
	mux.HandleFunc("GET "+p+"/setup", h.exportSetup)
	mux.HandleFunc("GET "+p+"/e/{reg}/setup", h.exportSetup)
	mux.HandleFunc("GET "+p+"/e/{reg}", h.registerPage)
	mux.HandleFunc("GET "+p+"/e/{reg}/r/{id}", h.registerPage)
	mux.HandleFunc("GET "+p+"/e/{reg}/rows", h.olderRows)
	mux.HandleFunc("GET "+p+"/e/{reg}/marks", h.marks)
	mux.HandleFunc("GET "+p+"/e/{reg}/events", h.events)
	mux.HandleFunc("POST "+p+"/e/{reg}/clear", h.clear)
	mux.HandleFunc("POST "+p+"/e/{reg}/delete", h.deleteEndpoint)
	mux.HandleFunc("POST "+p+"/e/{reg}/switch", h.switchEndpoint)
	mux.HandleFunc("POST "+p+"/e/{reg}/recheck", h.recheck)
	mux.HandleFunc("GET "+p+"/e/{reg}/report", h.report)
	mux.HandleFunc("GET "+p+"/e/{reg}/behaviour", h.behaviourPage)
	mux.HandleFunc("POST "+p+"/e/{reg}/behaviour", h.editBehaviour)
	mux.HandleFunc("GET "+p+"/r/{id}/body", h.downloadBody)
	mux.HandleFunc("GET "+p+"/r/{id}/image", h.image)
	mux.HandleFunc("POST "+p+"/r/{id}/replay", h.replayRequest)
	mux.HandleFunc("GET "+p+"/replays/{id}/body", h.replayBody)
	mux.HandleFunc("GET "+p+"/static/{file...}", h.static)
	mux.HandleFunc("GET "+p+"/login", h.loginPage)
	mux.HandleFunc("POST "+p+"/login", h.login)
	mux.HandleFunc("POST "+p+"/logout", h.logout)
	mux.HandleFunc("POST "+p+"/theme", h.setTheme)
	mux.HandleFunc("GET "+p+"/account", h.accountPage)
	mux.HandleFunc("POST "+p+"/account/password", h.changePassword)
	mux.HandleFunc("POST "+p+"/account/tokens", h.createToken)
	mux.HandleFunc("POST "+p+"/account/tokens/{id}/revoke", h.revokeOwnToken)
	mux.HandleFunc("GET "+p+"/users", h.usersPage)
	mux.HandleFunc("POST "+p+"/users", h.addUser)
	mux.HandleFunc("POST "+p+"/users/{name}/role", h.setRole)
	mux.HandleFunc("POST "+p+"/users/{name}/password", h.resetPassword)
	mux.HandleFunc("POST "+p+"/users/{name}/remove", h.removeUser)
	mux.HandleFunc("POST "+p+"/tokens/{id}/revoke", h.revokeAnyToken)

	// Browsers send Sec-Fetch-Site / Origin; a form posted from another site
	// (cross-site request forgery against a local server) is refused.
	h.csrf = http.NewCrossOriginProtection()
	h.mux = h.csrf.Handler(mux)

	if o.Replay == nil {
		o.Replay = replay.New(st, log, replay.Options{MaxWait: h.maxWait})
	}
	h.replay = o.Replay
	ao := api.Options{Prefix: prefix, Reserved: h.reserve, Catch: h.catch, MaxWait: h.maxWait, Stop: h.stop, Version: version.String(),
		Access: o.Access, Replay: o.Replay}
	if o.Keys != nil {
		ao.Keys, ao.Keyring = o.Keys, o.Keys // a nil *secret.Key must stay a nil interface
	}
	ao.Fonts, _ = fs.Sub(staticFS, "static")
	h.api = api.New(st, broker, log, ao)
	h.mcp = mcp.Handler(h.api, api.Base(prefix), version.String(), log, h.stop, o.MCPMaxSessions)
	h.mcpAuthed = withMCPIdentity(h.mcp)
	return h
}

// StopStreams ends the open live feeds; give it to server.Serve as an
// onShutdown function.
func (h *Handler) StopStreams() {
	h.stopOnce.Do(func() { close(h.stop) }) // first: waits (feeds, long-polls, MCP tools) end
	h.replay.Stop()                         // replays in flight end, recorded as stopped
	h.mcp.CloseSessions()                   // then the MCP sessions, which wait for their tools
}

// csp allows nothing from other origins: the UI is self-contained.
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; " +
	"connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	isMCP := r.URL.Path == h.prefix+"/mcp"
	isAPI := isMCP || api.Is(h.prefix, r.URL.Path)
	id, ok := h.identify(w, r, isAPI)
	if !ok {
		return
	}
	byToken := id != nil && id.Via != access.ViaSession
	if !byToken && !h.allowedHost(r.Host) {
		// A page on another site can point its own host name at this server
		// (DNS rebinding) and then read the UI as same-origin.
		h.log.Warn("web: request for an unknown host name refused", "host", r.Host)
		if isAPI {
			api.WriteError(w, http.StatusMisdirectedRequest, "wrong_host", fmt.Sprintf("hookwitness does not answer to the host name %q; "+
				"call it by IP address or localhost, or allow the name with -admin-hosts / HOOKWITNESS_ADMIN_HOSTS", r.Host), "")
			return
		}
		http.Error(w, fmt.Sprintf("The web UI does not answer to the host name %q. "+
			"Open it by IP address or localhost, or allow the name with -admin-hosts.", r.Host), http.StatusMisdirectedRequest)
		return
	}
	if id != nil {
		r = r.WithContext(access.With(r.Context(), id))
	}
	hd := w.Header()
	hd.Set("Content-Security-Policy", csp)
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Referrer-Policy", "same-origin")
	if isAPI {
		if err := h.csrf.Check(r); err != nil {
			api.WriteError(w, http.StatusForbidden, "cross_origin", "a request from a page on another site is refused", "")
			return
		}
		if !h.gateAPI(w, r, id, isMCP) {
			return
		}
		if isMCP {
			if id != nil {
				h.mcpAuthed.ServeHTTP(w, r)
				return
			}
			h.mcp.ServeHTTP(w, r)
			return
		}
		h.api.ServeHTTP(w, r)
		return
	}
	if !h.gateUI(w, r, id) {
		return
	}
	h.mux.ServeHTTP(w, r)
}

// allowedHost reports whether the UI answers to host (a Host header): an IP
// address, localhost or a *.localhost name, or one of the configured names.
func (h *Handler) allowedHost(host string) bool {
	name := host
	if hn, _, err := net.SplitHostPort(host); err == nil {
		name = hn
	}
	name = strings.ToLower(strings.TrimSuffix(strings.Trim(name, "[]"), "."))
	if _, err := netip.ParseAddr(name); err == nil {
		return true
	}
	return name == "localhost" || strings.HasSuffix(name, ".localhost") || h.hosts[name]
}

// regID reads the {reg} path value: an endpoint id, or "unmatched".
func regID(r *http.Request) (int64, bool) {
	s := r.PathValue("reg")
	if s == "unmatched" {
		return store.Unmatched, true
	}
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil && id > 0
}

func (h *Handler) regPath(id int64) string {
	if id == store.Unmatched {
		return h.prefix + "/e/unmatched"
	}
	return h.prefix + "/e/" + strconv.FormatInt(id, 10)
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	var buf bytes.Buffer
	if err := c.Render(r.Context(), &buf); err != nil {
		h.fail(w, "render", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (h *Handler) fail(w http.ResponseWriter, what string, err error) {
	h.log.Error("web: "+what, "err", err)
	http.Error(w, "Something went wrong on the server; the log has the details.", http.StatusInternalServerError)
}

// nav is what every page's masthead needs.
func (h *Handler) nav(r *http.Request, current int64, onRegister bool) (navView, error) {
	eps, err := h.store.Endpoints(r.Context())
	if err != nil {
		return navView{}, err
	}
	un, err := h.store.Endpoint(r.Context(), store.Unmatched)
	if err != nil {
		return navView{}, err
	}
	v := navView{
		Prefix:     h.prefix,
		Catch:      h.catch.Base(r),
		Endpoints:  eps,
		Unmatched:  un.Count,
		Current:    current,
		OnRegister: onRegister,
		Asset:      h.assetURL,
		Build:      version.String(),
		Theme:      theme(r),
		Here:       r.URL.RequestURI(),
	}
	if h.auth != nil {
		on, err := h.uiLoginOn(r.Context())
		if err != nil {
			return navView{}, err
		}
		v.NoLogin = !on
		if id := access.From(r.Context()); id != nil && id.Via == access.ViaSession {
			v.User, v.Admin = id.Name, id.Admin()
		}
	}
	return v, nil
}

func (h *Handler) endpointsPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d := endpointsDialog{Creating: q.Get("edit") == "new", Imported: q["imported"]}
	if q.Get("edit") == "import" {
		d.Import = &importView{}
	}
	d.Unset, _ = strconv.Atoi(q.Get("unset"))
	h.showEndpointsWith(w, r, http.StatusOK, d)
}

// endpointsDialog is what the endpoints page shows besides the list: the
// new-endpoint dialog, the import dialog, the outcome of an import.
type endpointsDialog struct {
	Creating bool
	FormPath string
	FormErr  string
	Import   *importView
	Imported []string // "created /path", "replaced /path", ...
	Unset    int      // secrets left without a value by the import
}

// showEndpoints renders the list; creating opens the new-endpoint dialog.
func (h *Handler) showEndpoints(w http.ResponseWriter, r *http.Request, status int, creating bool, formPath, formErr string) {
	h.showEndpointsWith(w, r, status, endpointsDialog{Creating: creating, FormPath: formPath, FormErr: formErr})
}

func (h *Handler) showEndpointsWith(w http.ResponseWriter, r *http.Request, status int, d endpointsDialog) {
	nav, err := h.nav(r, -1, false)
	if err != nil {
		h.fail(w, "endpoints", err)
		return
	}
	h.render(w, r, status, endpointsPage(nav, d))
}

// checkPath validates an endpoint path; the message is for the person who
// typed it.
func (h *Handler) checkPath(p string) string { return catch.CheckPath(h.reserve, p) }

func (h *Handler) createEndpoint(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimSpace(r.PostFormValue("path"))
	if p == "" {
		p = catch.RandomPath()
	}
	if msg := h.checkPath(p); msg != "" {
		h.showEndpoints(w, r, http.StatusUnprocessableEntity, true, p, msg)
		return
	}
	ep, err := h.store.CreateEndpoint(r.Context(), p)
	if errors.Is(err, store.ErrDuplicatePath) {
		h.showEndpoints(w, r, http.StatusUnprocessableEntity, true, p, p+" is already an endpoint.")
		return
	}
	if err != nil {
		h.fail(w, "create endpoint", err)
		return
	}
	h.log.Info("endpoint created", append([]any{"path", p}, who(r)...)...)
	http.Redirect(w, r, h.regPath(ep.ID), http.StatusSeeOther)
}

func (h *Handler) clear(w http.ResponseWriter, r *http.Request) {
	id, ok := regID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	err := h.store.ClearRequests(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, "clear", err)
		return
	}
	http.Redirect(w, r, h.back(r, h.regPath(id)), http.StatusSeeOther)
}

func (h *Handler) deleteEndpoint(w http.ResponseWriter, r *http.Request) {
	id, ok := regID(r)
	if !ok || id == store.Unmatched {
		http.NotFound(w, r)
		return
	}
	err := h.store.DeleteEndpoint(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, "delete endpoint", err)
		return
	}
	http.Redirect(w, r, h.prefix+"/", http.StatusSeeOther)
}

// switchEndpoint turns an endpoint off or on.
func (h *Handler) switchEndpoint(w http.ResponseWriter, r *http.Request) {
	id, ok := regID(r)
	if !ok || id == store.Unmatched {
		http.NotFound(w, r)
		return
	}
	on, err := strconv.ParseBool(r.PostFormValue("enabled"))
	if err != nil {
		http.Error(w, "enabled must be true or false.", http.StatusBadRequest)
		return
	}
	ep, err := h.store.Endpoint(r.Context(), id)
	if err == nil {
		err = h.store.UpdateEndpoint(r.Context(), id, store.EndpointChange{Enabled: &on})
	}
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, "switch endpoint", err)
		return
	}
	if on != ep.Enabled {
		state := "off"
		if on {
			state = "on"
		}
		h.log.Info("endpoint turned "+state, append([]any{"path", ep.Path}, who(r)...)...)
	}
	http.Redirect(w, r, h.back(r, h.regPath(id)), http.StatusSeeOther)
}

// back is the form's "next" page when it is one of ours, else def.
func (h *Handler) back(r *http.Request, def string) string {
	next := r.PostFormValue("next")
	if strings.HasPrefix(next, h.prefix+"/") && !strings.HasPrefix(next, "//") && !strings.ContainsAny(next, "\\\r\n") {
		return next
	}
	return def
}

func (h *Handler) registerPage(w http.ResponseWriter, r *http.Request) {
	reg, ok := regID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	ep, err := h.store.Endpoint(ctx, reg)
	if errors.Is(err, store.ErrNotFound) {
		h.notFound(w, r, "This endpoint does not exist (any more).")
		return
	}
	if err != nil {
		h.fail(w, "register", err)
		return
	}

	var ex *exhibitView
	if s := r.PathValue("id"); s != "" {
		id, _ := strconv.ParseInt(s, 10, 64)
		req, err := h.store.Request(ctx, id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && req.EndpointID != reg) {
			h.notFound(w, r, "This request is no longer kept: it was cleared, pruned or deleted.")
			return
		}
		if err != nil {
			h.fail(w, "request", err)
			return
		}
		v, err := h.exhibit(ctx, req)
		if err != nil {
			h.fail(w, "request", err)
			return
		}
		ex = &v
		// A click in the register swaps only the exhibit.
		if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-History-Restore-Request") != "true" {
			h.render(w, r, http.StatusOK, exhibit(v))
			return
		}
	}

	var export *exportView
	if r.URL.Query().Has("report") {
		export = h.exportDefaults(reg, ep, ex, r.URL.Path)
	}
	var draft *draftDialogView
	if r.URL.Query().Has("draft") && reg != store.Unmatched {
		if draft, err = h.draftDefaults(ctx, ep, ex, r.URL.Path); err != nil {
			h.fail(w, "schema draft", err)
			return
		}
	}
	var replayDialog *replayDialogView
	if r.URL.Query().Has("replay") && ex != nil {
		req, err := h.store.Request(ctx, ex.ID)
		if err == nil {
			replayDialog, err = h.replayDefaults(ctx, req, ex)
		}
		if err != nil {
			h.fail(w, "replay", err)
			return
		}
	}
	h.showRegister(w, r, reg, ep, ex, export, draft, replayDialog, http.StatusOK)
}

// showRegister renders a register page, with an exhibit and the report
// dialog when given.
func (h *Handler) showRegister(w http.ResponseWriter, r *http.Request, reg int64, ep store.Endpoint, ex *exhibitView, export *exportView,
	draft *draftDialogView, replay *replayDialogView, status int) {
	ctx := r.Context()
	reqs, err := h.store.Requests(ctx, reg, 0, pageSize+1)
	if err != nil {
		h.fail(w, "register", err)
		return
	}
	rows, more, err := h.rows(ctx, reqs)
	if err != nil {
		h.fail(w, "register", err)
		return
	}
	var newest int64
	if len(rows) > 0 {
		newest = rows[0].ID
	}
	nav, err := h.nav(r, reg, true)
	if err != nil {
		h.fail(w, "register", err)
		return
	}
	var verdicts verdictsView
	if reg != store.Unmatched {
		if verdicts, err = h.verdictsView(r, ep); err != nil {
			h.fail(w, "register", err)
			return
		}
	}
	h.render(w, r, status, registerPage(nav, registerView{
		Verdicts: verdicts,
		ID:       reg,
		Path:     ep.Path,
		Enabled:  ep.Enabled || reg == store.Unmatched,
		Unset:    unsetSecrets(ep.Config),
		Count:    ep.Count,
		Rows:     rows,
		More:     more,
		Newest:   newest,
		Base:     h.regPath(reg),
		Selected: selectedID(ex),
		Keep:     h.keep,
		Export:   export,
		Draft:    draft,
		Replay:   replay,
	}, ex))
}

func selectedID(ex *exhibitView) int64 {
	if ex == nil {
		return 0
	}
	return ex.ID
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request, msg string) {
	nav, err := h.nav(r, -1, false)
	if err != nil {
		h.fail(w, "not found", err)
		return
	}
	h.render(w, r, http.StatusNotFound, notFoundPage(nav, msg))
}

// rows turns up to pageSize+1 requests into rows; more reports the extra one.
func (h *Handler) rows(ctx context.Context, reqs []store.Request) ([]rowView, bool, error) {
	more := len(reqs) > pageSize
	if more {
		reqs = reqs[:pageSize]
	}
	nums, err := deliveryNumbers(h.store, ctx, reqs)
	if err != nil {
		return nil, false, err
	}
	out := make([]rowView, 0, len(reqs))
	for _, req := range reqs {
		out = append(out, h.row(req, nums[req.ID], false))
	}
	return out, more, nil
}

// deliveryNumbers numbers the deliveries of a page of rows in one query (a
// seam for the test that counts them).
var deliveryNumbers = (*store.Store).DeliveryNumbers

// row is one request of a register; n is its delivery number.
func (h *Handler) row(req store.Request, n int, fresh bool) rowView {
	v := rowView{
		ID:       req.ID,
		No:       req.No,
		Method:   req.Method,
		At:       req.ReceivedAt,
		Size:     req.Size,
		TooLarge: req.TooLarge,
		Status:   req.Status,
		Delivery: n,
		Href:     h.regPath(req.EndpointID) + "/r/" + strconv.FormatInt(req.ID, 10),
		Fresh:    fresh,
		Outcome:  emulate.ParseOutcome(req.Outcome, req.Status),
		Verdict:  req.Verdict,
		Replays:  req.Replays,
	}
	if len(req.BodySHA256) > 0 {
		v.Group = hex.EncodeToString(req.BodySHA256[:6])
	}
	if req.EndpointID == store.Unmatched {
		v.Detail = req.Path
		if req.RawQuery != "" {
			v.Detail += "?" + req.RawQuery
		}
	} else {
		v.Detail = contentType(req.Headers)
		if req.RawQuery != "" {
			v.Detail = "?" + req.RawQuery + "  " + v.Detail
		}
	}
	return v
}

func contentType(hs []store.Header) string {
	for _, hd := range hs {
		if strings.EqualFold(hd.Name, "Content-Type") {
			return hd.Value
		}
	}
	return ""
}

func (h *Handler) olderRows(w http.ResponseWriter, r *http.Request) {
	reg, ok := regID(r)
	before, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	if !ok || err != nil || before <= 0 {
		http.NotFound(w, r)
		return
	}
	reqs, err := h.store.Requests(r.Context(), reg, before, pageSize+1)
	if err != nil {
		h.fail(w, "older rows", err)
		return
	}
	rows, more, err := h.rows(r.Context(), reqs)
	if err != nil {
		h.fail(w, "older rows", err)
		return
	}
	h.render(w, r, http.StatusOK, rowsChunk(rows, more, h.regPath(reg)))
}

func (h *Handler) exhibit(ctx context.Context, req store.Request) (exhibitView, error) {
	n, err := h.store.Deliveries(ctx, req)
	if err != nil {
		return exhibitView{}, err
	}
	v := exhibitView{
		ID:       req.ID,
		No:       req.No,
		Method:   req.Method,
		Path:     req.Path,
		RawQuery: req.RawQuery,
		Proto:    req.Proto,
		At:       req.ReceivedAt,
		From:     req.RemoteAddr,
		Size:     req.Size,
		Kept:     int64(len(req.Body)),
		TooLarge: req.TooLarge,
		Status:   req.Status,
		Delivery: n,
		Headers:  req.Headers,
		Query:    parseQuery(req.RawQuery),
		Body:     viewBody(req.Body),
		Download: h.prefix + "/r/" + strconv.FormatInt(req.ID, 10) + "/body",
		Self:     h.regPath(req.EndpointID) + "/r/" + strconv.FormatInt(req.ID, 10),
		Unmatch:  req.EndpointID == store.Unmatched,
		Limit:    h.limit,
		Outcome:  emulate.ParseOutcome(req.Outcome, req.Status),
		Verdicts: parseVerdicts(req.Verdicts),
	}
	if bad := badPaths(v.Verdicts); len(bad) > 0 && v.Body.Kind == bodyJSON {
		markBad(v.Body.JSON, bad)
		for _, t := range v.Body.JSON {
			if t.Bad {
				v.BadPaths++
			}
		}
	}
	v.Images = h.images(req, v.Verdicts, &v.Body)
	v.CanReplay = !req.TooLarge && int64(len(req.Body)) >= req.Size
	if req.Replays > 0 {
		if v.Replays, err = h.replays(ctx, req.ID); err != nil {
			return exhibitView{}, err
		}
	}
	return v, nil
}

func (h *Handler) downloadBody(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	req, err := h.store.Request(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && req.TooLarge) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, "download", err)
		return
	}
	// Always a download, never rendered: the body is whatever a sender sent.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="request-%d.body"`, req.ID))
	w.Header().Set("Content-Length", strconv.Itoa(len(req.Body)))
	w.Header().Set("Cache-Control", "no-store")
	w.Write(req.Body)
}

// events is the live feed of one register as Server-Sent Events. It first
// sends what the client missed (after the Last-Event-ID header, or the
// "after" parameter the page was rendered with), then each new request once.
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	reg, ok := regID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	last, err := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	if err != nil {
		last, _ = strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	}
	rc := http.NewResponseController(w)
	wake, unsubscribe := h.live.Subscribe(reg)
	defer unsubscribe()

	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 2000\n\n")

	// since: outcomes updated from then on are sent again (as "update"
	// events, without an id) so rows show what was finally done.
	since := time.Now().UnixMilli()
	writeRow := func(req store.Request, n int, event string) bool {
		v := h.row(req, n, event == "request")
		var buf bytes.Buffer
		if err := row(v).Render(r.Context(), &buf); err != nil {
			return false
		}
		if event == "request" {
			fmt.Fprintf(w, "id: %d\n", req.ID)
		}
		fmt.Fprintf(w, "event: %s\n", event)
		for line := range strings.SplitSeq(strings.TrimRight(buf.String(), "\n"), "\n") {
			fmt.Fprintf(w, "data: %s\n", line)
		}
		fmt.Fprint(w, "\n")
		return true
	}
	// writeCounts sends the register's totals after rows changed, so the
	// header's kept and verdict counts follow without a reload (rows past
	// the retention limit or not loaded yet count too).
	writeCounts := func() error {
		ep, err := h.store.Endpoint(r.Context(), reg)
		if err != nil {
			return err
		}
		c := map[string]int{}
		if reg != store.Unmatched {
			if c, err = h.store.VerdictCounts(r.Context(), reg); err != nil {
				return err
			}
		}
		b, _ := json.Marshal(map[string]int{"kept": ep.Count, "pass": c["pass"], "fail": c["fail"], "nv": c["n/v"], "nc": c["n/c"]})
		fmt.Fprintf(w, "event: counts\ndata: %s\n\n", b)
		return nil
	}
	send := func() bool {
		now := time.Now().UnixMilli()
		wrote := false
		reqs, err := h.store.RequestsAfter(r.Context(), reg, last)
		var changed []store.Request
		if err == nil && last > 0 {
			changed, err = h.store.RequestsChanged(r.Context(), reg, last, since)
		}
		var nums map[int64]int
		if err == nil && len(changed)+len(reqs) > 0 {
			nums, err = deliveryNumbers(h.store, r.Context(), append(changed[:len(changed):len(changed)], reqs...))
		}
		for _, req := range changed {
			if err != nil {
				break
			}
			if !writeRow(req, nums[req.ID], "update") {
				return false
			}
			wrote = true
		}
		for _, req := range reqs {
			if err != nil {
				break
			}
			if !writeRow(req, nums[req.ID], "request") {
				return false
			}
			last, wrote = req.ID, true
		}
		if err == nil && wrote {
			err = writeCounts()
		}
		if err != nil {
			if r.Context().Err() == nil {
				h.log.Error("web: live feed", "err", err)
			}
			return false
		}
		since = now
		return rc.Flush() == nil
	}

	if !send() {
		return
	}
	tick := time.NewTicker(heartbeat)
	defer tick.Stop()
	check := time.NewTicker(sessionCheck)
	defer check.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-h.stop:
			return
		case <-wake:
			if !send() {
				return
			}
		case <-tick.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			if rc.Flush() != nil {
				return
			}
		case <-check.C:
			// The login of this page ended (logout, expiry, a password or role
			// change): the page reloads onto the login page. An open feed
			// otherwise counts as activity, which the check records.
			if h.sessionEnded(r) {
				fmt.Fprint(w, "event: login\ndata: ended\n\n")
				rc.Flush()
				return
			}
		}
	}
}

// hashAssets fingerprints the static files so their URLs change with them
// and can be cached for good.
func hashAssets() map[string]string {
	out := map[string]string{}
	fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := staticFS.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[strings.TrimPrefix(p, "static/")] = hex.EncodeToString(sum[:4])
		return nil
	})
	return out
}

// assetURL is the cache-busting URL of a static file, e.g. "app.css".
func (h *Handler) assetURL(name string) string {
	return h.prefix + "/static/" + name + "?v=" + h.assets[name]
}

var staticTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".woff2": "font/woff2",
}

func (h *Handler) static(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	sum, ok := h.assets[name]
	ctype, served := staticTypes[path.Ext(name)]
	if !ok || !served {
		http.NotFound(w, r)
		return
	}
	b, err := staticFS.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("ETag", `"`+sum+`"`)
	if r.URL.Query().Get("v") == sum {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(b))
}
