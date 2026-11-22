// Package forward passes caught requests through to the receiver an
// endpoint names (the recorder in the middle) and brings back its answer.
// It bounds the work (64 exchanges at once, the answer's bytes held against
// -max-inflight, a timeout), knows its own connections so a request that
// comes back on one is a loop, and leaves the wire to internal/send, which
// sends byte for byte and applies the target policy on the dialled address.
// The catcher decides what the sender gets and stores the exchange.
package forward

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/send"
	"github.com/petroladkin/hookwitness/internal/store"
	"github.com/petroladkin/hookwitness/internal/wire"
)

// Limits.
const (
	Slots     = 64              // exchanges in flight at once
	SlotWait  = 5 * time.Second // an exchange waits this long for a slot
	MaxAnswer = 1 << 20         // the receiver's answer passed back; larger is a failure
	KeepBody  = 64 << 10        // the receiver's body kept with the request
)

// Budget holds bytes against -max-inflight (the catcher's Reserve and
// Release); nil holds nothing.
type Budget interface {
	Reserve(n int64) bool
	Release(n int64)
}

// Options configure a Service.
type Options struct {
	Policy  send.Policy
	MaxWait time.Duration // caps each forward's timeout
	Budget  Budget
	// SlotWait is how long an exchange waits for a slot; 0 means SlotWait.
	SlotWait time.Duration

	// Tests: the resolver the sender uses, and the sender itself.
	Resolver send.Resolver
	Send     func(context.Context, send.Head, []byte, send.Options) (send.Response, error)
}

// Service forwards requests. Create it with New.
type Service struct {
	log   *slog.Logger
	o     Options
	slots chan struct{}

	mu  sync.Mutex
	own map[string]int // local addresses of the connections open for forwards

	stopCtx context.Context // done on Stop
	stop    context.CancelFunc
	running sync.WaitGroup
}

// New returns a Service.
func New(log *slog.Logger, o Options) *Service {
	if o.MaxWait <= 0 {
		o.MaxWait = 5 * time.Minute
	}
	if o.SlotWait <= 0 {
		o.SlotWait = SlotWait
	}
	if o.Send == nil {
		o.Send = send.Send
	}
	s := &Service{log: log, o: o, slots: make(chan struct{}, Slots), own: map[string]int{}}
	s.stopCtx, s.stop = context.WithCancel(context.Background())
	return s
}

// Policy is the target policy in force.
func (s *Service) Policy() send.Policy { return s.o.Policy }

// MaxWait is the cap of a forward's timeout.
func (s *Service) MaxWait() time.Duration { return s.o.MaxWait }

// Stop ends the exchanges in flight (each ends as a timeout, "the server
// stopped") and refuses new ones.
func (s *Service) Stop() { s.stop() }

// Wait returns when every exchange in flight has ended.
func (s *Service) Wait() { s.running.Wait() }

// Own reports whether addr, a caught request's remote address, is the
// local end of a connection a forward has open: the request came back to
// the server on its own forward.
func (s *Service) Own(addr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.own[addr] > 0
}

func (s *Service) hold(addr string) {
	s.mu.Lock()
	s.own[addr]++
	s.mu.Unlock()
}

func (s *Service) drop(addr string) {
	s.mu.Lock()
	if s.own[addr]--; s.own[addr] <= 0 {
		delete(s.own, addr)
	}
	s.mu.Unlock()
}

// Request is what is forwarded of a caught request.
type Request struct {
	ID       int64
	Endpoint string // the endpoint's path, for logs
	Method   string
	RawQuery string
	Proto    string
	Headers  []store.Header
	Body     []byte
	Rule     int // the rule that let the receiver get it too; 0: forwarded as no rule took it
}

// Exchange is one forward's result.
type Exchange struct {
	// Row is the exchange as stored with the request (store.FinishForward).
	Row store.Forward
	// Kind is "" when the receiver answered, else the failure's kind
	// (emulate.Fail*, or send's: dial, tls, timeout, reset, write, answer).
	Kind string
	// Answer is what passes back to the sender when the receiver answered:
	// its status, its header lines without framing and hop-by-hop ones,
	// its whole body (at most MaxAnswer).
	Answer emulate.Rendered
	// Release gives back the bytes held for the answer; call it once the
	// answer is written (or not needed). Never nil.
	Release func()
}

// options is the stored record of a forward's switches.
type options struct {
	Host        string `json:"host"` // target or received
	InsecureTLS bool   `json:"insecure_tls"`
	TimeoutMS   int64  `json:"timeout_ms"`
	ByRule      int    `json:"by_rule,omitempty"`
}

// Exchange sends req to the receiver f names and reads its answer. It
// always returns an exchange to store: a failure (the policy, no slot, the
// receiver down, an answer too large) has Kind set. The caller going away
// does not end it; Stop does.
func (s *Service) Exchange(ctx context.Context, f emulate.Forward, req Request) Exchange {
	timeout := f.Timeout(s.o.MaxWait)
	host := "target"
	if f.KeepHost {
		host = "received"
	}
	opts, _ := json.Marshal(options{Host: host, InsecureTLS: f.InsecureTLS, TimeoutMS: timeout.Milliseconds(), ByRule: req.Rule})
	ex := Exchange{Row: store.Forward{RequestID: req.ID, CreatedAt: time.Now().UTC(), URL: f.URL, Options: string(opts)}, Release: func() {}}
	u, err := f.Target()
	if err != nil {
		return s.failed(ex, req, nil, emulate.FailRefused, err.Error())
	}
	ex.Row.URL = u.String()
	if err := s.o.Policy.CheckURL(u); err != nil {
		return s.failed(ex, req, u, emulate.FailRefused, policyWords(err))
	}

	wait := time.NewTimer(s.o.SlotWait)
	defer wait.Stop()
	select {
	case s.slots <- struct{}{}:
	case <-wait.C:
		return s.failed(ex, req, u, emulate.FailBusy, fmt.Sprintf("all %d forward slots stayed busy for %s", Slots, s.o.SlotWait))
	case <-s.stopCtx.Done():
		return s.failed(ex, req, u, "timeout", "timeout: the server stopped before the answer ended")
	}
	defer func() { <-s.slots }()
	if s.o.Budget != nil {
		if !s.o.Budget.Reserve(MaxAnswer) {
			return s.failed(ex, req, u, emulate.FailBusy, "too many bytes held at once (-max-inflight) for the receiver's answer")
		}
		var once sync.Once
		ex.Release = func() { once.Do(func() { s.o.Budget.Release(MaxAnswer) }) }
	}
	s.running.Add(1)
	defer s.running.Done()

	head := send.Head{Method: req.Method, Target: u.RequestURI()}
	if req.RawQuery != "" {
		head.Target += "?" + req.RawQuery
	}
	for _, hd := range req.Headers {
		head.Headers = append(head.Headers, wire.Header{Name: hd.Name, Value: hd.Value})
	}
	so := send.Options{URL: u, Timeout: timeout, MaxBody: MaxAnswer, Policy: s.o.Policy, InsecureTLS: f.InsecureTLS,
		KeepHost: f.KeepHost, Resolver: s.o.Resolver}
	var local string
	so.OnConnect = func(a net.Addr) {
		local = a.String()
		s.hold(local)
	}
	// The sender leaving does not end the exchange; shutdown does.
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	stopped := context.AfterFunc(s.stopCtx, cancel)
	defer stopped()
	res, sendErr := s.o.Send(sctx, head, req.Body, so)
	if local != "" {
		s.drop(local)
	}

	r := &ex.Row
	r.Line, r.Notes, r.Proto, r.Status, r.Reason = res.Line, res.Notes, res.Proto, res.Status, res.Reason
	r.Size, r.Truncated, r.Duration, r.TTFB = res.Size, res.Truncated, res.Duration, res.TTFB
	if req.Proto != "" && req.Proto != "HTTP/1.1" {
		r.Notes = append([]string{"sent as HTTP/1.1 (received as " + req.Proto + ")"}, r.Notes...)
	}
	for _, hd := range res.Sent {
		r.Sent = append(r.Sent, store.Header{Name: hd.Name, Value: hd.Value})
	}
	for _, hd := range res.Headers {
		r.Headers = append(r.Headers, store.Header{Name: hd.Name, Value: hd.Value})
	}
	r.Body = res.Body
	if len(r.Body) > KeepBody {
		r.Body, r.Truncated = r.Body[:KeepBody], true
	}

	var se *send.Error
	switch {
	case errors.Is(sendErr, send.ErrRefused):
		// Refused on the address to dial: nothing was sent.
		r.Line, r.Sent, r.Notes = "", nil, nil
		return s.failed(ex, req, u, emulate.FailRefused, policyWords(sendErr))
	case errors.As(sendErr, &se):
		why := se.Error()
		if s.stopCtx.Err() != nil && se.Kind == "timeout" {
			why = "timeout: the server stopped before the answer ended"
		}
		return s.failed(ex, req, u, se.Kind, why)
	case sendErr != nil:
		return s.failed(ex, req, u, "dial", sendErr.Error())
	case res.Status == http.StatusSwitchingProtocols:
		return s.failed(ex, req, u, emulate.FailUpgrade, "the receiver answered 101 Switching Protocols; tunnels are not passed back")
	case res.Truncated:
		return s.failed(ex, req, u, emulate.FailTooLarge, fmt.Sprintf("the receiver's answer is over %d KiB, the most passed back to the sender", MaxAnswer>>10))
	}
	ex.Answer = emulate.Rendered{Status: res.Status, Headers: passBack(res.Headers), Body: res.Body}
	s.log.Debug("request forwarded", "request", req.ID, "endpoint", req.Endpoint, "target", origin(u),
		"status", res.Status, "ms", res.Duration.Milliseconds())
	return ex
}

// failed records a failed exchange and logs it, naming the target's origin
// only (never its path's query, header values or bodies).
func (s *Service) failed(ex Exchange, req Request, u *url.URL, kind, why string) Exchange {
	ex.Kind = kind
	ex.Row.ErrorKind, ex.Row.Error = kind, oneLine(why)
	attrs := []any{"request", req.ID, "endpoint", req.Endpoint, "error", kind}
	if u != nil {
		attrs = append(attrs, "target", origin(u))
	}
	if kind == emulate.FailRefused {
		attrs = append(attrs, "policy", s.o.Policy.String())
	}
	s.log.Warn("forward failed", attrs...)
	return ex
}

func origin(u *url.URL) string {
	host, port := send.Endpoint(u)
	return u.Scheme + "://" + net.JoinHostPort(host, strconv.Itoa(port))
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// policyWords is a policy refusal for the exchange's record: forwarding
// shares the policy, and its words, with replay.
func policyWords(err error) string {
	if strings.Contains(err.Error(), "-replay-allow none") {
		return "forwarding is off on this server (-replay-allow none)"
	}
	return err.Error()
}

// hopByHop are the answer's lines that belong to its connection or its
// framing (the server frames what it passes back itself), and those that
// would act on the whole origin the catch paths share with the admin UI:
// clearing its site data, pinning it to HTTPS, moving it to another
// service.
var hopByHop = []string{"Connection", "Content-Length", "Keep-Alive", "Proxy-Authenticate", "Proxy-Connection",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade", "Clear-Site-Data", "Strict-Transport-Security", "Alt-Svc"}

// passBack is the receiver's header lines without framing and hop-by-hop
// lines, nor those its Connection line names.
func passBack(hs []wire.Header) []emulate.Header {
	drop := map[string]bool{}
	for _, n := range hopByHop {
		drop[strings.ToLower(n)] = true
	}
	for _, h := range hs {
		if strings.EqualFold(h.Name, "Connection") {
			for _, n := range strings.Split(h.Value, ",") {
				if n = strings.TrimSpace(n); n != "" {
					drop[strings.ToLower(n)] = true
				}
			}
		}
	}
	var out []emulate.Header
	for _, h := range hs {
		if !drop[strings.ToLower(h.Name)] {
			out = append(out, emulate.Header{Name: h.Name, Value: h.Value})
		}
	}
	return out
}
