// Package catch records the requests that senders deliver to endpoints.
package catch

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/petroladkin/hookwitness/internal/live"
	"github.com/petroladkin/hookwitness/internal/store"
	"github.com/petroladkin/hookwitness/internal/wire"
)

// Handler catches requests to every path that is not reserved.
type Handler struct {
	Store     *store.Store
	Live      *live.Broker
	Log       *slog.Logger
	MaxBody   int64 // larger bodies are answered 413 and not kept
	Retention int   // requests kept per register
}

// Catch answers and records r. headers are its header lines as received
// (wire.Take); ok false falls back to the order net/http keeps.
func (h *Handler) Catch(w http.ResponseWriter, r *http.Request, headers []wire.Header, ok bool) {
	received := time.Now().UTC()
	p := RawPath(r)

	endpointID, err := h.Store.EndpointIDByPath(r.Context(), p)
	switch {
	case errors.Is(err, store.ErrNotFound):
		endpointID = store.Unmatched
	case err != nil:
		h.Log.Error("catch: endpoint lookup failed", "path", p, "err", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	req := &store.Request{
		EndpointID: endpointID,
		ReceivedAt: received,
		Method:     r.Method,
		Path:       p,
		RawQuery:   rawQuery(r),
		Proto:      r.Proto,
		RemoteAddr: r.RemoteAddr,
		Headers:    storeHeaders(r, headers, ok),
	}
	if !h.readBody(r, req) {
		h.Log.Warn("catch: body could not be read; request not kept", "method", r.Method, "path", p, "remote", r.RemoteAddr)
		http.Error(w, "could not read the request body", http.StatusBadRequest)
		return
	}

	switch {
	case endpointID == store.Unmatched:
		req.Status = http.StatusNotFound
	case req.TooLarge:
		req.Status = http.StatusRequestEntityTooLarge
	default:
		req.Status = http.StatusOK
	}
	err = h.Store.AddRequest(r.Context(), req, h.Retention)
	if errors.Is(err, store.ErrNotFound) {
		// The endpoint was deleted after the lookup: the path is unknown now.
		req.EndpointID, req.Status = store.Unmatched, http.StatusNotFound
		err = h.Store.AddRequest(r.Context(), req, h.Retention)
	}
	if err != nil {
		h.Log.Error("catch: request not stored", "method", r.Method, "path", p, "err", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	h.Live.Publish(req.EndpointID)
	if req.EndpointID == store.Unmatched {
		h.Log.Info("unmatched request", "method", r.Method, "path", p, "remote", r.RemoteAddr, "no", req.No)
	}

	if req.TooLarge {
		// The rest of the body is not wanted; do not drain it.
		w.Header().Set("Connection", "close")
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(req.Status)
}

// readBody fills req's body fields; false means the body could not be read
// (the sender went away or stalled).
func (h *Handler) readBody(r *http.Request, req *store.Request) bool {
	if r.ContentLength > h.MaxBody {
		req.TooLarge, req.Size = true, r.ContentLength
		return true
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, h.MaxBody+1))
	if err != nil {
		return false
	}
	if int64(len(body)) > h.MaxBody {
		req.TooLarge, req.Size = true, r.ContentLength // -1 when not declared
		return true
	}
	req.Body, req.Size = body, int64(len(body))
	return true
}

// RawPath is the request path exactly as the sender wrote it, without the
// query: endpoints match it byte for byte, so no cleaning or decoding.
func RawPath(r *http.Request) string {
	if strings.HasPrefix(r.RequestURI, "/") {
		p, _, _ := strings.Cut(r.RequestURI, "?")
		return p
	}
	if r.RequestURI == "*" {
		return "*"
	}
	// Absolute form (a proxy-style request line).
	return r.URL.EscapedPath()
}

func rawQuery(r *http.Request) string {
	if _, q, found := strings.Cut(r.RequestURI, "?"); found {
		return q
	}
	return r.URL.RawQuery
}

func storeHeaders(r *http.Request, hs []wire.Header, ok bool) []store.Header {
	if ok {
		out := make([]store.Header, len(hs))
		for i, h := range hs {
			out[i] = store.Header{Name: h.Name, Value: h.Value}
		}
		return out
	}
	// Order unknown: Host first, then the names sorted; repeated values keep
	// their order.
	out := []store.Header{{Name: "Host", Value: r.Host}}
	names := make([]string, 0, len(r.Header))
	for n := range r.Header {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, v := range r.Header[n] {
			out = append(out, store.Header{Name: n, Value: v})
		}
	}
	return out
}
