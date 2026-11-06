package emulate

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/auth"
)

func ptrF(v float64) *float64 { return &v }

func TestJudgeDelivery(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	at := func(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }
	reply := func(status int, hs ...Header) Outcome { return Outcome{Action: ActReply, Status: status, Headers: hs} }
	prev := func(no int64, s float64, o Outcome) *Attempt { return &Attempt{No: no, At: at(s), Outcome: o} }
	ev := Facts{Event: "evt_1"}
	order := Check{Kind: KindOrder, Order: &OrderSpec{Source{FromBody, "/created"}, "number"}}
	backoff := Check{Kind: KindBackoff, FirstS: 1, MaxS: 4, MaxAttempts: 5}
	raOnly := Check{Kind: KindBackoff}
	for _, c := range []struct {
		name   string
		ch     Check
		f      Facts
		rel    Relation
		at     float64
		result auth.Result
		reason string
	}{
		{"first delivery", Check{Kind: KindDuplicates}, ev, Relation{Attempt: 1}, 0, auth.Pass, "first delivery of evt_1"},
		{"retry after a 503", Check{Kind: KindDuplicates}, ev, Relation{Attempt: 2, Prev: prev(12, 0, reply(503))}, 5, auth.Pass, "attempt 2 of evt_1; #12 was answered 503"},
		{"retry after a timeout", Check{Kind: KindDuplicates}, ev, Relation{Attempt: 2, Prev: prev(12, 0, Outcome{Action: ActTimeout})}, 5, auth.Pass, "held until a timeout"},
		{"retry after a drop", Check{Kind: KindDuplicates}, ev, Relation{Attempt: 3, Prev: prev(12, 0, Outcome{Action: ActDrop})}, 5, auth.Pass, "#12 was dropped"},
		{"duplicate after a 200", Check{Kind: KindDuplicates}, ev, Relation{Attempt: 2, Prev: prev(12, 0, reply(200))}, 5, auth.Fail, "duplicate of #12, which was answered 200"},
		{"no event id", Check{Kind: KindDuplicates}, Facts{Missing: "no event id (header X-GitHub-Delivery missing)"}, Relation{}, 0, auth.NotVerifiable, "header X-GitHub-Delivery missing"},
		{"by body", Check{Kind: KindDuplicates}, Facts{ByBody: true}, Relation{Attempt: 1}, 0, auth.Pass, "first delivery of the body"},

		{"first in order", order, Facts{HasOrder: true, Order: ptrF(40)}, Relation{}, 0, auth.Pass, "first in order"},
		{"in order", order, Facts{HasOrder: true, Order: ptrF(41)}, Relation{High: &Mark{40, 16}}, 0, auth.Pass, "41 after 40 (#16)"},
		{"a tie passes", order, Facts{HasOrder: true, Order: ptrF(40)}, Relation{High: &Mark{40, 16}}, 0, auth.Pass, "40 after 40"},
		{"out of order", order, Facts{HasOrder: true, Order: ptrF(40)}, Relation{High: &Mark{41, 17}}, 0, auth.Fail, "out of order: 40 after 41 (#17)"},
		{"a retry is not judged again", order, Facts{Event: "evt_1", HasOrder: true, Order: ptrF(40)}, Relation{Attempt: 2, Prev: prev(3, 0, reply(503)), High: &Mark{41, 17}}, 0, auth.Pass, "attempt 2 of evt_1: judged on its first delivery"},
		{"no order value", order, Facts{HasOrder: true, OrderErr: "no value at /created"}, Relation{}, 0, auth.NotVerifiable, "no value at /created"},

		{"backoff: first attempt", backoff, ev, Relation{Attempt: 1}, 0, auth.NotChecked, "first attempt"},
		{"backoff: on time", backoff, ev, Relation{Attempt: 2, Prev: prev(1, 0, reply(503))}, 1.1, auth.Pass, "attempt 2 after 1.1 s, expected 1 s ± 25 %"},
		{"backoff: second gap doubles", backoff, ev, Relation{Attempt: 3, Prev: prev(2, 0, reply(503))}, 1.9, auth.Pass, "expected 2 s"},
		{"backoff: capped at max_s", backoff, ev, Relation{Attempt: 5, Prev: prev(4, 0, reply(503))}, 4, auth.Pass, "expected 4 s"},
		{"backoff: too early", backoff, ev, Relation{Attempt: 4, Prev: prev(3, 0, reply(503))}, 0.5, auth.Fail, "attempt 4 after 0.5 s, expected 4 s ± 25 %"},
		{"backoff: too late", backoff, ev, Relation{Attempt: 2, Prev: prev(1, 0, reply(503))}, 3, auth.Fail, "attempt 2 after 3 s, expected 1 s"},
		{"backoff: over max attempts", backoff, ev, Relation{Attempt: 6, Prev: prev(5, 0, reply(503))}, 4, auth.Fail, "attempt 6: more than 5 attempts"},
		{"backoff: from the end of a delayed reply", backoff, ev, Relation{Attempt: 2, Prev: prev(1, 0, Outcome{Action: ActReply, Status: 503, DelayMs: 3000})}, 4, auth.Pass, "attempt 2 after 1 s"},
		{"backoff: from the end of a hold", backoff, ev, Relation{Attempt: 2, Prev: prev(1, 0, Outcome{Action: ActTimeout, WaitedMs: 10000})}, 11, auth.Pass, "attempt 2 after 1 s"},
		{"retry-after: early", backoff, ev, Relation{Attempt: 2, Prev: prev(12, 0, reply(429, Header{"Retry-After", "10"}))}, 3.2, auth.Fail, "attempt 2 after 3.2 s; #12 asked Retry-After: 10 s"},
		{"retry-after wins when longer", backoff, ev, Relation{Attempt: 2, Prev: prev(12, 0, reply(429, Header{"Retry-After", "10"}))}, 10, auth.Pass, "expected 10 s"},
		{"retry-after: a second early", raOnly, ev, Relation{Attempt: 2, Prev: prev(12, 0, reply(429, Header{"Retry-After", "2"}))}, 1, auth.Fail, "attempt 2 after 1 s; #12 asked Retry-After: 2 s"},
		{"retry-after: just before", raOnly, ev, Relation{Attempt: 2, Prev: prev(12, 0, reply(429, Header{"Retry-After", "2"}))}, 1.999, auth.Fail, "asked Retry-After: 2 s"},
		{"retry-after as a date: a second's grace", raOnly, ev, Relation{Attempt: 2, Prev: prev(12, 0, reply(503, Header{"Retry-After", at(30).Format(http.TimeFormat)}))}, 29.2, auth.Pass, "asked Retry-After: 30 s"},
		{"retry-after only: honoured", raOnly, ev, Relation{Attempt: 2, Prev: prev(12, 0, reply(429, Header{"retry-after", "2"}))}, 2, auth.Pass, "asked Retry-After: 2 s"},
		{"retry-after only: nothing to honour", raOnly, ev, Relation{Attempt: 2, Prev: prev(12, 0, reply(503))}, 2, auth.NotChecked, "no Retry-After to honour"},
		{"retry-after as a date", raOnly, ev, Relation{Attempt: 2, Prev: prev(12, 0, reply(503, Header{"Retry-After", at(30).Format(http.TimeFormat)}))}, 5, auth.Fail, "asked Retry-After: 30 s"},
	} {
		got, reason := judge(c.ch, c.f, c.rel, at(c.at))
		if got != c.result || !strings.Contains(reason, c.reason) {
			t.Errorf("%s: %s %q, want %s with %q", c.name, got, reason, c.result, c.reason)
		}
	}
}

func TestDeliveryFactsAndValidation(t *testing.T) {
	c := Config{Default: Reply{Status: 200}, Event: &Source{FromHeader, "X-GitHub-Delivery"},
		Checks: []Check{{Kind: KindOrder, Order: &OrderSpec{Source{FromBody, "/created"}, "time"}, Group: &Source{FromBody, "/obj"}}}}
	if err := c.Validate(time.Minute); err != nil {
		t.Fatal(err)
	}
	f := c.DeliveryFacts(Request{Headers: []Header{{"x-github-delivery", "d-1"}}, Body: []byte(`{"created":1730548800,"obj":"cus_1"}`)})
	if f == nil || f.Event != "d-1" || f.Order == nil || *f.Order != 1730548800000 || f.Group != "cus_1" ||
		f.OrderDef != "body /created as time by body /obj" {
		t.Fatalf("facts %+v", f)
	}
	// Header names in a definition are compared without case.
	h := Config{Default: Reply{Status: 200}, Checks: []Check{{Kind: KindOrder, Order: &OrderSpec{Source{FromHeader, "X-Seq"}, "number"}}}}
	if f := h.DeliveryFacts(Request{Headers: []Header{{"x-seq", "3"}}}); f.OrderDef != "header x-seq as number" {
		t.Fatalf("definition %q", f.OrderDef)
	}
	f = c.DeliveryFacts(Request{Body: []byte(`{"created":"2026-11-02T12:00:00Z"}`)})
	if f.Missing == "" || f.Order == nil || *f.Order != float64(time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC).UnixMilli()) {
		t.Fatalf("facts without an id %+v", f)
	}
	if f := c.DeliveryFacts(Request{Body: []byte(`{"created":"soon"}`)}); f.OrderErr == "" {
		t.Fatalf("an unreadable time: %+v", f)
	}
	if (Config{Default: Reply{Status: 200}}).DeliveryFacts(Request{}) != nil {
		t.Fatal("facts for an endpoint without delivery checks")
	}
	for _, bad := range []Config{
		{Default: Reply{Status: 200}, Event: &Source{"query", "x"}},
		{Default: Reply{Status: 200}, Event: &Source{FromBody, "id"}},
		{Default: Reply{Status: 200}, Checks: []Check{{Kind: KindDuplicates, Enforce: true}}},
		{Default: Reply{Status: 200}, Checks: []Check{{Kind: KindOrder}}},
		{Default: Reply{Status: 200}, Checks: []Check{{Kind: KindOrder, Order: &OrderSpec{Source{FromBody, "/n"}, "date"}}}},
		{Default: Reply{Status: 200}, Checks: []Check{{Kind: KindBackoff, Factor: 0.5, FirstS: 1}}},
		{Default: Reply{Status: 200}, Checks: []Check{{Kind: KindBackoff, FirstS: 10, MaxS: 5}}},
		{Default: Reply{Status: 200}, Checks: []Check{{Kind: KindBackoff, RetryAfter: new(bool)}}},
	} {
		if err := bad.Validate(time.Minute); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	b := Check{Kind: KindBackoff, FirstS: 5, MaxS: 3600, MaxAttempts: 8}
	if got := b.Summary(); got != "5 s × 2, at most 3600 s, 8 attempts, ± 25 %, honours Retry-After · record only" {
		t.Errorf("summary %q", got)
	}
}
