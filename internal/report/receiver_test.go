package report

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/store"
)

// TestReceiverInReports: each forwarded request carries the receiver's side
// (and, with the exchange loaded, its answer, masked like the request's
// headers); the summary counts it; the setup section names the receiver.
func TestReceiverInReports(t *testing.T) {
	at := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	req := func(id int64, status int, outcome string) store.Request {
		return store.Request{ID: id, No: id, EndpointID: 1, ReceivedAt: at.Add(time.Duration(id) * time.Second), Method: "POST", Path: "/live",
			Proto: "HTTP/1.1", Status: status, Outcome: outcome, Body: []byte(`{"n":1}`), Size: 7}
	}
	rs := []store.Request{
		req(1, 201, `{"source":"forward","action":"forward","status":201,"forward":{"target":"https://receiver.test/in","status":201,"ms":38}}`),
		req(2, 504, `{"source":"forward","action":"forward","status":504,"forward":{"target":"https://receiver.test/in","error_kind":"timeout","fallback":true,"ms":30000}}`),
		req(3, 503, `{"source":"rule","rule":1,"label":"every 3rd","action":"reply","status":503,"not_forwarded":true}`),
		req(4, 0, `{"source":"forward","action":"abandoned","would_status":200,"waited_ms":10000,"forward":{"target":"https://receiver.test/in","status":200,"sender_left_ms":10000,"ms":14200}}`),
	}
	cfg := emulate.Config{Default: emulate.Reply{Status: 200}, Forward: &emulate.Forward{URL: "https://receiver.test/in"},
		Rules: []emulate.Rule{{When: emulate.Condition{Kind: emulate.WhenEvery, N: 3}, Do: emulate.ActReply, Reply: emulate.Reply{Status: 503}}}}
	o := Options{Endpoint: "/live", Selection: "whole register", Generated: at, Version: "test", Mask: []string{"Set-Cookie"}, Setup: &cfg,
		Exchange: func(_ context.Context, id int64) (store.Forward, error) {
			if id != 1 {
				return store.Forward{}, store.ErrNotFound
			}
			return store.Forward{RequestID: 1, Status: 201, Reason: "Created", Headers: []store.Header{{Name: "Set-Cookie", Value: "s-ecret=1"},
				{Name: "Content-Type", Value: "application/json"}}, Body: []byte(`{"id":7}`), Size: 8}, nil
		}}
	for _, f := range []Format{Markdown, HTML} {
		out := render(t, f, o, rs)
		for _, want := range []string{
			"forwarded 3 · the receiver answered 2xx 2 · failed: timeout 1 · the sender left before the answer 1 · not forwarded (rules) 1",
			"201 from https://receiver.test in 38 ms",
			"no answer from https://receiver.test: the receiver did not answer within the timeout · after 30.0 s",
			"not forwarded: rule 1 took it",
			"the sender left after 10.0 s",
			"left after 10.0 s, before the answer; nothing sent, would have been 200",
			"requests no rule takes are forwarded to https://receiver.test (its path is not shown) · Host of the target · timeout 30.0 s · 502/504 when it fails",
			"every 3rd",
			"· not forwarded",
			"The receiver",
			"•••• 9 chars",
		} {
			if !strings.Contains(out, want) && !strings.Contains(out, strings.ReplaceAll(want, "'", "&#39;")) {
				t.Errorf("%s: no %q", f, want)
			}
		}
		if strings.Contains(out, "s-ecret") {
			t.Fatalf("%s: a masked receiver header is in the report", f)
		}
		if strings.Contains(out, "receiver.test/in") {
			t.Fatalf("%s: the receiver's path is in the report", f)
		}
	}
	if pdf := render(t, PDF, o, rs); !strings.HasPrefix(pdf, "%PDF-") {
		t.Fatal("no PDF")
	}
	// Without forwarding nothing changes: the golden files hold.
	if md := render(t, Markdown, goldenOptions(), fixture(t)); strings.Contains(md, "Receiver") {
		t.Fatal("a report without forwarding names a receiver")
	}
}

// A request whose body was dropped to stay under the database size limit
// shows the reason and its digest instead of the body.
func TestDroppedBodyInReports(t *testing.T) {
	at := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	r := store.Request{ID: 1, No: 1, EndpointID: 1, ReceivedAt: at, Method: "POST", Path: "/a", Proto: "HTTP/1.1", Status: 200,
		Outcome: `{"source":"default","action":"reply","status":200}`, Size: 2048, BodySHA256: []byte{0xab, 0xcd}, BodyDropped: true}
	o := Options{Endpoint: "/a", Selection: "whole register", Generated: at, Version: "test"}
	for _, f := range []Format{Markdown, HTML} {
		out := render(t, f, o, []store.Request{r})
		if !strings.Contains(out, "2.0 KiB · not kept: the body was dropped to stay under the database size limit · sha256 abcd") {
			t.Errorf("%s: no reason for the dropped body", f)
		}
	}
	if pdf := render(t, PDF, o, []store.Request{r}); !strings.HasPrefix(pdf, "%PDF-") {
		t.Fatal("no PDF")
	}
}
