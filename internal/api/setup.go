package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/setup"
	"github.com/petroladkin/hookwitness/internal/store"
)

func (h *Handler) setupOptions() setup.Options {
	return setup.Options{Reserved: h.o.Reserved, MaxWait: h.o.MaxWait}
}

// exportSetup answers the setup file of the endpoints named by id (all
// without one).
func (h *Handler) exportSetup(w http.ResponseWriter, r *http.Request) {
	ids := r.URL.Query()["id"]
	var eps []store.Endpoint
	if len(ids) == 0 {
		var err error
		if eps, err = h.store.Endpoints(r.Context()); err != nil {
			h.fail(w, "export setup", err)
			return
		}
	}
	for _, s := range ids {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, http.StatusBadRequest, "invalid", "id must be an endpoint id", "id")
			return
		}
		ep, err := h.store.Endpoint(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			WriteError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no endpoint %d", id), "id")
			return
		}
		if err != nil {
			h.fail(w, "export setup", err)
			return
		}
		eps = append(eps, ep)
	}
	f, name, err := ExportSetup(eps)
	if err != nil {
		h.fail(w, "export setup", err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Write(f.JSON())
}

// ExportSetup makes the setup file of endpoints and the name to save it
// under.
func ExportSetup(eps []store.Endpoint) (setup.File, string, error) {
	var src []setup.Source
	for _, ep := range eps {
		cfg, err := emulate.Parse(ep.Config)
		if err != nil {
			return setup.File{}, "", err
		}
		src = append(src, setup.Source{Path: ep.Path, Config: cfg})
	}
	name := "endpoints.hookwitness.json"
	if len(eps) == 1 {
		name = setup.FileName(eps[0].Path)
	}
	return setup.Export(src...), name, nil
}

// importSetup creates (or with on_conflict=replace also replaces) the
// endpoints of a setup file, all of them or none.
func (h *Handler) importSetup(w http.ResponseWriter, r *http.Request) {
	var replace bool
	switch c := r.URL.Query().Get("on_conflict"); c {
	case "", "fail":
	case "replace":
		replace = true
	default:
		WriteError(w, http.StatusBadRequest, "invalid", `on_conflict is "fail" or "replace"`, "on_conflict")
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, setup.MaxSize))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		WriteError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("the setup file is over %d bytes", setup.MaxSize), "")
		return
	}
	if err != nil {
		WriteError(w, http.StatusBadRequest, "bad_body", "the body could not be read", "")
		return
	}
	plan := setup.Plan{Replace: setup.Never}
	if replace {
		plan.Replace = setup.Always
	}
	res, status, err := h.ApplySetup(r, data, plan)
	if err != nil {
		code := "invalid_setup"
		switch status {
		case http.StatusConflict:
			code = "duplicate_path"
		case http.StatusInternalServerError:
			h.fail(w, "import setup", err)
			return
		}
		WriteError(w, status, code, err.Error(), "")
		return
	}
	writeJSON(w, status, map[string]any{"endpoints": res})
}

// ApplySetup parses and applies a setup file for a caller (the API and the
// UI), logs what changed, and gives the status to answer: 201 when it
// created an endpoint, 200 otherwise; on an error 422 (the file), 409 (a
// taken path) or 500.
func (h *Handler) ApplySetup(r *http.Request, data []byte, plan setup.Plan) ([]setup.Result, int, error) {
	f, err := setup.Parse(data, h.setupOptions())
	if err != nil {
		return nil, http.StatusUnprocessableEntity, err
	}
	return h.ApplyFile(r, f, plan)
}

// ApplyFile applies a parsed setup file, as ApplySetup.
func (h *Handler) ApplyFile(r *http.Request, f setup.File, plan setup.Plan) ([]setup.Result, int, error) {
	res, err := setup.Apply(r.Context(), h.store, f, plan, h.setupOptions())
	var inv *setup.InvalidError
	switch {
	case errors.Is(err, store.ErrDuplicatePath):
		path, _, _ := strings.Cut(err.Error(), ": ")
		return nil, http.StatusConflict, fmt.Errorf("%s is already an endpoint; nothing was imported. Replace its behaviour (on_conflict=replace; its requests stay) or change the path in the file", path)
	case errors.As(err, &inv):
		return nil, http.StatusUnprocessableEntity, err
	case err != nil:
		return nil, http.StatusInternalServerError, err
	}
	status := http.StatusOK
	for _, x := range res {
		if x.Result != setup.Unchanged {
			h.log.Info("endpoint "+x.Result+" from a setup file", logAttrs(r, "path", x.Path, "secrets_not_set", strings.Join(x.Unset, ","))...)
		}
		if x.Result == setup.Created {
			status = http.StatusCreated
		}
	}
	h.log.Info("endpoints imported", logAttrs(r, "paths", strings.Join(f.Paths(), ","))...)
	return res, status, nil
}
