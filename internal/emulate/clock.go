package emulate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"time"
)

// Conditions by time and state: they read the endpoint's clock (the start,
// the arrival of the first request that reached the rules after a reset),
// keep state of their own (rate), or the request's attempt of its event.
const (
	WhenWindow   = "window"   // from FromS to ToS seconds after the start (no ToS: for ever)
	WhenFlap     = "flap"     // the down part of a repeating down/up cycle, in seconds or requests
	WhenRate     = "rate"     // more than N requests within PerS seconds: then for BlockS seconds
	WhenAttempts = "attempts" // the first N attempts of each event
)

// MaxClock bounds a condition's times: a week.
const MaxClock = 7 * 24 * 3600

// State is an endpoint's rule state, kept in its row next to the counter:
// the start (unix ms; 0 before the first request after a reset) and the
// rate rules' windows, each keyed by its position and condition, so that
// editing or moving a rule drops its own state only.
type State struct {
	Start int64                 `json:"start,omitempty"`
	Rules map[string]*RateState `json:"rules,omitempty"`
}

// RateState is a rate rule's recent requests (unix ms) and its block's end.
type RateState struct {
	Times      []int64 `json:"times,omitempty"`
	BlockUntil int64   `json:"block_until,omitempty"`
}

// ParseState reads a stored state ("" or broken: a fresh one).
func ParseState(s string) State {
	var st State
	if s != "" {
		json.Unmarshal([]byte(s), &st)
	}
	return st
}

func (st State) String() string {
	if st.Start == 0 && len(st.Rules) == 0 {
		return ""
	}
	b, _ := json.Marshal(st)
	return string(b)
}

// StartTime is the start, or the zero time before the first request.
func (st State) StartTime() time.Time {
	if st.Start == 0 {
		return time.Time{}
	}
	return time.UnixMilli(st.Start).UTC()
}

// ChooseIn is what a decision reads: the request's number since the reset,
// its arrival, the endpoint's state, and its attempt of its event (1 for a
// first delivery).
type ChooseIn struct {
	Counter int64
	At      time.Time
	State   string
	Attempt int
}

// Clocked reports a condition by time or state, whose outcome records the
// time since the start.
func (c Condition) Clocked() bool {
	switch c.Kind {
	case WhenWindow, WhenFlap, WhenRate, WhenAttempts:
		return true
	}
	return false
}

// Note records on o what a rule by time or state read: the time since the
// start and a rate rule's block end.
func (ch Choice) Note(o *Outcome, c Config) {
	if ch.Rule < 1 || ch.Rule > len(c.Rules) || !c.Rules[ch.Rule-1].When.Clocked() {
		return
	}
	ms := ch.Elapsed.Milliseconds()
	o.ElapsedMs = &ms
	if ch.BlockUntil > 0 {
		o.BlockUntil = time.UnixMilli(ch.BlockUntil).UTC().Format("2006-01-02T15:04:05.000Z")
	}
}

// stateKey names a rule's state: its position and its condition.
func stateKey(i int, c Condition) string {
	b, _ := json.Marshal(c)
	s := sha256.Sum256(b)
	return strconv.Itoa(i) + ":" + hex.EncodeToString(s[:6])
}

// Decide picks the first rule whose condition holds, else the default
// reply, and returns the state to keep. The clock starts at the first
// request that reaches the rules; every rate rule counts every request,
// whichever rule takes it.
func (c Config) Decide(in ChooseIn) (Choice, string) {
	st := ParseState(in.State)
	if st.Start == 0 {
		st.Start = in.At.UnixMilli()
	}
	at := in.At.UnixMilli()
	elapsed := time.Duration(max(at-st.Start, 0)) * time.Millisecond
	rules := map[string]*RateState{}
	holds := make([]bool, len(c.Rules))
	until := make([]time.Duration, len(c.Rules)) // how long the condition still holds
	block := make([]int64, len(c.Rules))
	for i, r := range c.Rules {
		w := r.When
		switch w.Kind {
		case WhenWindow:
			t := elapsed.Seconds()
			holds[i] = t >= w.FromS && (w.ToS == 0 || t < w.ToS)
			if holds[i] && w.ToS > 0 {
				until[i] = seconds(w.ToS) - elapsed
			}
		case WhenFlap:
			cycle := w.Down + w.Up
			if w.Unit == "requests" {
				pos := math.Mod(float64(in.Counter-1), cycle)
				holds[i] = pos < w.Down
			} else {
				pos := math.Mod(elapsed.Seconds(), cycle)
				holds[i] = pos < w.Down
				if holds[i] {
					until[i] = seconds(w.Down - pos)
				}
			}
		case WhenRate:
			k := stateKey(i, w)
			rs := st.Rules[k]
			if rs == nil {
				rs = &RateState{}
			}
			rules[k] = rs
			if at < rs.BlockUntil {
				holds[i], block[i] = true, rs.BlockUntil
				until[i] = time.Duration(rs.BlockUntil-at) * time.Millisecond
				break
			}
			rs.BlockUntil = 0
			cut := at - int64(w.PerS*1000)
			kept := rs.Times[:0]
			for _, t := range rs.Times {
				if t > cut {
					kept = append(kept, t)
				}
			}
			rs.Times = append(kept, at)
			if int64(len(rs.Times)) > w.N {
				// Tripped: blocked from this request on; the count starts
				// again after the block.
				rs.BlockUntil, rs.Times = at+int64(w.BlockS*1000), nil
				holds[i], block[i] = true, rs.BlockUntil
				until[i] = seconds(w.BlockS)
			}
		case WhenAttempts:
			holds[i] = int64(max(in.Attempt, 1)) <= w.N
		default:
			holds[i] = w.holds(in.Counter)
		}
	}
	st.Rules = nil
	if len(rules) > 0 {
		st.Rules = rules
	}
	ch := Choice{Do: ActReply, Reply: c.Default, Elapsed: elapsed}
	for i, r := range c.Rules {
		if holds[i] {
			ch = Choice{Rule: i + 1, Label: r.When.Label(), Do: r.Do, Reply: r.Reply, Elapsed: elapsed, BlockUntil: block[i]}
			if until[i] > 0 {
				ch.RetryAfter = int(math.Ceil(until[i].Seconds()))
				ch.RetryAfter = max(ch.RetryAfter, 1)
			}
			break
		}
	}
	return ch, st.String()
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// validateClock checks a condition by time or state; times at most a week.
func validateClock(w Condition) error {
	within := func(v float64) bool { return v >= 0 && v <= MaxClock }
	switch w.Kind {
	case WhenWindow:
		if !within(w.FromS) || !within(w.ToS) {
			return errors.New("the window's times are from 0 to 604800 s (a week)")
		}
		if w.ToS != 0 && w.ToS <= w.FromS {
			return errors.New("the window ends before it starts: to_s must be above from_s")
		}
	case WhenFlap:
		if w.Unit != "s" && w.Unit != "requests" {
			return errors.New(`a flap counts in "s" or "requests"`)
		}
		if w.Down <= 0 || w.Up <= 0 {
			return errors.New("a flap's down and up parts are above 0")
		}
		if w.Unit == "s" && (!within(w.Down) || !within(w.Up)) {
			return errors.New("a flap's parts are at most 604800 s (a week)")
		}
		if w.Unit == "requests" && (w.Down != math.Trunc(w.Down) || w.Up != math.Trunc(w.Up) || w.Down > 1000000 || w.Up > 1000000) {
			return errors.New("a flap in requests counts whole requests, at most 1000000")
		}
	case WhenRate:
		if w.N < 1 || w.N > 1000 {
			return errors.New("a rate's n is from 1 to 1000")
		}
		if w.PerS <= 0 || w.BlockS <= 0 || !within(w.PerS) || !within(w.BlockS) {
			return errors.New("a rate's per_s and block_s are above 0 and at most 604800 s (a week)")
		}
	case WhenAttempts:
		if w.N < 1 || w.N > 1000 {
			return errors.New("attempts' n is from 1 to 1000")
		}
	}
	return nil
}

// clockLabel names a condition by time or state: "first 30 s", "30–90 s",
// "from 60 s on", "down 10 s of every 30 s", "over 5 in 10 s: 30 s",
// "first 2 attempts of each event".
func clockLabel(w Condition) string {
	n := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	switch w.Kind {
	case WhenWindow:
		switch {
		case w.ToS == 0:
			return "from " + n(w.FromS) + " s on"
		case w.FromS == 0:
			return "first " + n(w.ToS) + " s"
		}
		return n(w.FromS) + "–" + n(w.ToS) + " s"
	case WhenFlap:
		if w.Unit == "requests" {
			return "down " + n(w.Down) + " of every " + n(w.Down+w.Up) + " requests"
		}
		return "down " + n(w.Down) + " s of every " + n(w.Down+w.Up) + " s"
	case WhenRate:
		return "over " + strconv.FormatInt(w.N, 10) + " in " + n(w.PerS) + " s: " + n(w.BlockS) + " s"
	case WhenAttempts:
		if w.N == 1 {
			return "first attempt of each event"
		}
		return "first " + strconv.FormatInt(w.N, 10) + " attempts of each event"
	}
	return w.Kind
}
