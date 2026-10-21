// Package replay sends a caught request again, to a URL a person or an
// agent gives, and records the target's answer with the request. It loads
// the request, refuses what cannot be sent byte for byte, bounds the work
// (four at once, the bytes held, a timeout) and leaves the wire to
// internal/send, which applies the target policy.
package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/petroladkin/hookwitness/internal/access"
	"github.com/petroladkin/hookwitness/internal/send"
	"github.com/petroladkin/hookwitness/internal/store"
	"github.com/petroladkin/hookwitness/internal/wire"
)

// Limits.
const (
	Slots       = 4               // replays in flight at once
	SlotWait    = 5 * time.Second // a replay waits this long for a slot
	MinTimeout  = 100 * time.Millisecond
	DefaultWait = send.DefaultTimeout
)

// Errors a caller answers by kind.
var (
	ErrNotFound = errors.New("no such request")
	ErrBusy     = errors.New("too many replays at once; try again in a few seconds")
	ErrStopped  = errors.New("the server is stopping")
)

// NotKeptError: the request's body was not stored whole, so it cannot be
// sent as it arrived.
type NotKeptError struct{ Reason string }

func (e *NotKeptError) Error() string { return e.Reason }

// InvalidError is a bad input; Field names it.
type InvalidError struct{ Field, Reason string }

func (e *InvalidError) Error() string { return e.Reason }

// Input is one replay's target and switches.
type Input struct {
	URL         string
	KeepHost    bool          // send the received Host line; by default the target's host and port
	InsecureTLS bool          // https: accept any certificate
	Timeout     time.Duration // 0: the default, 30 s or -max-wait if shorter
}

// Budget holds request bodies' bytes against -max-inflight (the catcher's
// Reserve and Release); nil holds nothing.
type Budget interface {
	Reserve(n int64) bool
	Release(n int64)
}

// Options configure a Service.
type Options struct {
	Policy  send.Policy
	MaxWait time.Duration // the longest timeout a replay may ask for
	Budget  Budget
	// SlotWait is how long a replay waits for a slot; 0 means SlotWait.
	SlotWait time.Duration

	// Tests: the resolver and roots the sender uses.
	Resolver send.Resolver
	Send     func(context.Context, send.Head, []byte, send.Options) (send.Response, error)
}

// Service replays requests. Create it with New.
type Service struct {
	st    *store.Store
	log   *slog.Logger
	o     Options
	slots chan struct{}

	stopCtx context.Context // done on Stop
	stop    context.CancelFunc
	running sync.WaitGroup
}

// New returns a Service.
func New(st *store.Store, log *slog.Logger, o Options) *Service {
	if o.MaxWait <= 0 {
		o.MaxWait = 5 * time.Minute
	}
	if o.SlotWait <= 0 {
		o.SlotWait = SlotWait
	}
	if o.Send == nil {
		o.Send = send.Send
	}
	s := &Service{st: st, log: log, o: o, slots: make(chan struct{}, Slots)}
	s.stopCtx, s.stop = context.WithCancel(context.Background())
	return s
}

// Policy is the target policy in force.
func (s *Service) Policy() send.Policy { return s.o.Policy }

// DefaultTimeout is the timeout of a replay that asks for none.
func (s *Service) DefaultTimeout() time.Duration { return min(DefaultWait, s.o.MaxWait) }

// MaxTimeout is the longest timeout a replay may ask for.
func (s *Service) MaxTimeout() time.Duration { return s.o.MaxWait }

// Stop ends the replays in flight (each is recorded with a timeout error)
// and refuses new ones.
func (s *Service) Stop() { s.stop() }

// Wait returns when every replay in flight has been recorded.
func (s *Service) Wait() { s.running.Wait() }

// Origin is a URL's scheme, host and port: what logs name of a target.
func Origin(u *url.URL) string {
	host, port := send.Endpoint(u)
	return u.Scheme + "://" + net.JoinHostPort(host, strconv.Itoa(port))
}

// options is the stored record of a replay's switches.
type options struct {
	Host        string `json:"host"` // target or received
	InsecureTLS bool   `json:"insecure_tls"`
	TimeoutMS   int64  `json:"timeout_ms"`
}

// Replay sends the request id to in.URL and stores the outcome. A target
// that fails (refuses the connection, times out, resets) is still a stored
// replay with its Error set; errors are returned for what was not sent:
// ErrNotFound, *NotKeptError, *InvalidError, a policy refusal (wrapping
// send.ErrRefused), ErrBusy and ErrStopped.
func (s *Service) Replay(ctx context.Context, id int64, in Input) (store.Replay, error) {
	u, err := send.ParseTarget(in.URL)
	if err != nil {
		return store.Replay{}, &InvalidError{"url", err.Error()}
	}
	timeout := in.Timeout
	if timeout == 0 {
		timeout = s.DefaultTimeout()
	}
	if timeout < MinTimeout || timeout > s.o.MaxWait {
		return store.Replay{}, &InvalidError{"timeout_ms", fmt.Sprintf("the timeout is from %d to %d ms (-max-wait)",
			MinTimeout.Milliseconds(), s.o.MaxWait.Milliseconds())}
	}
	if err := s.o.Policy.CheckURL(u); err != nil {
		s.refused(ctx, id, u)
		return store.Replay{}, err
	}
	req, err := s.st.Request(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Replay{}, ErrNotFound
	}
	if err != nil {
		return store.Replay{}, err
	}
	switch {
	case req.TooLarge:
		return store.Replay{}, &NotKeptError{"the body of this request was not kept (over the size limit, or the server was busy), so it cannot be sent as it arrived"}
	case req.Kept < req.Size:
		return store.Replay{}, &NotKeptError{fmt.Sprintf("only the first %d of %d body bytes were kept (a path with no endpoint), so it cannot be sent as it arrived", req.Kept, req.Size)}
	}

	if s.stopped() {
		return store.Replay{}, ErrStopped
	}
	wait := time.NewTimer(s.o.SlotWait)
	defer wait.Stop()
	select {
	case s.slots <- struct{}{}:
	case <-wait.C:
		return store.Replay{}, ErrBusy
	case <-s.stopCtx.Done():
		return store.Replay{}, ErrStopped
	case <-ctx.Done():
		return store.Replay{}, ctx.Err()
	}
	defer func() { <-s.slots }()
	n := int64(len(req.Body))
	if s.o.Budget != nil && !s.o.Budget.Reserve(n) {
		return store.Replay{}, ErrBusy
	}
	if s.o.Budget != nil {
		defer s.o.Budget.Release(n)
	}
	s.running.Add(1)
	defer s.running.Done()

	head := send.Head{Method: req.Method, Target: u.RequestURI()}
	for _, hd := range req.Headers {
		head.Headers = append(head.Headers, wire.Header{Name: hd.Name, Value: hd.Value})
	}
	so := send.Options{URL: u, Timeout: timeout, Policy: s.o.Policy, InsecureTLS: in.InsecureTLS, KeepHost: in.KeepHost,
		Resolver: s.o.Resolver}
	// A caller that goes away does not cancel the replay: it is recorded
	// either way, within its timeout; shutdown ends it.
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	stopped := context.AfterFunc(s.stopCtx, cancel)
	defer stopped()
	started := time.Now().UTC()
	res, sendErr := s.o.Send(sctx, head, req.Body, so)
	if errors.Is(sendErr, send.ErrRefused) {
		s.refused(ctx, id, u)
		return store.Replay{}, sendErr
	}

	host := "target"
	if in.KeepHost {
		host = "received"
	}
	opts, _ := json.Marshal(options{Host: host, InsecureTLS: in.InsecureTLS, TimeoutMS: timeout.Milliseconds()})
	rp := &store.Replay{RequestID: id, CreatedAt: started, URL: u.String(), Options: string(opts), Line: res.Line,
		Notes: res.Notes, Proto: res.Proto, Status: res.Status, Reason: res.Reason, Body: res.Body, Size: res.Size,
		Truncated: res.Truncated, Duration: res.Duration, TTFB: res.TTFB}
	if req.Proto != "" && req.Proto != "HTTP/1.1" {
		rp.Notes = append([]string{"sent as HTTP/1.1 (received as " + req.Proto + ")"}, rp.Notes...)
	}
	if id := access.From(ctx); id != nil {
		rp.By = id.Name
	}
	for _, hd := range res.Sent {
		rp.Sent = append(rp.Sent, store.Header{Name: hd.Name, Value: hd.Value})
	}
	for _, hd := range res.Headers {
		rp.Headers = append(rp.Headers, store.Header{Name: hd.Name, Value: hd.Value})
	}
	var se *send.Error
	switch {
	case errors.As(sendErr, &se):
		rp.ErrorKind, rp.Error = se.Kind, se.Error()
		if s.stopped() && se.Kind == "timeout" {
			rp.Error = "timeout: the server stopped before the answer ended"
		}
	case sendErr != nil:
		rp.ErrorKind, rp.Error = "dial", sendErr.Error()
	}
	if err := s.st.AddReplay(context.WithoutCancel(ctx), rp); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Replay{}, ErrNotFound // cleared while it was sent
		}
		return store.Replay{}, err
	}
	attrs := []any{"request", id, "target", Origin(u), "replay", rp.ID, "ms", rp.Duration.Milliseconds()}
	if rp.ErrorKind != "" {
		attrs = append(attrs, "error", rp.ErrorKind)
	} else {
		attrs = append(attrs, "status", rp.Status)
	}
	s.log.Info("request replayed", append(attrs, access.Who(ctx)...)...)
	return *rp, nil
}

func (s *Service) refused(ctx context.Context, id int64, u *url.URL) {
	s.log.Warn("replay refused by the target policy", append([]any{"request", id, "target", Origin(u), "policy", s.o.Policy.String()},
		access.Who(ctx)...)...)
}

func (s *Service) stopped() bool { return s.stopCtx.Err() != nil }
