package web

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/report"
	"github.com/petroladkin/hookwitness/internal/store"
	"github.com/petroladkin/hookwitness/internal/version"
)

// exportView is the report dialog.
type exportView struct {
	Action string // the report URL
	Close  string // the page without the dialog
	Count  int    // requests kept
	Format string // md, html, pdf
	Scope  string // all, range, picked
	From   string // datetime-local, UTC
	To     string
	Pick   string // "3, 5-9"
	Images string // shown, listed
	Masks  []maskOption
	Other  string // more header names to mask, comma-separated
	Error  string
	Field  string // the field the error is about
}

type maskOption struct {
	Name string
	On   bool
}

// exportDefaults is the dialog as first opened: HTML of the whole
// register, or of the request on show; nothing masked.
func (h *Handler) exportDefaults(reg int64, ep store.Endpoint, ex *exhibitView, closeURL string) *exportView {
	v := &exportView{Action: h.regPath(reg) + "/report", Close: closeURL, Count: ep.Count, Format: "html", Scope: "all", Images: "shown"}
	if ex != nil {
		v.Scope, v.Pick = "picked", strconv.FormatInt(ex.No, 10)
	}
	for _, n := range maskPreset(ep) {
		v.Masks = append(v.Masks, maskOption{Name: n})
	}
	return v
}

// maskPreset is what the dialog offers to mask: the usual credential
// headers and the ones the endpoint's checks verify.
func maskPreset(ep store.Endpoint) []string {
	names := []string{"Authorization", "Cookie", "Set-Cookie"}
	if cfg, err := emulate.Parse(ep.Config); err == nil {
		for _, n := range cfg.CredentialHeaders() {
			if !slices.ContainsFunc(names, func(o string) bool { return strings.EqualFold(o, n) }) {
				names = append(names, n)
			}
		}
	}
	return names
}

// exportError is a refused export: which field and why.
type exportError struct{ field, msg string }

func (e exportError) Error() string { return e.msg }

// readExport reads the dialog's query into the view and the report's
// request; a bad value is an exportError.
func (h *Handler) readExport(q url.Values, v *exportView) (report.Query, error) {
	v.Format, v.Scope = q.Get("format"), q.Get("scope")
	v.From, v.To, v.Pick, v.Other = q.Get("from"), q.Get("to"), q.Get("pick"), q.Get("mask_other")
	if v.Images = q.Get("images"); v.Images != "listed" {
		v.Images = "shown"
	}
	for i := range v.Masks {
		v.Masks[i].On = slices.Contains(q["mask"], v.Masks[i].Name)
	}
	rq, err := report.ParseQuery(q)
	var fe *report.FieldError
	if errors.As(err, &fe) {
		return rq, exportError{fe.Field, fe.Message}
	}
	return rq, err
}

// report writes a register's report as a download, or shows the dialog
// again with the reason when a field is wrong.
func (h *Handler) report(w http.ResponseWriter, r *http.Request) {
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
		h.fail(w, "report", err)
		return
	}
	v := h.exportDefaults(reg, ep, nil, h.regPath(reg))
	rq, err := h.readExport(r.URL.Query(), v)
	var bad exportError
	if errors.As(err, &bad) {
		v.Error, v.Field = bad.msg, bad.field
		h.showRegister(w, r, reg, ep, nil, v, nil, http.StatusUnprocessableEntity)
		return
	}
	format, sel := rq.Format, rq.Selection
	now := time.Now().UTC()
	o := report.Options{
		Endpoint:  ep.Path,
		Selection: rq.Words,
		From:      sel.From,
		To:        sel.To,
		Mask:      rq.Mask,
		Version:   version.String(),
		Generated: now,
		NoImages:  rq.NoImages,
	}
	if format == report.HTML {
		o.Fonts, _ = fs.Sub(staticFS, "static")
	}
	src := func(ctx context.Context, withBody bool, through int64, fn func(store.Request) error) error {
		s := sel
		s.Through = through
		return h.store.EachSelected(ctx, reg, s, withBody, fn)
	}
	w.Header().Set("Content-Type", format.ContentType())
	w.Header().Set("Content-Disposition", `attachment; filename="`+report.FileName(ep.Path, now, format)+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := report.Write(ctx, w, format, o, src); err != nil {
		// The status is sent; the download ends short and the log says why.
		h.log.Error("report", "endpoint", ep.Path, "err", err)
	}
}
