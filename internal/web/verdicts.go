package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/petroladkin/hookwitness/internal/auth"
	"github.com/petroladkin/hookwitness/internal/catch"
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
	NC        int    // not checked: no schema for the event
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
	v.Pass, v.Fail, v.NV, v.NC = counts["pass"], counts["fail"], counts["n/v"], counts["n/c"]
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
	var keys emulate.Keyring
	if h.keys != nil {
		keys = h.keys
	}
	n, err := catch.Recheck(ctx, h.store, ep, keys)
	if err != nil {
		h.fail(w, "re-check", err)
		return
	}
	h.log.Info("verdicts re-checked", append([]any{"path", ep.Path, "requests", n}, who(r)...)...)
	h.live.Publish(reg)
	http.Redirect(w, r, h.regPath(reg)+"?rechecked="+strconv.Itoa(n), http.StatusSeeOther)
}

// resultLabel is how a verdict's result reads on its stamp.
func resultLabel(r auth.Result) string {
	switch r {
	case auth.NotVerifiable:
		return "not verifiable"
	case auth.NotChecked:
		return "not checked"
	}
	return string(r)
}

// vstamp is a verdict's stamp: "GitHub signature: pass", "Schema: fail, 3
// errors".
func vstamp(v auth.Verdict) string {
	label := v.Label
	if strings.HasPrefix(label, "JSON Schema") {
		label = "Schema"
	}
	s := label + ": " + resultLabel(v.Result)
	switch {
	case v.Result == auth.Fail && v.Total == 1:
		s += ", 1 error"
	case v.Result == auth.Fail && v.Total > 1:
		s += ", " + strconv.Itoa(v.Total) + " errors"
	}
	return s
}

// problemPath shows a JSON pointer, "(body)" for the whole body.
func problemPath(p string) string {
	if p == "" {
		return "(body)"
	}
	return p
}

// badPaths are the JSON pointers where failed schema checks found problems.
func badPaths(vs []auth.Verdict) map[string]bool {
	out := map[string]bool{}
	for _, v := range vs {
		if v.Result != auth.Fail {
			continue
		}
		for _, p := range v.Problems {
			if p.Keyword != "json" {
				out[p.Path] = true
			}
		}
	}
	return out
}

// verdictClass is a verdict as a CSS class part: pass, fail or nv.
func verdictClass(v string) string {
	switch v {
	case string(auth.NotVerifiable):
		return "nv"
	case string(auth.NotChecked):
		return "nc"
	}
	return v
}
