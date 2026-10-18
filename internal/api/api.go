// Package api is the JSON API under <prefix>/api/v1: endpoints, their
// behaviour and secrets, caught requests, and waiting for the next one. It
// is mounted by internal/web, behind the UI's host check and cross-origin
// protection, so whatever protects the UI protects it too.
package api

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/petroladkin/hookwitness/internal/access"
	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/live"
	"github.com/petroladkin/hookwitness/internal/store"
)

// maxBody bounds a request body sent to the API.
const maxBody = 1 << 20

// Options configure the API.
type Options struct {
	Prefix   string          // the admin prefix, e.g. /admin; the API is at Prefix+"/api/v1"
	Reserved string          // a path prefix endpoints may not use: the admin prefix when it shares the catch listener, else ""
	Catch    CatchURL        // where senders reach the catch listener: the endpoints' url
	MaxWait  time.Duration   // longest delay an endpoint may configure
	Keys     emulate.Sealer  // seals and hashes secrets; nil refuses to set them
	Keyring  emulate.Keyring // opens secrets for re-checks; nil: checks needing one are n/v
	Stop     <-chan struct{} // closed on shutdown: waiting calls answer at once
	Version  string          // the build, named in reports
	Fonts    fs.FS           // the UI's fonts, embedded in HTML reports
	// Access checks passwords and makes tokens; nil: login is off for good
	// (tests). The caller's identity comes in the request's context
	// (internal/access), put there by internal/web.
	Access *access.Authenticator
}

// Handler serves the API. Create it with New.
type Handler struct {
	store *store.Store
	live  *live.Broker
	log   *slog.Logger
	o     Options
	base  string
	mux   *http.ServeMux

	// beforeSave, when set (tests), runs between reading a behaviour and
	// saving its replacement: the window another client can change it in.
	beforeSave func()
}

// New returns the API.
func New(st *store.Store, broker *live.Broker, log *slog.Logger, o Options) *Handler {
	if o.MaxWait == 0 {
		o.MaxWait = 5 * time.Minute
	}
	h := &Handler{store: st, live: broker, log: log, o: o, base: Base(o.Prefix), mux: http.NewServeMux()}
	b := h.base
	for _, rt := range h.routes() {
		h.mux.HandleFunc(rt.method+" "+b+rt.path, h.guard(rt.need, rt.fn))
	}
	h.mux.HandleFunc("GET "+b+"/openapi.json", h.guard(needAny, serveOpenAPI))
	h.mux.HandleFunc(b+"/", h.guard(needAny, func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no route %s %s", r.Method, r.URL.Path), "")
	}))
	return h
}

// route is one operation of the API; openapi.json documents each. need is
// what it asks of the caller once login is on.
type route struct {
	method, path string
	fn           http.HandlerFunc
	need         need
}

func (h *Handler) routes() []route {
	return []route{
		{"GET", "/endpoints", h.listEndpoints, needAny},
		{"POST", "/endpoints", h.createEndpoint, needAny},
		{"GET", "/endpoints/{id}", h.getEndpoint, needAny},
		{"PATCH", "/endpoints/{id}", h.patchEndpoint, needAny},
		{"DELETE", "/endpoints/{id}", h.deleteEndpoint, needAny},
		{"GET", "/endpoints/{id}/behaviour", h.getBehaviour, needAny},
		{"PUT", "/endpoints/{id}/behaviour", h.putBehaviour, needAny},
		{"PUT", "/endpoints/{id}/secrets/{name}", h.putSecret, needAny},
		{"DELETE", "/endpoints/{id}/secrets/{name}", h.deleteSecret, needAny},
		{"POST", "/endpoints/{id}/reset-counter", h.resetCounter, needAny},
		{"POST", "/endpoints/{id}/recheck", h.recheck, needAny},
		{"POST", "/endpoints/{id}/schema-draft", h.schemaDraft, needAny},
		{"GET", "/endpoints/{id}/report", h.endpointReport, needAny},
		{"GET", "/endpoints/{id}/requests", h.listRequests, needAny},
		{"DELETE", "/endpoints/{id}/requests", h.clearRequests, needAny},
		{"GET", "/requests/{id}", h.getRequest, needAny},
		{"GET", "/requests/{id}/body", h.rawBody, needAny},
		{"GET", "/requests/{id}/image", h.getImage, needAny},
		{"GET", "/setup", h.exportSetup, needAny},
		{"POST", "/setup", h.importSetup, needAny},
		{"GET", "/unmatched/requests", h.listUnmatched, needAny},
		{"GET", "/unmatched/report", h.unmatchedReport, needAny},
		{"GET", "/me", h.me, needAny},
		{"PUT", "/me/password", h.changeOwnPassword, needSession},
		{"GET", "/me/tokens", h.listTokens(ownID), needSession},
		{"POST", "/me/tokens", h.createToken, needSession},
		{"DELETE", "/me/tokens/{id}", h.revokeToken(ownID), needSession},
		{"GET", "/users", h.listUsers, needAdmin},
		{"POST", "/users", h.addUser, needAdmin},
		{"PATCH", "/user/{name}", h.patchUser, needAdmin},
		{"DELETE", "/user/{name}", h.removeUser, needAdmin},
		{"GET", "/tokens", h.listTokens(everyone), needAdmin},
		{"DELETE", "/tokens/{id}", h.revokeToken(everyone), needAdmin},
	}
}

//go:embed openapi.json
var openAPI []byte

// serveOpenAPI is the API's description (OpenAPI 3.1).
func serveOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(openAPI)
}

// Base is where the API is mounted under an admin prefix.
func Base(prefix string) string { return prefix + "/api/v1" }

// Is reports whether a request path is under the API of an admin prefix.
func Is(prefix, path string) bool {
	b := Base(prefix)
	return path == b || strings.HasPrefix(path, b+"/")
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
		// A cross-site form cannot send this content type without a CORS
		// preflight, which this server never answers.
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
			WriteError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "send the body as JSON with Content-Type: application/json", "")
			return
		}
	}
	h.mux.ServeHTTP(w, r)
}

// errorBody is the one error shape.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Field   string `json:"field,omitempty"`
	} `json:"error"`
}

// WriteError answers with the API's error shape; field names the part of
// the request at fault, a JSON path such as "checks[0].secret".
func WriteError(w http.ResponseWriter, status int, code, msg, field string) {
	var b errorBody
	b.Error.Code, b.Error.Message, b.Error.Field = code, msg, field
	writeJSON(w, status, b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		status, b = http.StatusInternalServerError, []byte(`{"error":{"code":"internal","message":"the answer could not be encoded"}}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}

func (h *Handler) fail(w http.ResponseWriter, what string, err error) {
	h.log.Error("api: "+what, "err", err)
	WriteError(w, http.StatusInternalServerError, "internal", "something went wrong on the server; the log has the details", "")
}

// decode reads a JSON body into v: unknown fields, trailing data and bodies
// over maxBody are refused. With emptyOK an empty body leaves v as it is.
// It answers the error itself and reports whether v is ready.
func decode(w http.ResponseWriter, r *http.Request, v any, emptyOK bool) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if errors.Is(err, io.EOF) && emptyOK {
		return true
	}
	if err == nil && dec.Decode(&struct{}{}) != io.EOF {
		err = errors.New("more than one JSON value in the body")
	}
	if err == nil {
		return true
	}
	var tooLarge *http.MaxBytesError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &tooLarge):
		WriteError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("the body is over %d bytes", maxBody), "")
	case errors.As(err, &typ):
		WriteError(w, http.StatusBadRequest, "bad_json", fmt.Sprintf("%s must be %s", typ.Field, typ.Type), typ.Field)
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		name, _ := strconv.Unquote(strings.TrimPrefix(err.Error(), "json: unknown field "))
		WriteError(w, http.StatusBadRequest, "unknown_field", fmt.Sprintf("unknown field %q", name), name)
	case errors.Is(err, io.EOF):
		WriteError(w, http.StatusBadRequest, "bad_json", "the body is empty; send a JSON object", "")
	default:
		WriteError(w, http.StatusBadRequest, "bad_json", "the body is not valid JSON: "+err.Error(), "")
	}
	return false
}

// pathID reads a positive numeric path value.
func pathID(r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	return id, err == nil && id > 0
}

// endpoint loads the endpoint a route names, answering 404 itself.
func (h *Handler) endpoint(w http.ResponseWriter, r *http.Request) (store.Endpoint, bool) {
	id, ok := pathID(r, "id")
	if !ok {
		WriteError(w, http.StatusNotFound, "not_found", "no endpoint "+r.PathValue("id"), "")
		return store.Endpoint{}, false
	}
	ep, err := h.store.Endpoint(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no endpoint %d", id), "")
		return store.Endpoint{}, false
	}
	if err != nil {
		h.fail(w, "endpoint", err)
		return store.Endpoint{}, false
	}
	return ep, true
}
