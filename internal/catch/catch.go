// Package catch records the requests that senders deliver to endpoints.
package catch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/petroladkin/hookwitness/internal/access"
	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/live"
	"github.com/petroladkin/hookwitness/internal/store"
	"github.com/petroladkin/hookwitness/internal/wire"
)

// Handler catches requests to every path that is not reserved and answers
// them the way each endpoint is configured (internal/emulate).
type Handler struct {
	Store   *store.Store
	Live    *live.Broker
	Log     *slog.Logger
	MaxBody int64 // larger bodies are answered 413 and not kept
	// MaxInFlight bounds the bytes of request bodies held at once; a request
	// whose body does not fit is answered 503 and kept without it. Zero
	// means no bound.
	MaxInFlight int64
	Retention   int // requests kept per register
	// BodyTimeout bounds reading a request's body; a sender that trickles it
	// slower is cut off and nothing is kept. Zero means DefaultBodyTimeout.
	BodyTimeout time.Duration
	// MaxWait caps a timeout's hold (delays are capped when configured).
	// Zero means DefaultMaxWait.
	MaxWait time.Duration
	// Keys opens signing keys and matches tokens of authorization checks.
	Keys emulate.Keyring

	// after starts a wait's timer; tests replace it to see the durations
	// asked for without waiting. Nil means a real timer.
	after func(time.Duration) <-chan time.Time

	heldMu sync.Mutex
	held   int64 // bytes of bodies held now, against MaxInFlight

	stopOnce sync.Once
	stop     chan struct{}
	initOnce sync.Once
	inflight sync.WaitGroup
}

// DefaultBodyTimeout is enough for 10 MiB over a slow link.
const DefaultBodyTimeout = 60 * time.Second

// DefaultMaxWait is the longest delay or timeout hold.
const DefaultMaxWait = 5 * time.Minute

func (h *Handler) stopping() chan struct{} {
	h.initOnce.Do(func() { h.stop = make(chan struct{}) })
	return h.stop
}

// Stop ends pending delays and timeout holds; give it to server.Serve as an
// onShutdown function so a held request does not delay the shutdown.
func (h *Handler) Stop() { h.stopOnce.Do(func() { close(h.stopping()) }) }

// Wait returns when every request being answered has stored its outcome.
// http.Server.Shutdown does not wait for handlers whose connection was
// hijacked (timeouts, drops), so call Wait before closing the store.
func (h *Handler) Wait() { h.inflight.Wait() }

func (h *Handler) maxWait() time.Duration {
	if h.MaxWait > 0 {
		return h.MaxWait
	}
	return DefaultMaxWait
}

// Catch answers and records r. headers are its header lines as received
// (wire.Take); ok false falls back to the order net/http keeps.
func (h *Handler) Catch(w http.ResponseWriter, r *http.Request, headers []wire.Header, ok bool) {
	h.inflight.Add(1)
	defer h.inflight.Done()
	received := time.Now().UTC()
	p := RawPath(r)
	// net/http cancels the request context when the sender closes its side,
	// which fire-and-forget senders do right after the request; a request
	// that was received in full is still evidence and must be kept.
	ctx := context.WithoutCancel(r.Context())

	endpointID, cfgText, err := h.Store.EndpointByPath(ctx, p)
	switch {
	case errors.Is(err, store.ErrNotFound):
		endpointID = store.Unmatched
	case err != nil:
		h.Log.Error("catch: endpoint lookup failed", "path", p, "err", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	req := &store.Request{
		EndpointID: endpointID,
		ReceivedAt: received,
		Method:     r.Method,
		Path:       p,
		RawQuery:   rawQuery(r),
		Proto:      r.Proto,
		RemoteAddr: r.RemoteAddr,
		Headers:    storeHeaders(r, headers, ok),
	}
	read, reserved := h.readBody(w, r, req)
	// The body's bytes count against MaxInFlight until it is no longer
	// needed: once stored and, for a reply, rendered (carryOut).
	free := sync.OnceFunc(func() { h.release(reserved) })
	defer free()
	switch read {
	case bodyFailed:
		h.Log.Warn("catch: body could not be read; request not kept", "method", r.Method, "path", p, "remote", r.RemoteAddr)
		http.Error(w, "could not read the request body", http.StatusBadRequest)
		return
	case bodyBusy:
		h.Log.Warn("catch: too many bodies held at once; answered 503", "method", r.Method, "path", p, "held", h.heldNow())
	}

	busy := read == bodyBusy
	plan := h.plan(req, cfgText, busy)
	err = h.Store.AddRequest(ctx, req, h.Retention, plan.decide)
	if errors.Is(err, store.ErrNotFound) {
		// The endpoint was deleted after the lookup: the path is unknown now.
		req.EndpointID = store.Unmatched
		req.Verdict, req.Verdicts = "", "" // the endpoint's checks no longer apply
		plan = h.plan(req, "", busy)
		err = h.Store.AddRequest(ctx, req, h.Retention, nil)
	}
	if err != nil {
		h.Log.Error("catch: request not stored", "method", r.Method, "path", p, "err", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	h.Live.Publish(req.EndpointID)
	if req.EndpointID == store.Unmatched {
		h.Log.Info("unmatched request", "method", r.Method, "path", p, "remote", r.RemoteAddr, "no", req.No)
	}

	plan.free = free
	if changed := h.carryOut(w, r, req, plan); changed {
		if err := h.Store.UpdateOutcome(ctx, req.ID, plan.out.Status, plan.out.String()); err != nil {
			h.Log.Error("catch: outcome not stored", "request", req.ID, "err", err)
		}
		// Live feeds refresh the row with the final outcome.
		h.Live.Publish(req.EndpointID)
	}
}

// plan is what to do with a request, decided before it is stored.
type plan struct {
	cfg     emulate.Config
	data    *emulate.Data
	out     emulate.Outcome
	reply   emulate.Reply // to render, for replies and refusals
	render  bool          // reply holds templates to render (else an empty reply)
	closing bool          // close the connection after answering
	decide  store.Decide  // set when the rules decide (the request passed the checks)
	free    func()        // the body is no longer needed (MaxInFlight)
}

// plan works out the outcome for everything but the rules, which decide
// inside the store's transaction (plan.decide), where the counter is exact.
func (h *Handler) plan(req *store.Request, cfgText string, busy bool) *plan {
	pl := &plan{}
	if busy && req.EndpointID == store.Unmatched {
		pl.out = busyOutcome()
		req.Status, req.Outcome = pl.out.Status, pl.out.String()
		pl.closing = true
		return pl
	}
	if req.EndpointID == store.Unmatched {
		pl.out = emulate.Outcome{Source: emulate.FromUnmatched, Action: emulate.ActReply, Status: http.StatusNotFound}
		req.Status, req.Outcome = pl.out.Status, pl.out.String()
		pl.closing = req.TooLarge
		return pl
	}
	cfg, err := emulate.Parse(cfgText)
	if err != nil {
		h.Log.Error("catch: endpoint behaviour unreadable; using the default reply", "path", req.Path, "err", err)
		cfg = emulate.DefaultConfig()
	}
	pl.cfg = cfg
	er := EmulateRequest(*req)
	// Every authorization check gives its verdict, also on a request whose
	// body was not kept (those that need it say "not verifiable").
	verdicts, f := cfg.Verify(er, req.ReceivedAt, !req.TooLarge, h.Keys)
	req.Verdict, req.Verdicts = emulate.Overall(verdicts), emulate.VerdictsJSON(verdicts)
	if busy {
		pl.out = busyOutcome()
		req.Status, req.Outcome = pl.out.Status, pl.out.String()
		pl.closing = true
		return pl
	}
	if req.TooLarge {
		pl.out = emulate.Outcome{Source: emulate.FromLimit, Action: emulate.ActReply, Status: http.StatusRequestEntityTooLarge}
		req.Status, req.Outcome = pl.out.Status, pl.out.String()
		pl.closing = true
		return pl
	}
	if f != nil {
		pl.data = emulate.NewData(er, cfg.Vars, 0)
		pl.reply, pl.render = f.Refusal(), true
		pl.out = emulate.Outcome{Source: emulate.FromCheck, Check: f.Describe(), Action: emulate.ActRefused, Status: pl.reply.Status}
		req.Status, req.Outcome = pl.out.Status, pl.out.String()
		return pl
	}
	// The template data copies the body: made here, before the store's
	// transaction, so the copy is not made while the store is locked.
	pl.data = emulate.NewData(er, cfg.Vars, 0)
	pl.decide = func(n int64) (int, string) {
		ch := cfg.Choose(n)
		pl.data.N = n
		pl.out = emulate.Outcome{Source: emulate.FromDefault, Action: ch.Do, Counter: n}
		if ch.Rule > 0 {
			pl.out.Source, pl.out.Rule, pl.out.Label = emulate.FromRule, ch.Rule, ch.Label
		}
		if ch.Do == emulate.ActReply {
			pl.reply, pl.render = ch.Reply, true
			pl.out.Status = ch.Reply.Status
		}
		return pl.out.Status, pl.out.String()
	}
	return pl
}

// EmulateRequest is what checks and templates see of a stored request.
func EmulateRequest(r store.Request) emulate.Request {
	er := emulate.Request{Method: r.Method, Path: r.Path, RawQuery: r.RawQuery, Body: r.Body}
	for _, hd := range r.Headers {
		er.Headers = append(er.Headers, emulate.Header{Name: hd.Name, Value: hd.Value})
	}
	return er
}

// carryOut answers the request as planned and reports whether the outcome
// changed from what was stored (measurements, errors).
func (h *Handler) carryOut(w http.ResponseWriter, r *http.Request, req *store.Request, pl *plan) bool {
	if pl.free == nil {
		pl.free = func() {}
	}
	if !pl.render {
		// Nothing to render: the body is stored and needed no more, also
		// during a hold.
		req.Body, pl.data = nil, nil
		pl.free()
	}
	switch pl.out.Action {
	case emulate.ActTimeout:
		pl.out.WaitedMs = h.hold(r, h.maxWait()).Milliseconds()
		h.closeSilently(w, false)
		return true
	case emulate.ActDrop:
		h.closeSilently(w, true)
		return false
	}
	if !pl.render {
		if pl.closing {
			// The rest of the body is not wanted; do not drain it.
			w.Header().Set("Connection", "close")
		}
		for _, x := range pl.out.Headers {
			w.Header().Add(x.Name, x.Value)
		}
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(pl.out.Status)
		return false
	}
	rendered, err := pl.reply.Render(pl.data)
	req.Body, pl.data = nil, nil // rendered: the body is needed no more, also during a delay
	pl.free()
	if err != nil {
		h.Log.Warn("catch: reply template failed; answered 500", "path", req.Path, "request", req.ID, "err", err)
		pl.out.Action, pl.out.Error = emulate.ActError, err.Error()
		rendered = emulate.Rendered{Status: http.StatusInternalServerError,
			Headers: []emulate.Header{{Name: "Content-Type", Value: "text/plain; charset=utf-8"}},
			Body:    []byte("Hookwitness could not render this endpoint's reply: " + err.Error() + "\n")}
	}
	rendered = rendered.Protect()
	sent, withheld := rendered.AsSent(r.Method) // what net/http will send of it
	pl.out.SetReply(sent)
	pl.out.Withheld = withheld
	if d := pl.reply.Delay(h.maxWait()); d > 0 && err == nil {
		waited := h.wait(r, d)
		pl.out.DelayMs = waited.Milliseconds()
		if r.Context().Err() != nil {
			// The sender gave up before the delay ran out: it never got
			// this reply, so it is not recorded as one.
			pl.out.DelayMs = d.Milliseconds()
			pl.out.Abandon(waited.Milliseconds())
			return true
		}
	}
	hd := w.Header()
	hd["Content-Type"] = nil // no sniffing: only the configured headers
	for _, x := range rendered.Headers {
		if strings.EqualFold(x.Name, "Content-Type") {
			hd.Del("Content-Type")
		}
		hd.Add(x.Name, x.Value)
	}
	hd.Set("Content-Length", strconv.Itoa(len(rendered.Body)))
	w.WriteHeader(rendered.Status)
	w.Write(rendered.Body)
	return true
}

// wait sleeps for d unless the sender leaves or the server stops, and
// returns how long it actually waited.
func (h *Handler) wait(r *http.Request, d time.Duration) time.Duration {
	start := time.Now()
	var fired <-chan time.Time
	if h.after != nil {
		fired = h.after(d)
	} else {
		t := time.NewTimer(d)
		defer t.Stop()
		fired = t.C
	}
	select {
	case <-fired:
	case <-r.Context().Done():
	case <-h.stopping():
	}
	return time.Since(start)
}

// hold keeps a timeout's connection open until the sender gives up, the cap
// passes or the server stops, and returns how long that was.
func (h *Handler) hold(r *http.Request, limit time.Duration) time.Duration { return h.wait(r, limit) }

// closeSilently ends the connection without a response: with a TCP reset
// (reset) as a crashed receiver would, or a plain close.
func (h *Handler) closeSilently(w http.ResponseWriter, reset bool) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		// Not hijackable (HTTP/2 or a test recorder): the best left is to abort.
		panic(http.ErrAbortHandler)
	}
	if reset {
		if tc, ok := wire.Underlying(conn).(*net.TCPConn); ok {
			tc.SetLinger(0)
		}
	}
	conn.Close()
}

// unmatchedHead is the part of a body kept for a path with no endpoint: the
// Unmatched register is open to anything that reaches the server, so it
// keeps what shows a mistake, not whole uploads.
const unmatchedHead = 64 << 10

// retryAfter is what a busy answer asks the sender to wait, in seconds.
const retryAfter = "5"

func busyOutcome() emulate.Outcome {
	return emulate.Outcome{Source: emulate.FromBusy, Action: emulate.ActReply, Status: http.StatusServiceUnavailable,
		Headers: []emulate.Header{{Name: "Retry-After", Value: retryAfter}}}
}

// reserve takes n bytes of MaxInFlight, or reports that they are not free.
func (h *Handler) reserve(n int64) bool {
	if h.MaxInFlight <= 0 || n <= 0 {
		return true
	}
	h.heldMu.Lock()
	defer h.heldMu.Unlock()
	if h.held+n > h.MaxInFlight {
		return false
	}
	h.held += n
	return true
}

func (h *Handler) release(n int64) {
	if h.MaxInFlight <= 0 || n <= 0 {
		return
	}
	h.heldMu.Lock()
	h.held -= n
	h.heldMu.Unlock()
}

func (h *Handler) heldNow() int64 {
	h.heldMu.Lock()
	defer h.heldMu.Unlock()
	return h.held
}

// bodyRead is how reading a body went.
type bodyRead int

const (
	bodyOK     bodyRead = iota // req's body fields are set (TooLarge included)
	bodyFailed                 // the sender went away or stalled
	bodyBusy                   // MaxInFlight is spent: not read, not kept
)

// readBody fills req's body fields within MaxInFlight and returns the bytes
// it holds of it, to release when the body is no longer needed. A body is
// read whole up to MaxBody; for a path with no endpoint only its first
// unmatchedHead bytes are kept, and the rest is read to learn its size.
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request, req *store.Request) (bodyRead, int64) {
	if r.ContentLength > h.MaxBody {
		req.TooLarge, req.Size = true, r.ContentLength
		return bodyOK, 0
	}
	keep := h.MaxBody + 1 // one more to tell an over-limit body
	if req.EndpointID == store.Unmatched {
		keep = min(keep, unmatchedHead)
	}
	timeout := h.BodyTimeout
	if timeout == 0 {
		timeout = DefaultBodyTimeout
	}
	rc := http.NewResponseController(w)
	// Not every ResponseWriter supports deadlines (test recorders do not).
	_ = rc.SetReadDeadline(time.Now().Add(timeout))
	lr := io.LimitReader(r.Body, h.MaxBody+1)

	// The buffer grows as the body arrives, each step reserved first: a
	// declared length is reserved at once, an undeclared one in doublings.
	var reserved int64
	size := int64(64 << 10)
	if r.ContentLength >= 0 {
		size = r.ContentLength
	}
	size = min(max(size, 512), keep)
	if !h.reserve(size) {
		req.TooLarge, req.Size = true, r.ContentLength
		w.Header().Set("Connection", "close")
		return bodyBusy, 0
	}
	reserved = size
	buf := make([]byte, 0, size)
	for int64(len(buf)) < keep {
		if len(buf) == cap(buf) {
			more := min(int64(cap(buf)), keep-int64(cap(buf)))
			if !h.reserve(more) {
				h.release(reserved)
				req.TooLarge, req.Size, req.Body = true, r.ContentLength, nil
				w.Header().Set("Connection", "close")
				return bodyBusy, 0
			}
			reserved += more
			buf = append(buf[:cap(buf)], make([]byte, more)...)[:len(buf)]
		}
		n, err := lr.Read(buf[len(buf):min(int64(cap(buf)), keep)])
		buf = buf[:len(buf)+n]
		if err == io.EOF {
			break
		}
		if err != nil {
			// Keep the deadline: the server must not wait for the rest either.
			h.release(reserved)
			w.Header().Set("Connection", "close")
			return bodyFailed, 0
		}
	}
	total := int64(len(buf))
	if req.EndpointID == store.Unmatched && total == keep {
		rest, err := io.Copy(io.Discard, lr) // read, not kept: only its size
		if err != nil {
			h.release(reserved)
			w.Header().Set("Connection", "close")
			return bodyFailed, 0
		}
		total += rest
	}
	_ = rc.SetReadDeadline(time.Time{})
	if total > h.MaxBody {
		h.release(reserved)
		req.TooLarge, req.Size = true, r.ContentLength // -1 when not declared
		return bodyOK, 0
	}
	req.Body, req.Size = buf, total
	return bodyOK, reserved
}

// RawPath is the request path exactly as the sender wrote it, without the
// query: endpoints match it byte for byte, so no cleaning or decoding.
func RawPath(r *http.Request) string {
	if strings.HasPrefix(r.RequestURI, "/") {
		p, _, _ := strings.Cut(r.RequestURI, "?")
		return p
	}
	if r.RequestURI == "*" {
		return "*"
	}
	// Absolute form (a proxy-style request line).
	return r.URL.EscapedPath()
}

func rawQuery(r *http.Request) string {
	if _, q, found := strings.Cut(r.RequestURI, "?"); found {
		return q
	}
	return r.URL.RawQuery
}

func storeHeaders(r *http.Request, hs []wire.Header, ok bool) []store.Header {
	out := storeHeaderLines(r, hs, ok)
	for i, h := range out {
		if strings.EqualFold(h.Name, "Cookie") {
			out[i].Value = RedactSession(h.Value)
		}
	}
	return out
}

// SessionRedacted replaces the value of the admin's login cookie in a
// recorded Cookie header.
const SessionRedacted = "[hookwitness session]"

// RedactSession replaces the value of a hookwitness_session cookie in a
// Cookie header's value. Cookies ignore ports, so a browser logged in to an
// admin on another port of the same host sends it to catch paths too; it
// must never be kept with the evidence. Everything else stays as sent.
func RedactSession(v string) string {
	if !strings.Contains(v, access.CookieName) {
		return v
	}
	parts := strings.Split(v, ";")
	for i, p := range parts {
		name, _, found := strings.Cut(p, "=")
		if found && strings.TrimSpace(name) == access.CookieName {
			parts[i] = name + "=" + SessionRedacted
		}
	}
	return strings.Join(parts, ";")
}

func storeHeaderLines(r *http.Request, hs []wire.Header, ok bool) []store.Header {
	if ok {
		out := make([]store.Header, len(hs))
		for i, h := range hs {
			out[i] = store.Header{Name: h.Name, Value: h.Value}
		}
		return out
	}
	// Order unknown: Host first, then the names sorted; repeated values keep
	// their order.
	out := []store.Header{{Name: "Host", Value: r.Host}}
	names := make([]string, 0, len(r.Header))
	for n := range r.Header {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, v := range r.Header[n] {
			out = append(out, store.Header{Name: n, Value: v})
		}
	}
	return out
}
