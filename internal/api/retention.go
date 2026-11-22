package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/petroladkin/hookwitness/internal/store"
)

// laneJSON is one lane's effective limits.
type laneJSON struct {
	Count  int     `json:"count"`
	MaxAge *string `json:"max_age"` // null: no age limit
}

// effectiveJSON is a register's policy resolved against the server's
// defaults; anomalies is null for a register without an anomaly lane.
type effectiveJSON struct {
	Normal    laneJSON  `json:"normal"`
	Anomalies *laneJSON `json:"anomalies"`
}

type keptJSON struct {
	Normal    int `json:"normal"`
	Anomalies int `json:"anomalies"`
	Pinned    int `json:"pinned"`
}

type prunedJSON struct {
	Count          int64  `json:"count"`
	NormalUntil    *stamp `json:"normal_until"`    // the newest normal request pruned; null: none
	AnomaliesUntil *stamp `json:"anomalies_until"` // the newest anomaly pruned; null: none
}

// retentionView is what every register says about what it keeps.
type retentionView struct {
	Retention          *store.Retention `json:"retention"` // its own policy; null: the server's defaults
	RetentionEffective effectiveJSON    `json:"retention_effective"`
	Kept               keptJSON         `json:"kept"`
	Pruned             prunedJSON       `json:"pruned"`
}

func age(d time.Duration) *string {
	if d == 0 {
		return nil
	}
	s := store.FormatAge(d)
	return &s
}

func optStamp(t time.Time) *stamp {
	if t.IsZero() {
		return nil
	}
	s := stamp(t)
	return &s
}

// effectiveOf resolves a register's policy as the store does.
func effectiveOf(p store.Retention, reg int64, d store.Keep) store.Effective {
	e := p.Resolve(d)
	if reg == store.Unmatched {
		e.Anomalies, e.AnomalyAge = 0, 0
	}
	return e
}

func (h *Handler) retentionOf(reg int64, k store.Kept) retentionView {
	e := effectiveOf(k.Policy, reg, h.o.Keep)
	v := retentionView{
		RetentionEffective: effectiveJSON{Normal: laneJSON{Count: e.Normal, MaxAge: age(e.NormalAge)}},
		Kept:               keptJSON{Normal: k.Normal, Anomalies: k.Anomalies, Pinned: k.Pinned},
		Pruned:             prunedJSON{Count: k.Pruned, NormalUntil: optStamp(k.NormalAt), AnomaliesUntil: optStamp(k.AnomalyAt)},
	}
	if !k.Policy.IsZero() {
		p := k.Policy
		v.Retention = &p
	}
	if e.Anomalies > 0 {
		v.RetentionEffective.Anomalies = &laneJSON{Count: e.Anomalies, MaxAge: age(e.AnomalyAge)}
	}
	return v
}

// parseRetention reads a "retention" member: an object, or null for the
// server's defaults.
func parseRetention(raw json.RawMessage) (store.Retention, error) {
	var p store.Retention
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return p, nil
	}
	err := json.Unmarshal(raw, &p)
	return p, err
}

// applyRetention stores a register's new policy and prunes down to it at
// once; it answers how many requests the change removed.
func (h *Handler) applyRetention(ctx context.Context, reg int64, p store.Retention) (int, error) {
	if err := h.store.SetRetention(ctx, reg, p); err != nil {
		return 0, err
	}
	n, err := h.store.PruneNow(context.WithoutCancel(ctx), reg, time.Now(), h.o.Keep)
	if n > 0 {
		h.live.Publish(reg)
	}
	return n, err
}

// patchRetention is the retention part of PATCH /endpoints/{id} and
// /unmatched: it answers the requests removed, or writes the error.
func (h *Handler) patchRetention(w http.ResponseWriter, r *http.Request, reg int64, raw json.RawMessage) (int, bool) {
	p, err := parseRetention(raw)
	if err == nil && reg == store.Unmatched && (p.AnomaliesSet || p.Anomalies.MaxAge > 0) {
		err = errors.New("the Unmatched register has no anomaly lane: give normal only")
	}
	if err != nil {
		WriteError(w, http.StatusUnprocessableEntity, "invalid", err.Error(), "retention")
		return 0, false
	}
	n, err := h.applyRetention(r.Context(), reg, p)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no endpoint %d", reg), "")
		return 0, false
	}
	if err != nil {
		h.fail(w, "set retention", err)
		return 0, false
	}
	h.log.Info("retention set", logAttrs(r, "register", reg, "policy", p.String(), "pruned", n, "via", "api")...)
	return n, true
}

// unmatchedJSON is the Unmatched register's summary and settings.
type unmatchedJSON struct {
	Requests int    `json:"requests"`
	Last     *stamp `json:"last_request_at,omitempty"`
	retentionView
	ToPrune *int `json:"to_prune,omitempty"` // in a PATCH answer: requests the change removed
}

func (h *Handler) unmatchedView(ctx context.Context) (unmatchedJSON, error) {
	ep, err := h.store.Endpoint(ctx, store.Unmatched)
	if err != nil {
		return unmatchedJSON{}, err
	}
	return unmatchedJSON{Requests: ep.Count, Last: optStamp(ep.LastAt), retentionView: h.retentionOf(store.Unmatched, ep.Kept)}, nil
}

func (h *Handler) getUnmatched(w http.ResponseWriter, r *http.Request) {
	v, err := h.unmatchedView(r.Context())
	if err != nil {
		h.fail(w, "unmatched", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *Handler) patchUnmatched(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Retention json.RawMessage `json:"retention"`
	}
	if !decode(w, r, &in, false) {
		return
	}
	if in.Retention == nil {
		WriteError(w, http.StatusUnprocessableEntity, "invalid", "give retention (an object, or null for the server's defaults)", "retention")
		return
	}
	n, ok := h.patchRetention(w, r, store.Unmatched, in.Retention)
	if !ok {
		return
	}
	v, err := h.unmatchedView(r.Context())
	if err != nil {
		h.fail(w, "unmatched", err)
		return
	}
	v.ToPrune = &n
	writeJSON(w, http.StatusOK, v)
}

// pin pins (PUT) or unpins (DELETE) a request: a pinned request is never
// pruned by retention.
func (h *Handler) pin(on bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(r, "id")
		if !ok {
			WriteError(w, http.StatusNotFound, "not_found", "no request "+r.PathValue("id"), "")
			return
		}
		req, err := h.store.RequestHead(r.Context(), id)
		if err == nil {
			err = h.store.SetPinned(r.Context(), id, on)
		}
		if errors.Is(err, store.ErrNotFound) {
			WriteError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no request %d: it was never caught, or was cleared or pruned", id), "")
			return
		}
		if err != nil {
			h.fail(w, "pin", err)
			return
		}
		h.live.Publish(req.EndpointID)
		w.WriteHeader(http.StatusNoContent)
	}
}
