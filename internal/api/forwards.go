package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/petroladkin/hookwitness/internal/access"
	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/store"
)

// forwardJSON is a request's exchange with its endpoint's receiver: what was
// sent and what the receiver answered. status 0 and error mean there was no
// (complete) answer.
type forwardJSON struct {
	RequestID   int64       `json:"request_id"`
	CreatedAt   stamp       `json:"created_at"`
	URL         string      `json:"url"`
	Host        string      `json:"host"` // target or received
	InsecureTLS bool        `json:"insecure_tls"`
	TimeoutMS   int64       `json:"timeout_ms"`
	ByRule      int         `json:"by_rule"` // the rule that let the receiver get it too; 0: no rule took it
	Line        string      `json:"line"`    // "" when nothing was sent (the policy, no slot)
	Sent        [][2]string `json:"sent"`
	Notes       []string    `json:"notes"`
	Proto       string      `json:"proto"`
	Status      int         `json:"status"`
	Reason      string      `json:"reason"`
	Headers     [][2]string `json:"headers"`
	BodySize    int64       `json:"body_size"`              // -1 when the answer was cut before its end
	BodyCut     bool        `json:"body_cut"`               // only the first 64 KiB are kept
	BodyDropped bool        `json:"body_dropped,omitempty"` // dropped with the request's body (the size limit)
	DurationMS  int64       `json:"duration_ms"`
	TTFBMS      int64       `json:"ttfb_ms"`
	Error       *replayErr  `json:"error"`
	BodyText    *string     `json:"body_text,omitempty"`
	BodyBase64  *string     `json:"body_base64,omitempty"`
}

func forwardView(fw store.Forward) forwardJSON {
	v := forwardJSON{RequestID: fw.RequestID, CreatedAt: stamp(fw.CreatedAt), URL: fw.URL, Line: fw.Line, Sent: pairsOf(fw.Sent),
		Notes: fw.Notes, Proto: fw.Proto, Status: fw.Status, Reason: fw.Reason, Headers: pairsOf(fw.Headers), BodySize: fw.Size,
		BodyCut: fw.Truncated, BodyDropped: fw.BodyDropped, DurationMS: fw.Duration.Milliseconds(), TTFBMS: fw.TTFB.Milliseconds()}
	if v.Notes == nil {
		v.Notes = []string{}
	}
	var o struct {
		Host        string `json:"host"`
		InsecureTLS bool   `json:"insecure_tls"`
		TimeoutMS   int64  `json:"timeout_ms"`
		ByRule      int    `json:"by_rule"`
	}
	json.Unmarshal([]byte(fw.Options), &o)
	v.Host, v.InsecureTLS, v.TimeoutMS, v.ByRule = o.Host, o.InsecureTLS, o.TimeoutMS, o.ByRule
	if fw.ErrorKind != "" {
		v.Error = &replayErr{fw.ErrorKind, fw.Error}
	}
	switch {
	case fw.BodyDropped:
	case utf8.Valid(fw.Body):
		s := string(fw.Body)
		v.BodyText = &s
	default:
		s := base64.StdEncoding.EncodeToString(fw.Body)
		v.BodyBase64 = &s
	}
	return v
}

func (h *Handler) loadForward(w http.ResponseWriter, r *http.Request) (store.Forward, bool) {
	id, ok := pathID(r, "id")
	if !ok {
		WriteError(w, http.StatusNotFound, "not_found", "no request "+r.PathValue("id"), "")
		return store.Forward{}, false
	}
	fw, err := h.store.ForwardOf(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, http.StatusNotFound, "not_found", fmt.Sprintf("request %d has no exchange with a receiver: it was not forwarded, "+
			"its exchange has not ended yet, or it was cleared or pruned", id), "")
		return store.Forward{}, false
	}
	if err != nil {
		h.fail(w, "forward", err)
		return store.Forward{}, false
	}
	return fw, true
}

func (h *Handler) getForward(w http.ResponseWriter, r *http.Request) {
	if fw, ok := h.loadForward(w, r); ok {
		writeJSON(w, http.StatusOK, forwardView(fw))
	}
}

// forwardBody returns the receiver's answer body as it arrived (never
// decompressed; its first 64 KiB), with its Content-Type, always as a
// download in a sandbox.
func (h *Handler) forwardBody(w http.ResponseWriter, r *http.Request) {
	fw, ok := h.loadForward(w, r)
	if !ok {
		return
	}
	if fw.BodyDropped {
		WriteError(w, http.StatusNotFound, "not_kept", store.AnswerDropped, "")
		return
	}
	ct := "application/octet-stream"
	for _, hd := range fw.Headers {
		if strings.EqualFold(hd.Name, "Content-Type") {
			if _, _, err := mime.ParseMediaType(hd.Value); err == nil {
				ct = hd.Value
			}
			break
		}
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="forward-%d.body"`, fw.RequestID))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("Content-Length", strconv.Itoa(len(fw.Body)))
	w.Write(fw.Body)
}

// forwardChanged reports whether a behaviour change adds, changes or
// removes the receiver.
func forwardChanged(before, after *emulate.Forward) bool {
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	return string(a) != string(b)
}

// mayForward answers 403 when the caller may not change where requests are
// forwarded: once login is on, only an admin may.
func mayForward(w http.ResponseWriter, r *http.Request) bool {
	if id := access.From(r.Context()); id != nil && !id.Admin() {
		WriteError(w, http.StatusForbidden, "forbidden", ForwardAdminOnly, "forward")
		return false
	}
	return true
}

// ForwardAdminOnly is the refusal of a forward change by a user.
const ForwardAdminOnly = "only an admin can change where requests are forwarded (a receiver sends live traffic outward); " +
	"a PUT replaces the whole behaviour, so send the stored forward unchanged to edit the rest"

// OpenRelay is the warning for an endpoint that forwards without an
// enforced authorization check.
const OpenRelay = "anyone who reaches this endpoint's URL reaches its receiver, answers included: add an enforced authorization check"

// behaviourWarnings are what the behaviour's answer warns of.
func behaviourWarnings(c emulate.Config) []string {
	if c.Forward == nil {
		return nil
	}
	for _, ch := range c.Checks {
		if ch.Enforce && ch.GivesVerdict() && ch.Kind != emulate.KindSchema && !ch.IsDelivery() {
			return nil
		}
	}
	return []string{OpenRelay}
}

// checkTarget refuses, at save, a receiver the target policy would refuse:
// sending off, or a literal address that is never dialled.
func (h *Handler) checkTarget(w http.ResponseWriter, f *emulate.Forward) bool {
	if f == nil {
		return true
	}
	u, err := f.Target()
	if err == nil {
		err = h.replay.Policy().CheckURL(u)
	}
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "-replay-allow none") {
			msg = "forwarding is off on this server (-replay-allow none)"
		}
		WriteError(w, http.StatusUnprocessableEntity, "invalid", "receiver: "+msg, "forward")
		return false
	}
	return true
}
