package emulate

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// Outcome sources: what decided the answer.
const (
	FromDefault   = "default"
	FromRule      = "rule"
	FromCheck     = "check"     // a request check refused it
	FromLimit     = "limit"     // the body was over the limit
	FromUnmatched = "unmatched" // no endpoint at that path
)

// Outcome actions besides ActReply, ActTimeout and ActDrop.
const (
	ActRefused = "refused" // a check failed; the refusal reply was sent
	ActError   = "error"   // the reply could not be rendered; 500 was sent
)

// maxRecordedBody bounds the reply body kept with the request.
const maxRecordedBody = 64 << 10

// Outcome is what Hookwitness did with a request, kept with it as evidence.
type Outcome struct {
	Source   string   `json:"source"`
	Rule     int      `json:"rule,omitempty"`  // 1-based, for FromRule
	Label    string   `json:"label,omitempty"` // the rule's condition
	Check    string   `json:"check,omitempty"` // the failed check
	Action   string   `json:"action"`
	Status   int      `json:"status,omitempty"` // 0 when nothing was sent
	Headers  []Header `json:"headers,omitempty"`
	Body     string   `json:"body,omitempty"` // first 64 KiB of the reply body
	Cut      bool     `json:"cut,omitempty"`  // Body is truncated
	DelayMs  int64    `json:"delay_ms,omitempty"`
	WaitedMs int64    `json:"waited_ms,omitempty"` // a timeout: how long the sender waited
	Error    string   `json:"error,omitempty"`
	Counter  int64    `json:"counter,omitempty"` // the request's number since the reset
}

// SetReply records the reply actually sent.
func (o *Outcome) SetReply(r Rendered) {
	o.Status, o.Headers = r.Status, r.Headers
	o.Body, o.Cut = string(r.Body), false
	if len(r.Body) > maxRecordedBody {
		o.Body, o.Cut = string(r.Body[:maxRecordedBody]), true
	}
}

// String is the stored form.
func (o Outcome) String() string {
	b, _ := json.Marshal(o)
	return string(b)
}

// ParseOutcome reads a stored outcome; empty (requests caught before
// outcomes were recorded) gives an Outcome with only the status.
func ParseOutcome(s string, status int) Outcome {
	var o Outcome
	if s == "" || json.Unmarshal([]byte(s), &o) != nil {
		return Outcome{Action: ActReply, Status: status}
	}
	return o
}

// Stamp is the short text on the exhibit: "500 sent", "timeout", "dropped",
// "400 refused".
func (o Outcome) Stamp() string {
	switch o.Action {
	case ActTimeout:
		return "timeout"
	case ActDrop:
		return "dropped"
	case ActRefused:
		return strconv.Itoa(o.Status) + " refused"
	}
	return strconv.Itoa(o.Status) + " sent"
}

// Why says what decided it: "rule 2 · every 3rd", "default reply",
// "check 1: header X-Token missing".
func (o Outcome) Why() string {
	switch o.Source {
	case FromRule:
		return fmt.Sprintf("rule %d · %s", o.Rule, o.Label)
	case FromCheck:
		return o.Check
	case FromLimit:
		return "body over the limit"
	case FromUnmatched:
		return "no endpoint at this path"
	case FromDefault:
		return "default reply"
	}
	return ""
}

// Good reports a 2xx answer; the strip marks the rest.
func (o Outcome) Good() bool {
	return (o.Action == ActReply) && o.Status >= 200 && o.Status < 300
}
