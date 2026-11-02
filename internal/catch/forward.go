package catch

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"time"

	"github.com/petroladkin/hookwitness/internal/access"
	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/forward"
	"github.com/petroladkin/hookwitness/internal/store"
)

// forwarder is the Forward service, made with the default policy when the
// handler was given none.
func (h *Handler) forwarder() *forward.Service {
	h.fwOnce.Do(func() {
		if h.Forward == nil {
			h.Forward = forward.New(h.Log, forward.Options{MaxWait: h.maxWait(), Budget: h})
		}
	})
	return h.Forward
}

// loopText is the 508's body: a request that came back on the server's own
// forward is not forwarded again.
const loopText = "Hookwitness: this request came back on the server's own forward, so it is not forwarded again (a loop).\n"

func loopOutcome() emulate.Outcome {
	return emulate.Outcome{Source: emulate.FromLoop, Action: emulate.ActReply, Status: http.StatusLoopDetected}
}

// forwardRequest is what goes to the receiver of a caught request.
func forwardRequest(req *store.Request, rule int) forward.Request {
	return forward.Request{ID: req.ID, Endpoint: req.Path, Method: req.Method, RawQuery: req.RawQuery, Proto: req.Proto,
		Headers: req.Headers, Body: req.Body, Rule: rule}
}

// forwardOut lets the receiver answer: the request goes to the receiver,
// and its answer, or the failure answer, goes back to the sender. A sender
// that leaves does not end the exchange; the outcome records when it left.
// The exchange and the final outcome are stored together.
func (h *Handler) forwardOut(ctx context.Context, w http.ResponseWriter, r *http.Request, req *store.Request, pl *plan) {
	start := time.Now()
	done := make(chan forward.Exchange, 1)
	in := forwardRequest(req, 0)
	go func() { done <- h.forwarder().Exchange(ctx, *pl.cfg.Forward, in) }()
	var ex forward.Exchange
	var left time.Duration
	select {
	case ex = <-done:
	case <-r.Context().Done():
		left = time.Since(start)
		ex = <-done
	}
	defer ex.Release()
	req.Body = nil
	pl.free() // written to the receiver: the body is needed no more

	note := pl.out.Forward
	note.Pending, note.Status, note.ErrorKind = false, ex.Row.Status, ex.Kind
	note.Ms = time.Since(start).Milliseconds()
	answer := ex.Answer
	if ex.Kind != "" {
		note.Fallback = true
		answer = h.failureAnswer(req, pl, ex.Kind)
	}
	answer = answer.Protect(access.CookieName)
	sent, withheld := answer.AsSent(r.Method)
	pl.out.SetReply(sent)
	pl.out.Withheld = withheld
	if left == 0 && r.Context().Err() != nil {
		left = time.Since(start)
	}
	if left > 0 {
		note.SenderLeftMs = left.Milliseconds()
		pl.out.Abandon(left.Milliseconds())
	} else {
		writeReply(w, answer)
	}
	h.finishForward(ctx, req, pl, &ex.Row)
}

// forwardToo carries out a rule's answer to the sender while the receiver
// gets the request as well (the rule's forward switch), and stores both.
func (h *Handler) forwardToo(ctx context.Context, w http.ResponseWriter, r *http.Request, req *store.Request, pl *plan) {
	start := time.Now()
	done := make(chan forward.Exchange, 1)
	in := forwardRequest(req, pl.out.Rule)
	go func() { done <- h.forwarder().Exchange(ctx, *pl.cfg.Forward, in) }()
	// The body's bytes stay held until the receiver has it, not only until
	// the rule's reply is rendered.
	free := pl.free
	pl.free = func() {}
	h.carryOut(w, r, req, pl)
	ex := <-done
	ex.Release() // the receiver's answer is not passed back
	free()
	note := pl.out.Forward
	note.Pending, note.Status, note.ErrorKind = false, ex.Row.Status, ex.Kind
	note.Ms = time.Since(start).Milliseconds()
	h.finishForward(ctx, req, pl, &ex.Row)
}

// failureAnswer is what the sender gets when the receiver fails: the
// default reply when the endpoint says so, else a gateway error naming the
// failure (503 with Retry-After when forwards were busy).
func (h *Handler) failureAnswer(req *store.Request, pl *plan, kind string) emulate.Rendered {
	if pl.cfg.Forward.FallsBackToDefault() && kind != emulate.FailBusy {
		if rendered, err := pl.cfg.Default.Render(pl.data); err == nil {
			return rendered
		} else {
			h.Log.Warn("catch: default reply template failed after a forward failed", "path", req.Path, "request", req.ID, "err", err)
		}
	}
	status := emulate.GatewayStatus(kind)
	hs := []emulate.Header{{Name: "Content-Type", Value: "text/plain; charset=utf-8"}}
	if kind == emulate.FailBusy {
		hs = append(hs, emulate.Header{Name: "Retry-After", Value: retryAfter})
	}
	return emulate.Rendered{Status: status, Headers: hs,
		Body: []byte("Hookwitness: " + strconv.Itoa(status) + ": " + emulate.FailureWords(kind) + ".\n")}
}

// finishForward stores the exchange and the final outcome, and tells the
// live feeds.
func (h *Handler) finishForward(ctx context.Context, req *store.Request, pl *plan, row *store.Forward) {
	err := h.Store.FinishForward(ctx, row, pl.out.Status, pl.out.String())
	switch {
	case errors.Is(err, store.ErrNotFound):
		h.Log.Debug("catch: forwarded request gone before its exchange was stored", "request", req.ID)
	case err != nil:
		h.Log.Error("catch: forward not stored", "request", req.ID, "err", err)
	}
	h.Live.Publish(req.EndpointID)
}
