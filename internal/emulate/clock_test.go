package emulate

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

var start = time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)

// at is a moment s seconds after start.
func at(s float64) time.Time { return start.Add(time.Duration(s * float64(time.Second))) }

// run decides requests in order against c, carrying the state, and gives
// each one's choice.
type step struct {
	at      float64
	counter int64
	attempt int
}

func run(c Config, steps []step) []Choice {
	state := ""
	var out []Choice
	for _, s := range steps {
		ch, next := c.Decide(ChooseIn{Counter: s.counter, At: at(s.at), State: state, Attempt: s.attempt})
		state = next
		out = append(out, ch)
	}
	return out
}

func one(w Condition) Config {
	return Config{Default: Reply{Status: 200}, Rules: []Rule{{When: w, Do: ActReply, Reply: Reply{Status: 503}}}}
}

// TestClockConditions: each condition kind at its edges; RetryAfter counts
// whole seconds, rounded up, to the end of the condition.
func TestClockConditions(t *testing.T) {
	type want struct {
		at      float64
		counter int64
		attempt int
		rule    int
		retry   int
	}
	for _, c := range []struct {
		name string
		when Condition
		w    []want
	}{
		{"first 30 s", Condition{Kind: WhenWindow, ToS: 30}, []want{
			{at: 0, rule: 1, retry: 30}, {at: 12.4, rule: 1, retry: 18}, {at: 29.9, rule: 1, retry: 1},
			{at: 30, rule: 0}, {at: 300, rule: 0}}},
		{"30–90 s", Condition{Kind: WhenWindow, FromS: 30, ToS: 90}, []want{
			{at: 0}, {at: 29.999}, {at: 30, rule: 1, retry: 60}, {at: 89.5, rule: 1, retry: 1}, {at: 90}}},
		{"from 60 s on", Condition{Kind: WhenWindow, FromS: 60}, []want{
			{at: 0}, {at: 59.999}, {at: 60, rule: 1}, {at: 86400, rule: 1}}},
		{"down 10 s of every 30 s", Condition{Kind: WhenFlap, Down: 10, Up: 20, Unit: "s"}, []want{
			{at: 0, rule: 1, retry: 10}, {at: 9.9, rule: 1, retry: 1}, {at: 10}, {at: 29.9},
			{at: 30, rule: 1, retry: 10}, {at: 41}, {at: 65, rule: 1, retry: 5}}},
		{"down 3 of every 8 requests", Condition{Kind: WhenFlap, Down: 3, Up: 5, Unit: "requests"}, []want{
			{counter: 1, rule: 1}, {counter: 3, rule: 1}, {counter: 4}, {counter: 8}, {counter: 9, rule: 1}, {counter: 11, rule: 1}, {counter: 12}}},
		{"over 2 in 10 s: 30 s", Condition{Kind: WhenRate, N: 2, PerS: 10, BlockS: 30}, []want{
			{at: 0}, {at: 1}, {at: 2, rule: 1, retry: 30}, // the third within 10 s trips it
			{at: 31.5, rule: 1, retry: 1}, // blocked until 32
			{at: 32}, {at: 33},            // the count starts again
			{at: 50}, {at: 61}, {at: 72}, // never more than 2 within 10 s
		}},
		{"first 2 attempts of each event", Condition{Kind: WhenAttempts, N: 2}, []want{
			{attempt: 1, rule: 1}, {attempt: 2, rule: 1}, {attempt: 3}, {attempt: 0, rule: 1}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.when.Label(); got != c.name {
				t.Errorf("label %q", got)
			}
			if err := one(c.when).Validate(wait); err != nil {
				t.Fatal(err)
			}
			var steps []step
			for _, w := range c.w {
				steps = append(steps, step{w.at, max(w.counter, 1), w.attempt})
			}
			for i, ch := range run(one(c.when), steps) {
				w := c.w[i]
				if ch.Rule != w.rule || ch.RetryAfter != w.retry {
					t.Errorf("at %g s, request %d, attempt %d: rule %d Retry-After %d; want rule %d Retry-After %d",
						w.at, w.counter, w.attempt, ch.Rule, ch.RetryAfter, w.rule, w.retry)
				}
			}
		})
	}
}

// TestClockStartsAtTheFirstRequest: the start is the first decided request;
// Elapsed counts from it; a cleared state starts the clock again.
func TestClockStartsAtTheFirstRequest(t *testing.T) {
	c := one(Condition{Kind: WhenWindow, ToS: 5})
	ch, state := c.Decide(ChooseIn{Counter: 1, At: at(100)})
	if ch.Rule != 1 || ch.Elapsed != 0 || ParseState(state).StartTime() != at(100) {
		t.Fatalf("first request: %+v, state %s", ch, state)
	}
	ch, state = c.Decide(ChooseIn{Counter: 2, At: at(106.5), State: state})
	if ch.Rule != 0 || ch.Elapsed != 6500*time.Millisecond || ParseState(state).StartTime() != at(100) {
		t.Fatalf("6.5 s later: %+v, state %s", ch, state)
	}
	// A request decided before the start it reads (arrivals race to the
	// lock) is at 0 s, not before.
	if ch, _ = c.Decide(ChooseIn{Counter: 3, At: at(99), State: state}); ch.Elapsed != 0 || ch.Rule != 1 {
		t.Fatalf("an earlier arrival: %+v", ch)
	}
	if ch, state = c.Decide(ChooseIn{Counter: 1, At: at(200), State: ""}); ch.Rule != 1 || ParseState(state).StartTime() != at(200) {
		t.Fatalf("after a reset: %+v, state %s", ch, state)
	}
	if (State{}).String() != "" || !ParseState("").StartTime().IsZero() || !ParseState("{broken").StartTime().IsZero() {
		t.Fatal("an empty or broken state is a fresh one")
	}
}

// TestClockRulesCompose: rules by time and state sit in the ordered list
// with the others; the first that holds wins, and a rate rule counts every
// request, also those an earlier rule took.
func TestClockRulesCompose(t *testing.T) {
	c := Config{Default: Reply{Status: 200}, Rules: []Rule{
		{When: Condition{Kind: WhenWindow, ToS: 5}, Do: ActReply, Reply: Reply{Status: 503}},
		{When: Condition{Kind: WhenEvery, N: 2}, Do: ActDrop},
		{When: Condition{Kind: WhenRate, N: 4, PerS: 60, BlockS: 10}, Do: ActReply, Reply: Reply{Status: 429}},
	}}
	got := run(c, []step{{0, 1, 1}, {1, 2, 1}, {6, 3, 1}, {7, 4, 1}, {8, 5, 1}, {9, 6, 1}, {20, 7, 1}})
	var rules []int
	for _, ch := range got {
		rules = append(rules, ch.Rule)
	}
	// 1-2: the window; 3: default (odd); 4: every 2nd; 5: the 5th within
	// 60 s trips the rate; 6: every 2nd comes first, although blocked;
	// 7: the block (8–18 s) is over.
	if want := []int{1, 1, 0, 2, 3, 2, 0}; !equal(rules, want) {
		t.Fatalf("rules %v, want %v", rules, want)
	}
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRuleStateFollowsItsRule: editing or moving a rate rule drops its own
// state only; the clock and the other rules' state stay.
func TestRuleStateFollowsItsRule(t *testing.T) {
	rate := func(n int64) Rule {
		return Rule{When: Condition{Kind: WhenRate, N: n, PerS: 60, BlockS: 60}, Do: ActReply, Reply: Reply{Status: 429}}
	}
	c := Config{Default: Reply{Status: 200}, Rules: []Rule{rate(2), rate(3)}}
	state := ""
	for i := range 2 {
		_, state = c.Decide(ChooseIn{Counter: int64(i + 1), At: at(float64(i)), State: state})
	}
	before := ParseState(state)
	if len(before.Rules) != 2 {
		t.Fatalf("state %s, want two rules", state)
	}
	edited := Config{Default: c.Default, Rules: []Rule{rate(2), rate(5)}}
	ch, state := edited.Decide(ChooseIn{Counter: 3, At: at(2), State: state})
	after := ParseState(state)
	k1, k2 := stateKey(0, rate(2).When), stateKey(1, rate(5).When)
	if ch.Rule != 1 || after.Start != before.Start || after.Rules[k1].BlockUntil == 0 || len(after.Rules[k2].Times) != 1 || len(after.Rules) != 2 {
		t.Fatalf("after editing rule 2: %+v, state %s", ch, state)
	}
	// Rule 1 moved below a new rule: its state goes with its position.
	moved := Config{Default: c.Default, Rules: []Rule{{When: Condition{Kind: WhenFirst, N: 1}, Do: ActDrop}, rate(2)}}
	_, state = moved.Decide(ChooseIn{Counter: 4, At: at(3), State: state})
	if st := ParseState(state); len(st.Rules) != 1 || st.Rules[stateKey(1, rate(2).When)].BlockUntil != 0 {
		t.Fatalf("after moving rule 1: %s", state)
	}
	// Without rules by state only the start is kept.
	_, state = Config{Default: c.Default}.Decide(ChooseIn{Counter: 5, At: at(4), State: state})
	if state != `{"start":`+strconv.FormatInt(start.UnixMilli(), 10)+`}` {
		t.Fatalf("no rate rules: %s", state)
	}
}

// TestClockValidation: impossible conditions are refused with a message
// that names what is wrong.
func TestClockValidation(t *testing.T) {
	for _, c := range []struct {
		when Condition
		want string
	}{
		{Condition{Kind: WhenWindow, FromS: 30, ToS: 30}, "to_s must be above from_s"},
		{Condition{Kind: WhenWindow, FromS: 30, ToS: 10}, "to_s must be above from_s"},
		{Condition{Kind: WhenWindow, FromS: -1}, "from 0 to 604800 s"},
		{Condition{Kind: WhenWindow, ToS: 604801}, "from 0 to 604800 s"},
		{Condition{Kind: WhenFlap, Down: 0, Up: 5, Unit: "s"}, "above 0"},
		{Condition{Kind: WhenFlap, Down: 5, Up: 0, Unit: "s"}, "above 0"},
		{Condition{Kind: WhenFlap, Down: 5, Up: 5}, `"s" or "requests"`},
		{Condition{Kind: WhenFlap, Down: 604801, Up: 5, Unit: "s"}, "at most 604800 s"},
		{Condition{Kind: WhenFlap, Down: 1.5, Up: 5, Unit: "requests"}, "whole requests"},
		{Condition{Kind: WhenRate, N: 0, PerS: 10, BlockS: 10}, "from 1 to 1000"},
		{Condition{Kind: WhenRate, N: 1001, PerS: 10, BlockS: 10}, "from 1 to 1000"},
		{Condition{Kind: WhenRate, N: 5, PerS: 0, BlockS: 10}, "per_s and block_s are above 0"},
		{Condition{Kind: WhenRate, N: 5, PerS: 10, BlockS: 0}, "per_s and block_s are above 0"},
		{Condition{Kind: WhenRate, N: 5, PerS: 10, BlockS: 604801}, "per_s and block_s are above 0"},
		{Condition{Kind: WhenAttempts, N: 0}, "from 1 to 1000"},
		{Condition{Kind: WhenAttempts, N: 1001}, "from 1 to 1000"},
	} {
		err := one(c.when).Validate(wait)
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "rule 1") {
			t.Errorf("%+v: %v, want rule 1 and %q", c.when, err, c.want)
		}
	}
	for _, ok := range []Condition{
		{Kind: WhenWindow, FromS: 0, ToS: 0.5}, {Kind: WhenWindow, FromS: 604800},
		{Kind: WhenFlap, Down: 0.5, Up: 1.5, Unit: "s"}, {Kind: WhenFlap, Down: 1, Up: 1, Unit: "requests"},
		{Kind: WhenRate, N: 1000, PerS: 604800, BlockS: 0.5}, {Kind: WhenAttempts, N: 1},
	} {
		if err := one(ok).Validate(wait); err != nil {
			t.Errorf("%+v refused: %v", ok, err)
		}
	}
}

// TestClockLabels: the words the register, the rules list and reports use.
func TestClockLabels(t *testing.T) {
	for w, want := range map[Condition]string{
		{Kind: WhenWindow, FromS: 0.5, ToS: 1.5}:          "0.5–1.5 s",
		{Kind: WhenWindow, FromS: 2}:                      "from 2 s on",
		{Kind: WhenFlap, Down: 0.25, Up: 0.75, Unit: "s"}: "down 0.25 s of every 1 s",
		{Kind: WhenRate, N: 1, PerS: 0.5, BlockS: 1}:      "over 1 in 0.5 s: 1 s",
		{Kind: WhenAttempts, N: 1}:                        "first attempt of each event",
	} {
		if got := w.Label(); got != want {
			t.Errorf("%+v: %q, want %q", w, got, want)
		}
	}
}

// TestClockOutcomeNote: a rule by time or state records the time since the
// start (also 0) and a rate block's end; other rules record neither.
func TestClockOutcomeNote(t *testing.T) {
	c := Config{Default: Reply{Status: 200}, Rules: []Rule{
		{When: Condition{Kind: WhenRate, N: 1, PerS: 10, BlockS: 2}, Do: ActReply, Reply: Reply{Status: 429}},
		{When: Condition{Kind: WhenEvery, N: 1}, Do: ActReply, Reply: Reply{Status: 500}},
	}}
	ch, state := c.Decide(ChooseIn{Counter: 1, At: at(0)})
	var o Outcome
	ch.Note(&o, c)
	if ch.Rule != 2 || o.ElapsedMs != nil || o.BlockUntil != "" {
		t.Fatalf("every 1st: %+v %+v", ch, o)
	}
	ch, _ = c.Decide(ChooseIn{Counter: 2, At: at(1.25), State: state})
	o = Outcome{Source: FromRule, Rule: ch.Rule, Label: ch.Label}
	ch.Note(&o, c)
	if ch.Rule != 1 || o.ElapsedMs == nil || *o.ElapsedMs != 1250 || o.BlockUntil != "2026-11-03T10:00:03.250Z" {
		t.Fatalf("rate: %+v %s", ch, o.String())
	}
	if got := o.Why(); got != "rule 1 · over 1 in 10 s: 2 s · at 1.2 s" {
		t.Fatalf("why %q", got)
	}
	ch, _ = one(Condition{Kind: WhenWindow, ToS: 1}).Decide(ChooseIn{Counter: 1, At: at(0)})
	o = Outcome{}
	ch.Note(&o, one(Condition{Kind: WhenWindow, ToS: 1}))
	if o.ElapsedMs == nil || *o.ElapsedMs != 0 || !strings.Contains(o.String(), `"elapsed_ms":0`) {
		t.Fatalf("at the start: %s", o.String())
	}
}
