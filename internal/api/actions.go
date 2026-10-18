package api

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/petroladkin/hookwitness/internal/catch"
	"github.com/petroladkin/hookwitness/internal/report"
	"github.com/petroladkin/hookwitness/internal/store"
)

// patchEndpoint moves an endpoint to another path, validated as the UI
// validates a new one; its requests and behaviour stay.
func (h *Handler) patchEndpoint(w http.ResponseWriter, r *http.Request) {
	ep, ok := h.endpoint(w, r)
	if !ok {
		return
	}
	var in struct {
		Path *string `json:"path"`
	}
	if !decode(w, r, &in, false) {
		return
	}
	if in.Path == nil {
		WriteError(w, http.StatusUnprocessableEntity, "invalid", "give the new path", "path")
		return
	}
	p := strings.TrimSpace(*in.Path)
	if msg := catch.CheckPath(h.o.Reserved, p); msg != "" {
		WriteError(w, http.StatusUnprocessableEntity, "invalid", msg, "path")
		return
	}
	if p != ep.Path {
		err := h.store.SetPath(r.Context(), ep.ID, p)
		if errors.Is(err, store.ErrDuplicatePath) {
			WriteError(w, http.StatusConflict, "duplicate_path", p+" is already an endpoint.", "path")
			return
		}
		if err != nil {
			h.fail(w, "change path", err)
			return
		}
		h.log.Info("endpoint moved", logAttrs(r, "from", ep.Path, "to", p, "via", "api")...)
	}
	h.writeEndpoint(w, r, ep.ID)
}

func (h *Handler) writeEndpoint(w http.ResponseWriter, r *http.Request, id int64) {
	ep, err := h.store.Endpoint(r.Context(), id)
	if err != nil {
		h.fail(w, "endpoint", err)
		return
	}
	writeJSON(w, http.StatusOK, h.endpointView(r, ep))
}

// deleteEndpoint removes an endpoint and its requests.
func (h *Handler) deleteEndpoint(w http.ResponseWriter, r *http.Request) {
	ep, ok := h.endpoint(w, r)
	if !ok {
		return
	}
	if err := h.store.DeleteEndpoint(r.Context(), ep.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		h.fail(w, "delete endpoint", err)
		return
	}
	h.log.Info("endpoint deleted", logAttrs(r, "path", ep.Path, "via", "api")...)
	w.WriteHeader(http.StatusNoContent)
}

// clearRequests removes every request an endpoint keeps; numbering goes on.
func (h *Handler) clearRequests(w http.ResponseWriter, r *http.Request) {
	ep, ok := h.endpoint(w, r)
	if !ok {
		return
	}
	if err := h.store.ClearRequests(r.Context(), ep.ID); err != nil {
		h.fail(w, "clear", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// resetCounter starts the rules' count again ("every 3rd" counts from here).
func (h *Handler) resetCounter(w http.ResponseWriter, r *http.Request) {
	ep, ok := h.endpoint(w, r)
	if !ok || !decode(w, r, &struct{}{}, true) {
		return
	}
	if err := h.store.ResetCounter(r.Context(), ep.ID); err != nil {
		h.fail(w, "reset counter", err)
		return
	}
	h.writeEndpoint(w, r, ep.ID)
}

// recheck verifies the kept requests again with the current checks,
// secrets and schemas, as the UI's Re-check does.
func (h *Handler) recheck(w http.ResponseWriter, r *http.Request) {
	ep, ok := h.endpoint(w, r)
	if !ok || !decode(w, r, &struct{}{}, true) {
		return
	}
	n, err := catch.Recheck(r.Context(), h.store, ep, h.o.Keyring)
	if err != nil {
		h.fail(w, "re-check", err)
		return
	}
	h.live.Publish(ep.ID)
	h.log.Info("verdicts re-checked", logAttrs(r, "path", ep.Path, "requests", n, "via", "api")...)
	writeJSON(w, http.StatusOK, map[string]int{"rechecked": n})
}

// rawBody returns a request's body exactly as received, with the
// Content-Type the sender gave it, but always as a download in a sandbox:
// whatever a sender sent must never run as a page of this origin.
func (h *Handler) rawBody(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		WriteError(w, http.StatusNotFound, "not_found", "no request "+r.PathValue("id"), "")
		return
	}
	req, err := h.store.Request(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no request %d", id), "")
		return
	}
	if err != nil {
		h.fail(w, "body", err)
		return
	}
	if req.TooLarge {
		WriteError(w, http.StatusNotFound, "not_kept", "the body was over the limit and was not kept", "")
		return
	}
	ct := "application/octet-stream"
	for _, hd := range req.Headers {
		if strings.EqualFold(hd.Name, "Content-Type") {
			if _, _, err := mime.ParseMediaType(hd.Value); err == nil {
				ct = hd.Value
			}
			break
		}
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="request-%d.body"`, req.ID))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("Content-Length", strconv.Itoa(len(req.Body)))
	w.Write(req.Body)
}

// report writes a register's report (an endpoint's, or with unmatched the
// Unmatched register's) with the UI dialog's parameters.
func (h *Handler) report(w http.ResponseWriter, r *http.Request, reg int64, path string) {
	rq, err := report.ParseQuery(r.URL.Query())
	var fe *report.FieldError
	if errors.As(err, &fe) {
		WriteError(w, http.StatusUnprocessableEntity, "invalid", fe.Message, fe.Field)
		return
	}
	now := time.Now().UTC()
	o := report.Options{
		Endpoint: path, Selection: rq.Words, From: rq.Selection.From, To: rq.Selection.To,
		Mask: rq.Mask, Version: h.o.Version, Generated: now, NoImages: rq.NoImages, Fonts: h.o.Fonts,
	}
	src := func(ctx context.Context, withBody bool, through int64, fn func(store.Request) error) error {
		s := rq.Selection
		s.Through = through
		return h.store.EachSelected(ctx, reg, s, withBody, fn)
	}
	out := report.NewLazy(w, func() {
		w.Header().Set("Content-Type", rq.Format.ContentType())
		w.Header().Set("Content-Disposition", `attachment; filename="`+report.FileName(path, now, rq.Format)+`"`)
		w.Header().Set("X-Content-Type-Options", "nosniff")
	})
	if err := report.Write(r.Context(), out, rq.Format, o, src); err != nil {
		h.log.Error("report", "endpoint", path, "err", err)
		if !out.Written() { // nothing sent yet: an error, not an empty file
			WriteError(w, http.StatusInternalServerError, "internal", "the report could not be made", "")
		}
		// otherwise the status is sent; the download ends short
	}
}

func (h *Handler) endpointReport(w http.ResponseWriter, r *http.Request) {
	ep, ok := h.endpoint(w, r)
	if !ok {
		return
	}
	h.report(w, r, ep.ID, ep.Path)
}

func (h *Handler) unmatchedReport(w http.ResponseWriter, r *http.Request) {
	h.report(w, r, store.Unmatched, "")
}
