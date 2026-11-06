package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/petroladkin/hookwitness/internal/store"
)

// keepsView is the register header's line about retention: what it keeps
// and what it lost.
type keepsView struct {
	Normal, Anomalies     int    // counts in force; Anomalies 0: one lane
	NormalAge, AnomalyAge string // "" without an age limit
	Pinned                int
	Pruned                int64
	Own                   bool // the register has its own policy
}

func (h *Handler) keepsOf(reg int64, k store.Kept) keepsView {
	e := k.Policy.Resolve(h.keep)
	if reg == store.Unmatched {
		e.Anomalies, e.AnomalyAge = 0, 0
	}
	v := keepsView{Normal: e.Normal, Anomalies: e.Anomalies, Pinned: k.Pinned, Pruned: k.Pruned, Own: !k.Policy.IsZero()}
	if e.NormalAge > 0 {
		v.NormalAge = store.FormatAge(e.NormalAge)
	}
	if e.AnomalyAge > 0 {
		v.AnomalyAge = store.FormatAge(e.AnomalyAge)
	}
	return v
}

// retentionDialogView is the Retention dialog: the register's own limits
// (empty fields take the server's defaults, shown as placeholders), and,
// before a save that would delete requests, how many.
type retentionDialogView struct {
	Action, Close                 string
	Unmatched                     bool
	NormalCount, NormalAge        string
	AnomalyCount, AnomalyAge      string
	DefaultNormal, DefaultAnomaly int
	Error, Field                  string
	ToPrune                       int // > 0: the save asks again, naming it
}

func (h *Handler) retentionDefaults(reg int64, k store.Kept, base string) *retentionDialogView {
	v := &retentionDialogView{Action: base + "/retention", Close: base, Unmatched: reg == store.Unmatched,
		DefaultNormal: h.keep.Normal, DefaultAnomaly: h.keep.Anomalies}
	p := k.Policy
	if p.Normal.Count > 0 {
		v.NormalCount = strconv.Itoa(p.Normal.Count)
	}
	if p.Normal.MaxAge > 0 {
		v.NormalAge = store.FormatAge(p.Normal.MaxAge)
	}
	if p.AnomaliesSet {
		v.AnomalyCount = strconv.Itoa(p.Anomalies.Count)
	}
	if p.Anomalies.MaxAge > 0 {
		v.AnomalyAge = store.FormatAge(p.Anomalies.MaxAge)
	}
	return v
}

// policyFromForm reads the dialog's fields; an empty field takes the
// default. It answers the field at fault.
func policyFromForm(r *http.Request, unmatched bool) (store.Retention, string, error) {
	var p store.Retention
	count := func(name string, min int) (int, bool, error) {
		s := strings.TrimSpace(r.PostFormValue(name))
		if s == "" {
			return 0, false, nil
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < min || n > store.MaxKeep {
			return 0, false, errors.New("a whole number from " + strconv.Itoa(min) + " to 1000000")
		}
		return n, true, nil
	}
	age := func(name string) (time.Duration, error) {
		s := strings.TrimSpace(r.PostFormValue(name))
		if s == "" {
			return 0, nil
		}
		return store.ParseAge(s)
	}
	var err error
	if p.Normal.Count, _, err = count("normal_count", 1); err != nil {
		return p, "normal_count", errors.New("Normal requests: " + err.Error())
	}
	if p.Normal.MaxAge, err = age("normal_age"); err != nil {
		return p, "normal_age", errors.New("Normal requests, for at most: " + err.Error())
	}
	if unmatched {
		return p, "", nil
	}
	if p.Anomalies.Count, p.AnomaliesSet, err = count("anomaly_count", 0); err != nil {
		return p, "anomaly_count", errors.New("Anomalies: " + err.Error())
	}
	if p.Anomalies.MaxAge, err = age("anomaly_age"); err != nil {
		return p, "anomaly_age", errors.New("Anomalies, for at most: " + err.Error())
	}
	return p, "", nil
}

// saveRetention sets a register's retention from the dialog. A change that
// would delete requests is shown first with the count; the second post
// (confirm) saves it and prunes at once.
func (h *Handler) saveRetention(w http.ResponseWriter, r *http.Request) {
	reg, ok := regID(r)
	if !ok {
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
		h.fail(w, "retention", err)
		return
	}
	base := h.regPath(reg)
	v := h.retentionDefaults(reg, ep.Kept, base)
	v.NormalCount, v.NormalAge = r.PostFormValue("normal_count"), r.PostFormValue("normal_age")
	v.AnomalyCount, v.AnomalyAge = r.PostFormValue("anomaly_count"), r.PostFormValue("anomaly_age")
	var p store.Retention
	if r.PostFormValue("op") == "defaults" {
		v.NormalCount, v.NormalAge, v.AnomalyCount, v.AnomalyAge = "", "", "", ""
	} else if p, v.Field, err = policyFromForm(r, reg == store.Unmatched); err != nil {
		v.Error = err.Error()
		h.showRetention(w, r, reg, ep, v, http.StatusUnprocessableEntity)
		return
	}
	n, err := h.store.ToPrune(ctx, reg, p, time.Now(), h.keep)
	if err != nil {
		h.fail(w, "retention", err)
		return
	}
	if n > 0 && r.PostFormValue("confirm") != strconv.Itoa(n) {
		v.ToPrune = n
		h.showRetention(w, r, reg, ep, v, http.StatusOK)
		return
	}
	if err := h.store.SetRetention(ctx, reg, p); err != nil {
		h.fail(w, "retention", err)
		return
	}
	pruned, err := h.store.PruneNow(ctx, reg, time.Now(), h.keep)
	if err != nil {
		h.fail(w, "retention", err)
		return
	}
	if pruned > 0 {
		h.live.Publish(reg)
	}
	h.log.Info("retention set", "register", reg, "policy", p.String(), "pruned", pruned, "via", "ui")
	http.Redirect(w, r, base, http.StatusSeeOther)
}

func (h *Handler) showRetention(w http.ResponseWriter, r *http.Request, reg int64, ep store.Endpoint, v *retentionDialogView, status int) {
	h.showRegister(w, r, reg, ep, nil, nil, nil, nil, status, v)
}

// pin pins or unpins a request from its page.
func (h *Handler) pin(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	req, err := h.store.RequestHead(r.Context(), id)
	if err == nil {
		err = h.store.SetPinned(r.Context(), id, r.PostFormValue("pinned") == "true")
	}
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, "pin", err)
		return
	}
	http.Redirect(w, r, h.back(r, h.regPath(req.EndpointID)+"/r/"+strconv.FormatInt(id, 10)), http.StatusSeeOther)
}

// hatches are the strip's two hatched spans from a register's watermarks:
// before keptFrom nothing is kept (but pins); from there to thinnedUntil
// only one lane is (thinned names it).
func hatches(k store.Kept) (keptFrom, thinnedUntil time.Time, thinned string) {
	n, a := k.NormalAt, k.AnomalyAt
	switch {
	case n.IsZero() && a.IsZero():
		return
	case n.IsZero():
		return time.Time{}, a, "normal"
	case a.IsZero():
		return time.Time{}, n, "anomalies"
	case n.Equal(a):
		return n, time.Time{}, ""
	case n.After(a):
		return a, n, "anomalies"
	default:
		return n, a, "normal"
	}
}

// thousands writes n with thin spaces between groups of three: 41 208.
func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + " " + s[i:]
	}
	return s
}

// clearQuestion is the confirmation of a clear; it names the pinned
// requests it removes too.
func clearQuestion(what string, pinned int) string {
	q := "Clear every request kept for " + what + "?"
	if pinned > 0 {
		q = "Clear every request kept for " + what + ", the " + strconv.Itoa(pinned) + " pinned " + pick(pinned == 1, "one", "ones") + " too?"
	}
	return q
}
