package emulate

import (
	"strconv"
	"strings"
	"time"

	"github.com/petroladkin/hookwitness/internal/auth"
)

// A behaviour in words, as the behaviour page lists it and a report's setup
// section repeats it. Nothing here reads a secret's value: a secret is
// named, never shown.

// Summary describes a check after its label: what a required header must
// match, or for a verdict check its secret, its parameters and whether it
// refuses.
func (ch Check) Summary() string {
	if !ch.GivesVerdict() {
		return ch.matchLabel() + " · else " + ch.Refusal()
	}
	parts := []string{"secret " + ch.Secret}
	if ch.kind() == KindSchema {
		parts = nil
		if ch.Select != nil {
			for _, m := range ch.Select.Map {
				parts = append(parts, strconv.Quote(m.Value)+" → "+m.Schema)
			}
			other := "other values not checked"
			if d := ch.Select.Default; len(d) > 0 {
				other = "other values → " + strings.Join(d, " or ")
			}
			parts = []string{strings.Join(parts, ", "), other}
		}
	}
	switch ch.kind() {
	case KindBasic:
		parts = append(parts, "user "+ch.User)
	case KindHMAC:
		parts = append(parts, ch.Algo+" "+ch.Encoding)
	case KindStripe, KindStandard:
		if ch.IgnoreTimestamp {
			parts = append(parts, "any timestamp")
		} else {
			parts = append(parts, "tolerance "+strconv.FormatInt(ch.tolerance(), 10)+" s")
		}
	}
	if ch.Enforce {
		parts = append(parts, "enforced · else "+ch.Refusal())
	} else {
		parts = append(parts, "record only")
	}
	return strings.Join(parts, " · ")
}

// Refusal says how a check refuses a request.
func (ch Check) Refusal() string {
	switch {
	case ch.Error != nil:
		return strconv.Itoa(ch.Error.Status) + " (own reply)"
	case ch.kind() == KindSchema:
		return "400 listing the problems"
	case ch.GivesVerdict():
		return "401"
	}
	return "400 naming the header"
}

func (ch Check) matchLabel() string {
	switch ch.Match {
	case MatchEquals:
		return "equals " + strconv.Quote(ch.Value)
	case MatchRegex:
		return "matches /" + ch.Value + "/"
	}
	return "is present"
}

func (ch Check) tolerance() int64 {
	if ch.ToleranceS > 0 {
		return ch.ToleranceS
	}
	return auth.DefaultTolerance
}

// Action says what a rule does: "503", "503 after 2.0 s+", "time out",
// "drop the connection".
func (r Rule) Action() string {
	switch r.Do {
	case ActTimeout:
		return "time out"
	case ActDrop:
		return "drop the connection"
	}
	if r.Reply.DelayMs > 0 || r.Reply.JitterMs > 0 {
		return strconv.Itoa(r.Reply.Status) + " after " + Duration(r.Reply.DelayMs) + "+"
	}
	return strconv.Itoa(r.Reply.Status)
}

// DelayLabel says how long a reply waits: "none", "2.0 s", "2.0 s + up to
// 500 ms at random".
func (r Reply) DelayLabel() string {
	switch {
	case r.DelayMs == 0 && r.JitterMs == 0:
		return "none"
	case r.JitterMs == 0:
		return Duration(r.DelayMs)
	}
	return Duration(r.DelayMs) + " + up to " + Duration(r.JitterMs) + " at random"
}

// Duration is milliseconds in words: "250 ms", "2.0 s".
func Duration(msec int64) string {
	d := time.Duration(msec) * time.Millisecond
	if d < time.Second {
		return strconv.FormatInt(msec, 10) + " ms"
	}
	return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + " s"
}

// SecretKindTitle names a secret kind.
func SecretKindTitle(kind string) string {
	if kind == SecretToken {
		return "token or password"
	}
	return "signing key"
}
