package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/store"
)

// forwardAdminOnly refuses a change of the receiver by a user.
const forwardAdminOnly = "only an admin can change where requests are forwarded: a receiver sends live traffic outward"

// forwardChanged reports whether an edit adds, changes or removes the
// receiver.
func forwardChanged(before, after *emulate.Forward) bool {
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	return string(a) != string(b)
}

// checkTarget refuses, at save, a receiver the target policy would refuse.
func (h *Handler) checkTarget(f *emulate.Forward) error {
	if f == nil {
		return nil
	}
	u, err := f.Target()
	if err != nil {
		return err
	}
	if err := h.replay.Policy().CheckURL(u); err != nil {
		if strings.Contains(err.Error(), "-replay-allow none") {
			return errors.New("receiver: forwarding is off on this server (-replay-allow none)")
		}
		return errors.New("receiver: " + err.Error())
	}
	return nil
}

// formForward reads the receiver's dialog.
func formForward(r *http.Request) (emulate.Forward, error) {
	f := emulate.Forward{URL: strings.TrimSpace(r.PostFormValue("url")), KeepHost: r.PostFormValue("host") == "received",
		InsecureTLS: r.PostFormValue("insecure_tls") == "on", OnFailure: r.PostFormValue("on_failure")}
	if f.OnFailure == emulate.OnFailureGateway {
		f.OnFailure = "" // the default, stored as nothing
	}
	if s := strings.TrimSpace(r.PostFormValue("timeout")); s != "" {
		n, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return f, errors.New("receiver: the timeout is a number of seconds")
		}
		f.TimeoutMs = int64(n * 1000)
	}
	return f, nil
}

// openRelay reports a receiver without an enforced authorization check.
func openRelay(c emulate.Config) bool {
	if c.Forward == nil {
		return false
	}
	for _, ch := range c.Checks {
		if ch.Enforce && ch.GivesVerdict() && ch.Kind != emulate.KindSchema && !ch.IsDelivery() {
			return false
		}
	}
	return true
}

// timeoutField is the dialog's timeout in seconds, "" for the default.
func timeoutField(f emulate.Forward) string {
	if f.TimeoutMs == 0 {
		return ""
	}
	return strconv.FormatFloat(float64(f.TimeoutMs)/1000, 'f', -1, 64)
}

// outcomeOf is a request's outcome, a forward that can no longer be
// running (pending past -max-wait and a minute) shown as interrupted.
func (h *Handler) outcomeOf(req store.Request) emulate.Outcome {
	o := emulate.ParseOutcome(req.Outcome, req.Status)
	o.Interrupted(req.ReceivedAt, time.Now().Add(-h.maxWait-time.Minute))
	return o
}

// receiverOf is where an endpoint's requests are forwarded, "" when it
// answers them itself: its badge.
func receiverOf(config string) string {
	if !strings.Contains(config, `"forward"`) {
		return ""
	}
	c, err := emulate.Parse(config)
	if err != nil || c.Forward == nil {
		return ""
	}
	return c.Forward.Where()
}

// receiverAction is the receiver tab's button: "" for a viewer who may not
// change it.
func receiverAction(b behaviourView) string {
	switch {
	case !b.MayForward:
		return ""
	case b.Config.Forward != nil:
		return "Edit receiver"
	}
	return "Set receiver"
}

// defaultNote says when the default reply answers.
func defaultNote(c emulate.Config) string {
	switch {
	case c.Forward == nil:
		return "The answer when no check refuses the request and no rule matches."
	case c.Forward.FallsBackToDefault():
		return "Used when the receiver fails (see the Receiver tab): the sender gets this instead of a gateway error."
	}
	return "Not used while a receiver answers: requests no rule takes go to the receiver (see the Receiver tab)."
}

// secondsOf is a duration in seconds for a placeholder: the default timeout.
func secondsOf(maxWait time.Duration) string {
	return strconv.FormatFloat(min(emulate.DefaultForwardTimeout, maxWait).Seconds(), 'f', -1, 64)
}

// mayForwardView says whether the page's viewer may change the receiver.
func (h *Handler) mayForwardView(ctx context.Context) bool { return h.mayReplay(ctx) }

// exchangeView is a request's exchange with its endpoint's receiver, for
// the request page.
type exchangeView struct {
	URL        string
	Host       string // target or received
	Insecure   bool
	ByRule     int
	Line       string
	Sent       []store.Header
	Notes      []string
	Status     int
	Reason     string
	Headers    []store.Header
	Body       bodyView
	Size       int64
	Cut        bool
	Duration   time.Duration
	TTFB       time.Duration
	ErrorKind  string
	Error      string
	SenderLeft time.Duration // the sender left this long after the exchange began; 0: it stayed
	Download   string
}

// exchange loads a request's exchange, nil when it has none (yet).
func (h *Handler) exchange(ctx context.Context, req store.Request, o emulate.Outcome) (*exchangeView, error) {
	fw, err := h.store.ForwardOf(ctx, req.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var opt struct {
		Host        string `json:"host"`
		InsecureTLS bool   `json:"insecure_tls"`
		ByRule      int    `json:"by_rule"`
	}
	json.Unmarshal([]byte(fw.Options), &opt)
	v := &exchangeView{URL: fw.URL, Host: opt.Host, Insecure: opt.InsecureTLS, ByRule: opt.ByRule, Line: fw.Line, Sent: fw.Sent,
		Notes: fw.Notes, Status: fw.Status, Reason: fw.Reason, Headers: fw.Headers, Body: viewBody(fw.Body), Size: fw.Size,
		Cut: fw.Truncated, Duration: fw.Duration, TTFB: fw.TTFB, ErrorKind: fw.ErrorKind, Error: fw.Error,
		Download: h.prefix + "/r/" + strconv.FormatInt(req.ID, 10) + "/forward/body"}
	if o.Forward != nil {
		v.SenderLeft = time.Duration(o.Forward.SenderLeftMs) * time.Millisecond
	}
	return v, nil
}

// stamp is the receiver's answer in a word or two: "201 Created",
// "no answer: the receiver could not be reached".
func (v exchangeView) stamp() string {
	if v.ErrorKind != "" {
		return emulate.FailureWords(v.ErrorKind)
	}
	s := strconv.Itoa(v.Status)
	if v.Reason != "" {
		s += " " + v.Reason
	}
	return s
}

// hostWord says which Host line was sent.
func hostWord(h string) string {
	if h == "received" {
		return "as received"
	}
	return "of the receiver"
}

func (v exchangeView) good() bool { return v.ErrorKind == "" && v.Status >= 200 && v.Status < 300 }

// forwardBody downloads the receiver's answer body as it arrived.
func (h *Handler) forwardBody(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	fw, err := h.store.ForwardOf(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, "forward body", err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="forward-`+strconv.FormatInt(id, 10)+`.body"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(fw.Body)))
	w.Header().Set("Cache-Control", "no-store")
	w.Write(fw.Body)
}
