// Package web is the browser UI under the admin prefix: endpoints, the live
// register of caught requests, and each request as an exhibit.
package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
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

	"github.com/petroladkin/hookwitness/internal/api"
	"github.com/petroladkin/hookwitness/internal/catch"
	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/live"
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
	store   *store.Store
	live    *live.Broker
	log     *slog.Logger
	prefix  string
	limit   int64 // the body limit, shown when a body was refused
	keep    int   // retention per register, so the page can drop pruned rows
	maxWait time.Duration
	hosts   map[string]bool
	keys    *secret.Key
	mux     http.Handler
	csrf    *http.CrossOriginProtection
	api     *api.Handler      // the JSON API under prefix/api/v1
	assets  map[string]string // asset path -> content hash for cache busting

	stopOnce sync.Once
	stop     chan struct{}
}

// Options configure the UI.
type Options struct {
	Prefix    string        // where the UI is mounted, e.g. /admin
	MaxBody   int64         // the catcher's body limit, shown when a body was refused
	Retention int           // requests kept per register
	MaxWait   time.Duration // longest delay an endpoint may configure
	Hosts     []string      // host names the UI answers to besides IP addresses and localhost
	Keys      *secret.Key   // seals and hashes endpoint secrets; verifies on re-check
}

// New returns the UI.
func New(st *store.Store, broker *live.Broker, log *slog.Logger, o Options) *Handler {
	h := &Handler{store: st, live: broker, log: log, prefix: o.Prefix, limit: o.MaxBody, keep: o.Retention, maxWait: o.MaxWait, keys: o.Keys,
		hosts: map[string]bool{}, stop: make(chan struct{})}
	if h.maxWait == 0 {
		h.maxWait = 5 * time.Minute
	}
	for _, name := range o.Hosts {
		h.hosts[strings.ToLower(name)] = true
	}
	prefix := o.Prefix
	h.assets = hashAssets()

	mux := http.NewServeMux()
	p := prefix
	mux.HandleFunc("GET "+p, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, p+"/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET "+p+"/{$}", h.endpointsPage)
	mux.HandleFunc("POST "+p+"/endpoints", h.createEndpoint)
	mux.HandleFunc("GET "+p+"/e/{reg}", h.registerPage)
	mux.HandleFunc("GET "+p+"/e/{reg}/r/{id}", h.registerPage)
	mux.HandleFunc("GET "+p+"/e/{reg}/rows", h.olderRows)
	mux.HandleFunc("GET "+p+"/e/{reg}/events", h.events)
	mux.HandleFunc("POST "+p+"/e/{reg}/clear", h.clear)
	mux.HandleFunc("POST "+p+"/e/{reg}/delete", h.deleteEndpoint)
	mux.HandleFunc("POST "+p+"/e/{reg}/recheck", h.recheck)
	mux.HandleFunc("GET "+p+"/e/{reg}/report", h.report)
	mux.HandleFunc("GET "+p+"/e/{reg}/behaviour", h.behaviourPage)
	mux.HandleFunc("POST "+p+"/e/{reg}/behaviour", h.editBehaviour)
	mux.HandleFunc("GET "+p+"/r/{id}/body", h.downloadBody)
	mux.HandleFunc("GET "+p+"/r/{id}/image", h.image)
	mux.HandleFunc("GET "+p+"/static/{file...}", h.static)

	// Browsers send Sec-Fetch-Site / Origin; a form posted from another site
	// (cross-site request forgery against a local server) is refused.
	h.csrf = http.NewCrossOriginProtection()
	h.mux = h.csrf.Handler(mux)

	ao := api.Options{Prefix: prefix, MaxWait: h.maxWait, Stop: h.stop, Version: version.Version}
	if o.Keys != nil {
		ao.Keys, ao.Keyring = o.Keys, o.Keys // a nil *secret.Key must stay a nil interface
	}
	ao.Fonts, _ = fs.Sub(staticFS, "static")
	h.api = api.New(st, broker, log, ao)
	return h
}

// StopStreams ends the open live feeds; give it to server.Serve as an
// onShutdown function.
func (h *Handler) StopStreams() { h.stopOnce.Do(func() { close(h.stop) }) }

// csp allows nothing from other origins: the UI is self-contained.
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; " +
	"connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	isAPI := api.Is(h.prefix, r.URL.Path)
	if !h.allowedHost(r.Host) {
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
	hd := w.Header()
	hd.Set("Content-Security-Policy", csp)
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Referrer-Policy", "same-origin")
	if isAPI {
		if err := h.csrf.Check(r); err != nil {
			api.WriteError(w, http.StatusForbidden, "cross_origin", "a request from a page on another site is refused", "")
			return
		}
		h.api.ServeHTTP(w, r)
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
	return navView{
		Prefix:     h.prefix,
		Host:       r.Host,
		Endpoints:  eps,
		Unmatched:  un.Count,
		Current:    current,
		OnRegister: onRegister,
		Asset:      h.assetURL,
	}, nil
}

func (h *Handler) endpointsPage(w http.ResponseWriter, r *http.Request) {
	h.showEndpoints(w, r, http.StatusOK, r.URL.Query().Get("edit") == "new", "", "")
}

// showEndpoints renders the list; creating opens the new-endpoint dialog.
func (h *Handler) showEndpoints(w http.ResponseWriter, r *http.Request, status int, creating bool, formPath, formErr string) {
	nav, err := h.nav(r, -1, false)
	if err != nil {
		h.fail(w, "endpoints", err)
		return
	}
	h.render(w, r, status, endpointsPage(nav, creating, formPath, formErr))
}

// checkPath validates an endpoint path; the message is for the person who
// typed it.
func (h *Handler) checkPath(p string) string { return catch.CheckPath(h.prefix, p) }

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
	h.log.Info("endpoint created", "path", p)
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
	h.showRegister(w, r, reg, ep, ex, export, draft, http.StatusOK)
}

// showRegister renders a register page, with an exhibit and the report
// dialog when given.
func (h *Handler) showRegister(w http.ResponseWriter, r *http.Request, reg int64, ep store.Endpoint, ex *exhibitView, export *exportView, draft *draftDialogView, status int) {
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
		Count:    ep.Count,
		Rows:     rows,
		More:     more,
		Newest:   newest,
		Base:     h.regPath(reg),
		Selected: selectedID(ex),
		Keep:     h.keep,
		Export:   export,
		Draft:    draft,
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
	out := make([]rowView, 0, len(reqs))
	for _, req := range reqs {
		v, err := h.row(ctx, req, false)
		if err != nil {
			return nil, false, err
		}
		out = append(out, v)
	}
	return out, more, nil
}

func (h *Handler) row(ctx context.Context, req store.Request, fresh bool) (rowView, error) {
	n, err := h.store.Deliveries(ctx, req)
	if err != nil {
		return rowView{}, err
	}
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
	return v, nil
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
	writeRow := func(req store.Request, event string) bool {
		v, err := h.row(r.Context(), req, event == "request")
		if err != nil {
			return false
		}
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
	send := func() bool {
		now := time.Now().UnixMilli()
		reqs, err := h.store.RequestsAfter(r.Context(), reg, last)
		if err == nil && last > 0 {
			var changed []store.Request
			changed, err = h.store.RequestsChanged(r.Context(), reg, last, since)
			for _, req := range changed {
				if !writeRow(req, "update") {
					return false
				}
			}
		}
		if err != nil {
			if r.Context().Err() == nil {
				h.log.Error("web: live feed", "err", err)
			}
			return false
		}
		for _, req := range reqs {
			if !writeRow(req, "request") {
				return false
			}
			last = req.ID
		}
		since = now
		return rc.Flush() == nil
	}

	if !send() {
		return
	}
	tick := time.NewTicker(heartbeat)
	defer tick.Stop()
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
