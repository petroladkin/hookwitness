package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/petroladkin/hookwitness/internal/catch"
	"github.com/petroladkin/hookwitness/internal/store"
)

// endpointJSON is an endpoint as the API shows it.
type endpointJSON struct {
	ID       int64  `json:"id"`
	Path     string `json:"path"`
	URL      string `json:"url"` // where senders deliver, as reached by this API call
	Requests int    `json:"requests"`
	Counter  int64  `json:"counter"` // requests that passed its checks since the last reset
	Created  stamp  `json:"created_at"`
	Last     *stamp `json:"last_request_at,omitempty"`
}

func endpointView(r *http.Request, ep store.Endpoint) endpointJSON {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	v := endpointJSON{ID: ep.ID, Path: ep.Path, URL: scheme + "://" + r.Host + ep.Path,
		Requests: ep.Count, Counter: ep.Counter, Created: stamp(ep.CreatedAt)}
	if !ep.LastAt.IsZero() {
		last := stamp(ep.LastAt)
		v.Last = &last
	}
	return v
}

func (h *Handler) listEndpoints(w http.ResponseWriter, r *http.Request) {
	eps, err := h.store.Endpoints(r.Context())
	if err != nil {
		h.fail(w, "list endpoints", err)
		return
	}
	q := r.URL.Query()
	out := []endpointJSON{}
	for _, ep := range eps {
		if q.Has("path") && ep.Path != q.Get("path") {
			continue
		}
		out = append(out, endpointView(r, ep))
	}
	writeJSON(w, http.StatusOK, map[string]any{"endpoints": out})
}

func (h *Handler) createEndpoint(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path string `json:"path"`
	}
	if !decode(w, r, &in, true) {
		return
	}
	p := strings.TrimSpace(in.Path)
	if p == "" {
		p = catch.RandomPath()
	}
	if msg := catch.CheckPath(h.o.Reserved, p); msg != "" {
		WriteError(w, http.StatusUnprocessableEntity, "invalid", msg, "path")
		return
	}
	created, err := h.store.CreateEndpoint(r.Context(), p)
	if errors.Is(err, store.ErrDuplicatePath) {
		WriteError(w, http.StatusConflict, "duplicate_path", p+" is already an endpoint.", "path")
		return
	}
	if err != nil {
		h.fail(w, "create endpoint", err)
		return
	}
	ep, err := h.store.Endpoint(r.Context(), created.ID)
	if err != nil {
		h.fail(w, "create endpoint", err)
		return
	}
	h.log.Info("endpoint created", "path", p, "via", "api")
	w.Header().Set("Location", h.base+"/endpoints/"+strconv.FormatInt(ep.ID, 10))
	writeJSON(w, http.StatusCreated, endpointView(r, ep))
}

func (h *Handler) getEndpoint(w http.ResponseWriter, r *http.Request) {
	ep, ok := h.endpoint(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, endpointView(r, ep))
}
