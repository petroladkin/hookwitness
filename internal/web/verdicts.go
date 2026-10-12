package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/petroladkin/hookwitness/internal/auth"
	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/store"
)

// verdictsView is an endpoint's authorization verdicts at a glance, on its
// register page.
type verdictsView struct {
	On        bool // the endpoint has authorization checks
	Pass      int
	Fail      int
	NV        int
	Rechecked string // "12" after a re-check, for the notice
}

// parseVerdicts reads a request's stored verdicts; nil when there are none.
func parseVerdicts(s string) []auth.Verdict {
	if s == "" {
		return nil
	}
	var vs []auth.Verdict
	if json.Unmarshal([]byte(s), &vs) != nil {
		return nil
	}
	return vs
}

func (h *Handler) verdictsView(r *http.Request, ep store.Endpoint) (verdictsView, error) {
	cfg, err := emulate.Parse(ep.Config)
	if err != nil {
		return verdictsView{}, err
	}
	v := verdictsView{Rechecked: r.URL.Query().Get("rechecked")}
	for _, ch := range cfg.Checks {
		v.On = v.On || ch.GivesVerdict()
	}
	counts, err := h.store.VerdictCounts(r.Context(), ep.ID)
	if err != nil {
		return verdictsView{}, err
	}
	v.Pass, v.Fail, v.NV = counts["pass"], counts["fail"], counts["n/v"]
	return v, nil
}

// recheck verifies an endpoint's kept requests again with its current
// checks and secrets. Only the verdicts change: the status and reply stay
// what the sender saw.
func (h *Handler) recheck(w http.ResponseWriter, r *http.Request) {
	reg, ok := regID(r)
	if !ok || reg == store.Unmatched {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	ep, err := h.store.Endpoint(ctx, reg)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, "re-check", err)
		return
	}
	cfg, err := emulate.Parse(ep.Config)
	if err != nil {
		h.fail(w, "re-check", err)
		return
	}
	n := 0
	err = h.store.EachRequest(ctx, reg, func(req store.Request) error {
		er := emulate.Request{Method: req.Method, Path: req.Path, RawQuery: req.RawQuery, Body: req.Body}
		for _, hd := range req.Headers {
			er.Headers = append(er.Headers, emulate.Header{Name: hd.Name, Value: hd.Value})
		}
		vs, _ := cfg.Verify(er, req.ReceivedAt, !req.TooLarge, h.keys)
		n++
		return h.store.SetVerdicts(ctx, req.ID, emulate.Overall(vs), emulate.VerdictsJSON(vs))
	})
	if err != nil {
		h.fail(w, "re-check", err)
		return
	}
	h.log.Info("verdicts re-checked", "path", ep.Path, "requests", n)
	h.live.Publish(reg)
	http.Redirect(w, r, h.regPath(reg)+"?rechecked="+strconv.Itoa(n), http.StatusSeeOther)
}

// resultLabel is how a verdict's result reads on its stamp.
func resultLabel(r auth.Result) string {
	if r == auth.NotVerifiable {
		return "not verifiable"
	}
	return string(r)
}

// verdictClass is a verdict as a CSS class part: pass, fail or nv.
func verdictClass(v string) string {
	if v == string(auth.NotVerifiable) {
		return "nv"
	}
	return v
}
