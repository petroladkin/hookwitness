package web

import (
	"context"
	"errors"
	"fmt"
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

// maxPicked bounds a hand-picked selection.
const maxPicked = 1000

// localLayout is the value of an <input type="datetime-local"> with seconds
// and milliseconds; the dialog reads and writes it as UTC.
const localLayout = "2006-01-02T15:04:05.000"

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
	v := &exportView{Action: h.regPath(reg) + "/report", Close: closeURL, Count: ep.Count, Format: "html", Scope: "all"}
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
// selection; a bad value is an exportError.
func (h *Handler) readExport(q url.Values, v *exportView) (store.Selection, []string, error) {
	var sel store.Selection
	v.Format, v.Scope = q.Get("format"), q.Get("scope")
	v.From, v.To, v.Pick, v.Other = q.Get("from"), q.Get("to"), q.Get("pick"), q.Get("mask_other")
	masked := q["mask"]
	for i := range v.Masks {
		v.Masks[i].On = slices.Contains(masked, v.Masks[i].Name)
	}
	if _, err := report.ParseFormat(v.Format); err != nil {
		return sel, nil, exportError{"format", "choose Markdown, HTML or PDF"}
	}
	switch v.Scope {
	case "all":
	case "range":
		var err error
		if sel.From, err = parseLocal(v.From); err != nil {
			return sel, nil, exportError{"from", "the start " + err.Error()}
		}
		if sel.To, err = parseLocal(v.To); err != nil {
			return sel, nil, exportError{"to", "the end " + err.Error()}
		}
		if sel.From.IsZero() && sel.To.IsZero() {
			return sel, nil, exportError{"from", "give the start, the end or both"}
		}
		if !sel.From.IsZero() && !sel.To.IsZero() && sel.To.Before(sel.From) {
			return sel, nil, exportError{"to", "the end is before the start"}
		}
	case "picked":
		nos, err := parseNumbers(v.Pick)
		if err != nil {
			return sel, nil, exportError{"pick", err.Error()}
		}
		sel.Nos = nos
	default:
		return sel, nil, exportError{"scope", "choose what the report covers"}
	}
	var names []string
	for _, n := range masked {
		if headerName(n) {
			names = append(names, n)
		}
	}
	for n := range strings.SplitSeq(v.Other, ",") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if !headerName(n) {
			return sel, nil, exportError{"mask_other", fmt.Sprintf("%q is not a header name", n)}
		}
		names = append(names, n)
	}
	return sel, names, nil
}

// headerName reports a valid HTTP header name.
func headerName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range []byte(s) {
		if c <= ' ' || c >= 0x7f || strings.IndexByte(`"(),/:;<=>?@[\]{}`, c) >= 0 {
			return false
		}
	}
	return true
}

// parseLocal reads a datetime-local value as UTC; empty is zero.
func parseLocal(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{localLayout, "2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("is not a date and time")
}

// parseNumbers reads "3, 5-9" into request numbers, in order, each once.
func parseNumbers(s string) ([]int64, error) {
	var out []int64
	seen := map[int64]bool{}
	add := func(n int64) error {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
		if len(out) > maxPicked {
			return fmt.Errorf("at most %d requests can be picked; use a time range", maxPicked)
		}
		return nil
	}
	for part := range strings.SplitSeq(strings.ReplaceAll(s, " ", ","), ",") {
		if part == "" {
			continue
		}
		a, b, isRange := strings.Cut(strings.TrimPrefix(part, "#"), "-")
		lo, err1 := strconv.ParseInt(a, 10, 64)
		hi, err2 := lo, error(nil)
		if isRange {
			hi, err2 = strconv.ParseInt(strings.TrimPrefix(b, "#"), 10, 64)
		}
		if err1 != nil || err2 != nil || lo < 1 || hi < lo {
			return nil, fmt.Errorf("%q is not a request number or a range like 5-9", part)
		}
		if hi-lo >= maxPicked {
			return nil, fmt.Errorf("at most %d requests can be picked; use a time range", maxPicked)
		}
		for n := lo; n <= hi; n++ {
			if err := add(n); err != nil {
				return nil, err
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("give the request numbers, e.g. 3, 5-9")
	}
	slices.Sort(out)
	return out, nil
}

// selectionWords is how the report names its selection.
func selectionWords(v *exportView, sel store.Selection) string {
	switch v.Scope {
	case "range":
		from, to := "the first request", "the last"
		if !sel.From.IsZero() {
			from = sel.From.Format("2006-01-02T15:04:05.000Z")
		}
		if !sel.To.IsZero() {
			to = sel.To.Format("2006-01-02T15:04:05.000Z")
		}
		return "received from " + from + " to " + to
	case "picked":
		if len(sel.Nos) == 1 {
			return "request #" + strconv.FormatInt(sel.Nos[0], 10)
		}
		return "picked requests " + v.Pick
	}
	return "whole register"
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
	sel, mask, err := h.readExport(r.URL.Query(), v)
	var bad exportError
	if errors.As(err, &bad) {
		v.Error, v.Field = bad.msg, bad.field
		h.showRegister(w, r, reg, ep, nil, v, http.StatusUnprocessableEntity)
		return
	}
	format, _ := report.ParseFormat(v.Format)
	now := time.Now().UTC()
	o := report.Options{
		Endpoint:  ep.Path,
		Selection: selectionWords(v, sel),
		From:      sel.From,
		To:        sel.To,
		Mask:      mask,
		Version:   version.Version,
		Generated: now,
	}
	if format == report.HTML {
		o.Fonts, _ = fs.Sub(staticFS, "static")
	}
	src := func(ctx context.Context, withBody bool, fn func(store.Request) error) error {
		return h.store.EachSelected(ctx, reg, sel, withBody, fn)
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
