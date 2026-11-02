package emulate

import (
	"strings"
	"testing"
	"time"
)

func forwarding(rules ...Rule) Config {
	return Config{Default: Reply{Status: 202}, Rules: rules, Forward: &Forward{URL: "https://receiver.test:8443/hooks/in"}}
}

// TestForwardValidation: the target's URL, its timeout and the failure
// answer are checked when the behaviour is saved.
func TestForwardValidation(t *testing.T) {
	for _, ok := range []Forward{
		{URL: "http://127.0.0.1:3000/webhooks"},
		{URL: "https://receiver.test/", KeepHost: true, InsecureTLS: true, TimeoutMs: 100, OnFailure: OnFailureDefault},
		{URL: "https://receiver.test", TimeoutMs: 300000, OnFailure: OnFailureGateway},
	} {
		c := forwarding()
		c.Forward = &ok
		if err := c.Validate(5 * time.Minute); err != nil {
			t.Errorf("%+v refused: %v", ok, err)
		}
	}
	for f, want := range map[Forward]string{
		{URL: "ftp://receiver.test/x"}:                                   "http or https",
		{URL: "https://user:pw@receiver.test/x"}:                         "user name or password",
		{URL: "https://receiver.test/x?token=1"}:                         "must not have a query",
		{URL: "https://receiver.test/x?"}:                                "must not have a query",
		{URL: "https://receiver.test/x#top"}:                             "fragment",
		{URL: "https:///x"}:                                              "no host",
		{URL: "https://receiver.test/a b"}:                               "space",
		{URL: "https://receiver.test/x", TimeoutMs: 99}:                  "from 100 ms to 300000 ms",
		{URL: "https://receiver.test/x", TimeoutMs: 300001}:              "from 100 ms to 300000 ms",
		{URL: "https://receiver.test/x", TimeoutMs: 9223372036854775807}: "from 100 ms to 300000 ms",
		{URL: "https://receiver.test/x", TimeoutMs: 18446744073710}:      "from 100 ms to 300000 ms",
		{URL: "https://receiver.test/x", OnFailure: "mirror"}:            `on_failure is "gateway" or "default"`,
		{URL: ""}: "receiver:",
	} {
		c := forwarding()
		c.Forward = &f
		err := c.Validate(5 * time.Minute)
		if err == nil || !strings.Contains(err.Error(), want) || !strings.HasPrefix(err.Error(), "receiver: ") {
			t.Errorf("%+v: %v, want %q", f, err, want)
		}
	}
}

// TestForwardDecides: requests no rule takes are forwarded; a rule takes
// its requests as today, and the receiver gets them too only with the
// rule's switch on an endpoint that forwards.
func TestForwardDecides(t *testing.T) {
	c := forwarding(
		Rule{When: Condition{Kind: WhenEvery, N: 3}, Do: ActReply, Reply: Reply{Status: 500}},
		Rule{When: Condition{Kind: WhenEvery, N: 2}, Do: ActTimeout, Forward: true},
	)
	for n, want := range map[int64]struct {
		rule    int
		do      string
		forward bool
	}{1: {0, ActForward, true}, 2: {2, ActTimeout, true}, 3: {1, ActReply, false}, 5: {0, ActForward, true}} {
		ch := c.Choose(n)
		if ch.Rule != want.rule || ch.Do != want.do || ch.Forward != want.forward {
			t.Errorf("request %d: %+v, want %+v", n, ch, want)
		}
	}
	// Without a receiver the switch means nothing and the default replies.
	c.Forward = nil
	if ch := c.Choose(2); ch.Forward || ch.Do != ActTimeout {
		t.Errorf("a rule's switch without a receiver: %+v", ch)
	}
	if ch := c.Choose(1); ch.Forward || ch.Do != ActReply || ch.Reply.Status != 202 {
		t.Errorf("the default without a receiver: %+v", ch)
	}
}

// TestForwardWords: what the register, the request page and the reports
// say of a forwarded request, judged by what the sender got.
func TestForwardWords(t *testing.T) {
	note := func(n ForwardNote) *ForwardNote { n.Target = "https://receiver.test:8443/hooks/in"; return &n }
	for _, c := range []struct {
		o                Outcome
		stamp, why, mark string
		good, anomaly    bool
	}{
		{Outcome{Source: FromForward, Action: ActForward, Forward: note(ForwardNote{Pending: true})},
			"forwarding…", "forwarded to https://receiver.test:8443/hooks/in", "forwarding…", false, false},
		{Outcome{Source: FromForward, Action: ActForward, Status: 200, Forward: note(ForwardNote{Status: 200, Ms: 38})},
			"200 from receiver", "forwarded to https://receiver.test:8443/hooks/in", "receiver 200", true, false},
		{Outcome{Source: FromForward, Action: ActForward, Status: 409, Forward: note(ForwardNote{Status: 409})},
			"409 from receiver", "forwarded to https://receiver.test:8443/hooks/in", "receiver 409", false, true},
		{Outcome{Source: FromForward, Action: ActForward, Status: 502, Forward: note(ForwardNote{ErrorKind: "dial", Fallback: true})},
			"502 receiver down", "forwarded to https://receiver.test:8443/hooks/in · the receiver could not be reached", "receiver down", false, true},
		{Outcome{Source: FromForward, Action: ActForward, Status: 504, Forward: note(ForwardNote{ErrorKind: "timeout", Fallback: true})},
			"504 receiver timeout", "forwarded to https://receiver.test:8443/hooks/in · the receiver did not answer within the timeout", "receiver timeout", false, true},
		{Outcome{Source: FromForward, Action: ActForward, Status: 202, Forward: note(ForwardNote{ErrorKind: "reset", Fallback: true})},
			"202 default · receiver down", "forwarded to https://receiver.test:8443/hooks/in · the receiver closed the connection before its answer ended · the default reply was sent", "receiver down", true, true},
		{Outcome{Source: FromRule, Rule: 1, Label: "every 3rd", Action: ActReply, Status: 500, NotForwarded: true},
			"500 sent", "rule 1 · every 3rd · not forwarded", "not forwarded", false, true},
		{Outcome{Source: FromRule, Rule: 1, Label: "every 3rd", Action: ActReply, Status: 200, NotForwarded: true},
			"200 sent", "rule 1 · every 3rd · not forwarded", "not forwarded", true, true},
		{Outcome{Source: FromRule, Rule: 2, Label: "every 2nd", Action: ActTimeout, Forward: note(ForwardNote{Status: 200})},
			"timeout", "rule 2 · every 2nd · the receiver got it too", "receiver 200", false, true},
		{Outcome{Source: FromRule, Rule: 2, Label: "every 2nd", Action: ActReply, Status: 200, Forward: note(ForwardNote{ErrorKind: "dial"})},
			"200 sent", "rule 2 · every 2nd · the receiver got it too", "receiver down", true, true},
		{Outcome{Source: FromLoop, Action: ActReply, Status: 508},
			"508 sent", "the request came back on the server's own forward", "", false, true},
		{Outcome{Source: FromDefault, Action: ActReply, Status: 200}, "200 sent", "default reply", "", true, false},
	} {
		if got := c.o.Stamp(); got != c.stamp {
			t.Errorf("%s: stamp %q, want %q", c.o.String(), got, c.stamp)
		}
		if got := c.o.Why(); got != c.why {
			t.Errorf("%s: why %q, want %q", c.o.String(), got, c.why)
		}
		if got := c.o.ReceiverMark(); got != c.mark {
			t.Errorf("%s: mark %q, want %q", c.o.String(), got, c.mark)
		}
		if c.o.Good() != c.good || Anomalous(c.o, false, "", false) != c.anomaly {
			t.Errorf("%s: good %v anomaly %v, want %v %v", c.o.String(), c.o.Good(), Anomalous(c.o, false, "", false), c.good, c.anomaly)
		}
	}
	// A sender that left before the receiver answered got nothing.
	o := Outcome{Source: FromForward, Action: ActForward, Status: 200, Forward: note(ForwardNote{Status: 200, SenderLeftMs: 10000, Ms: 14200})}
	o.Abandon(10000)
	if o.Stamp() != "sender left" || !o.HeldUp() || o.Would != 200 || !Anomalous(o, false, "", false) {
		t.Errorf("sender left: %s %s", o.Stamp(), o.String())
	}
}

// TestForwardTarget: the timeout under the cap, and the names of the
// target for people and for logs.
func TestForwardTarget(t *testing.T) {
	f := &Forward{URL: "https://Receiver.test/hooks/%7Ein"}
	if f.Timeout(5*time.Minute) != DefaultForwardTimeout || f.Timeout(10*time.Second) != 10*time.Second {
		t.Fatalf("default timeout %v, capped %v", f.Timeout(5*time.Minute), f.Timeout(10*time.Second))
	}
	f.TimeoutMs = 90000
	if f.Timeout(5*time.Minute) != 90*time.Second || f.Timeout(time.Minute) != time.Minute {
		t.Fatalf("set timeout %v, capped %v", f.Timeout(5*time.Minute), f.Timeout(time.Minute))
	}
	if f.Where() != "https://Receiver.test/hooks/%7Ein" || f.Origin() != "https://Receiver.test:443" {
		t.Fatalf("where %q origin %q", f.Where(), f.Origin())
	}
	if got := f.Summary(5 * time.Minute); got != "Host of the target · timeout 90.0 s · 502/504 when it fails" {
		t.Fatalf("summary %q", got)
	}
	f.KeepHost, f.InsecureTLS, f.OnFailure = true, true, OnFailureDefault
	if got := f.Summary(5 * time.Minute); got != "Host as received · any certificate · timeout 90.0 s · the default reply when it fails" {
		t.Fatalf("summary %q", got)
	}
	for kind, status := range map[string]int{"dial": 502, "tls": 502, "timeout": 504, FailLoop: 508, FailBusy: 503, FailTooLarge: 502, FailUpgrade: 502, FailRefused: 502} {
		if GatewayStatus(kind) != status || strings.HasPrefix(FailureWords(kind), "the forward failed") {
			t.Errorf("%s: %d %q", kind, GatewayStatus(kind), FailureWords(kind))
		}
	}
}

// TestProtectAgainstShadowing: a Set-Cookie a browser could read as the
// login cookie is dropped (also nameless, as "=hookwitness_session=…");
// NoSniff overrides a third party's own X-Content-Type-Options.
func TestProtectAgainstShadowing(t *testing.T) {
	r := Rendered{Status: 200, Headers: []Header{
		{"Set-Cookie", "hookwitness_session=a; Path=/"},
		{"Set-Cookie", "=hookwitness_session=b; Path=/admin/"},
		{"set-cookie", " hookwitness_session =c"},
		{"Set-Cookie", "app=1; Path=/"},
		{"Set-Cookie", "=other=2"},
		{"X-Content-Type-Options", "foo"},
	}}
	got := r.Protect("hookwitness_session").NoSniff()
	var cookies, sniff []string
	for _, h := range got.Headers {
		switch {
		case strings.EqualFold(h.Name, "Set-Cookie"):
			cookies = append(cookies, h.Value)
		case strings.EqualFold(h.Name, "X-Content-Type-Options"):
			sniff = append(sniff, h.Value)
		}
	}
	if strings.Join(cookies, "|") != "app=1; Path=/|=other=2" || strings.Join(sniff, "|") != "nosniff" {
		t.Fatalf("cookies %q, nosniff %q", cookies, sniff)
	}
}
