package emulate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/petroladkin/hookwitness/internal/auth"
	"github.com/petroladkin/hookwitness/internal/jsonptr"
	"github.com/petroladkin/hookwitness/internal/schema"
)

// Delivery checks judge how a sender delivers rather than one request:
// duplicates of an event, the order of events, the gaps between retries.
// They depend on earlier kept requests, so their verdicts are finished
// where the request is stored (JudgeDelivery), and they never refuse.
const (
	KindDuplicates = "duplicates"
	KindOrder      = "order"
	KindBackoff    = "backoff"
)

// DeliveryKinds are the delivery check kinds.
var DeliveryKinds = []string{KindDuplicates, KindOrder, KindBackoff}

// Delivery check defaults.
const (
	DefaultFactor    = 2.0
	DefaultTolerance = 25 // percent
	maxEventID       = 1024
)

// Source is where a request carries a value: a header, or a JSON pointer
// into its body.
type Source struct {
	From string `json:"from"` // FromHeader or FromBody
	Key  string `json:"key"`  // the header name or the JSON pointer
}

// OrderSpec is the order check's value: a sequence number or a time.
type OrderSpec struct {
	Source
	As string `json:"as"` // "number" or "time"
}

// EventPresets are the event id sources of the providers the signature
// checks know, by preset name.
var EventPresets = []struct {
	Name   string
	Source Source
	Kind   string // the signature check kind that suggests it
}{
	{"GitHub", Source{FromHeader, "X-GitHub-Delivery"}, KindGitHub},
	{"Stripe", Source{FromBody, "/id"}, KindStripe},
	{"Standard Webhooks", Source{FromHeader, "webhook-id"}, KindStandard},
	{"Svix", Source{FromHeader, "svix-id"}, ""},
}

func (s Source) String() string {
	if s.From == FromHeader {
		return "header " + s.Key
	}
	return "body " + s.Key
}

func (s Source) validate(what string) error {
	switch s.From {
	case FromHeader:
		if !headerName.MatchString(s.Key) {
			return fmt.Errorf("%s: header name %q is not a valid HTTP header name", what, s.Key)
		}
	case FromBody:
		if _, err := jsonptr.Parse(s.Key); err != nil || s.Key == "" {
			return fmt.Errorf("%s: %q is not a JSON pointer such as /id", what, s.Key)
		}
	default:
		return fmt.Errorf("%s: from is header or body", what)
	}
	return nil
}

// read finds the source's value in a request: a header's first value, or a
// string or number at the pointer. found is false when it is not there; err
// names a body that is not JSON.
func (s Source) read(r Request) (value string, found bool, err error) {
	if s.From == FromHeader {
		vs := headerValues(r.Headers, s.Key)
		if len(vs) == 0 {
			return "", false, nil
		}
		return vs[0], true, nil
	}
	if msg := schema.BodyError(r.Body); msg != "" {
		return "", false, errors.New(msg)
	}
	dec := json.NewDecoder(bytes.NewReader(r.Body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return "", false, err
	}
	toks, err := jsonptr.Parse(s.Key)
	if err != nil {
		return "", false, nil
	}
	cur, ok := jsonptr.Get(doc, toks)
	if !ok {
		return "", false, nil
	}
	switch v := cur.(type) {
	case string:
		return v, true, nil
	case json.Number:
		return v.String(), true, nil
	}
	return "", false, nil
}

// IsDelivery reports a delivery check.
func (ch Check) IsDelivery() bool {
	k := ch.kind()
	return k == KindDuplicates || k == KindOrder || k == KindBackoff
}

func (ch Check) factor() float64 {
	if ch.Factor == 0 {
		return DefaultFactor
	}
	return ch.Factor
}

func (ch Check) tolerancePct() int {
	if ch.TolerancePct == 0 {
		return DefaultTolerance
	}
	return ch.TolerancePct
}

// honoursRetryAfter reports whether a backoff check judges Retry-After.
func (ch Check) honoursRetryAfter() bool { return ch.RetryAfter == nil || *ch.RetryAfter }

func (c Config) validateDelivery(ch Check) error {
	if ch.Enforce {
		return errors.New("a delivery check never refuses a request: what a receiver answers to a duplicate is the rules' job")
	}
	switch ch.kind() {
	case KindOrder:
		if ch.Order == nil {
			return errors.New("give the value to order by")
		}
		if err := ch.Order.validate("order"); err != nil {
			return err
		}
		if ch.Order.As != "number" && ch.Order.As != "time" {
			return errors.New(`order: as is "number" or "time"`)
		}
		if ch.Group != nil {
			if err := ch.Group.validate("group"); err != nil {
				return err
			}
		}
	case KindBackoff:
		switch {
		case ch.FirstS < 0 || ch.FirstS > 86400:
			return errors.New("first_s is 0-86400 s (0: Retry-After only)")
		case ch.Factor != 0 && (ch.Factor < 1 || ch.Factor > 100):
			return errors.New("factor is 1-100")
		case ch.MaxS < 0 || ch.MaxS > 7*86400 || (ch.MaxS > 0 && ch.MaxS < ch.FirstS):
			return errors.New("max_s is at least first_s and at most 7 days")
		case ch.MaxAttempts < 0 || ch.MaxAttempts > 1000:
			return errors.New("max_attempts is 0-1000 (0: no limit)")
		case ch.TolerancePct < 0 || ch.TolerancePct > 100:
			return errors.New("tolerance_pct is 0-100")
		case ch.FirstS == 0 && !ch.honoursRetryAfter():
			return errors.New("give first_s, or let it judge Retry-After")
		}
	}
	return nil
}

// Facts are what a request carries for the delivery checks, read before it
// is stored: its event, and the order check's value and group.
type Facts struct {
	Event    string // the event id; "" when ByBody or Missing
	ByBody   bool   // no event source: attempts are counted per body
	Missing  string // the event source is set and the request has no id: why
	HasOrder bool   // the endpoint has an order check
	Order    *float64
	OrderErr string // why there is no order value
	Group    string // the order check's partition ("" without one)
}

// DeliveryFacts reads a request's facts; nil when the endpoint has neither
// an event source nor a delivery check.
func (c Config) DeliveryFacts(r Request) *Facts {
	var order *Check
	any := c.Event != nil
	for i, ch := range c.Checks {
		if ch.IsDelivery() {
			any = true
			if ch.kind() == KindOrder && order == nil {
				order = &c.Checks[i]
			}
		}
	}
	if !any {
		return nil
	}
	f := &Facts{ByBody: c.Event == nil}
	if c.Event != nil {
		v, ok, err := c.Event.read(r)
		switch {
		case err != nil:
			f.Missing = "no event id: " + err.Error()
		case !ok || v == "":
			f.Missing = "no event id (" + c.Event.String() + " missing)"
		case len(v) > maxEventID:
			f.Missing = "no event id (" + c.Event.String() + " over 1 KiB)"
		default:
			f.Event = v
		}
	}
	if order != nil {
		f.HasOrder = true
		v, ok, err := order.Order.read(r)
		switch {
		case err != nil:
			f.OrderErr = err.Error()
		case !ok:
			f.OrderErr = "no value at " + order.Order.Key
		default:
			if n, perr := orderValue(v, order.Order.As); perr != nil {
				f.OrderErr = order.Order.Key + " " + perr.Error()
			} else {
				f.Order = &n
			}
		}
		if order.Group != nil {
			if g, ok, _ := order.Group.read(r); ok {
				f.Group = g
			}
		}
	}
	return f
}

// orderValue reads a sequence number, or a time as unix milliseconds
// (RFC 3339, or unix seconds; a number over 10^11 is milliseconds already).
func orderValue(v, as string) (float64, error) {
	if as == "time" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return float64(t.UnixMilli()), nil
		}
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, errors.New("is not a time (RFC 3339 or unix seconds)")
		}
		if n < 1e11 {
			n *= 1000
		}
		return n, nil
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, errors.New("is not a number")
	}
	return n, nil
}

// Attempt is an earlier kept attempt of the same event (or body).
type Attempt struct {
	No      int64
	At      time.Time
	Outcome Outcome
}

// end is when the attempt ended for its sender: the reply's delay, or how
// long a hold kept it, after its arrival.
func (a Attempt) end() time.Time {
	switch a.Outcome.Action {
	case ActTimeout, ActAbandoned:
		return a.At.Add(time.Duration(a.Outcome.WaitedMs) * time.Millisecond)
	case ActDrop:
		return a.At
	}
	return a.At.Add(time.Duration(a.Outcome.DelayMs) * time.Millisecond)
}

// Mark is the highest order value among earlier kept requests of the group
// that are other events, and the request that carried it.
type Mark struct {
	Value float64
	No    int64
}

// Relation is what the store finds about a request among the earlier kept
// ones, in the transaction that stores it.
type Relation struct {
	Attempt int      // 1-based among kept attempts of its event; 0 without an event
	Prev    *Attempt // the previous kept attempt
	High    *Mark    // the order check's high mark; nil: first in order
}

// JudgeDelivery finishes the delivery checks' verdicts in vs (as Verify
// left them, one per verdict-giving check in order) for a request received
// at with facts f and relation rel.
func (c Config) JudgeDelivery(vs []auth.Verdict, f Facts, rel Relation, at time.Time) {
	for i := range vs {
		n := vs[i].Check - 1
		if n < 0 || n >= len(c.Checks) || !c.Checks[n].IsDelivery() {
			continue
		}
		ch := c.Checks[n]
		r, reason := judge(ch, f, rel, at)
		vs[i].Result, vs[i].Reason = r, reason
	}
}

func judge(ch Check, f Facts, rel Relation, at time.Time) (auth.Result, string) {
	event := "the body"
	if !f.ByBody {
		event = f.Event
	}
	switch ch.kind() {
	case KindDuplicates:
		switch {
		case f.Missing != "":
			return auth.NotVerifiable, f.Missing
		case rel.Prev == nil:
			return auth.Pass, "first delivery of " + event
		case rel.Prev.Outcome.Good():
			return auth.Fail, fmt.Sprintf("duplicate of #%d, which was answered %d", rel.Prev.No, rel.Prev.Outcome.Status)
		}
		return auth.Pass, fmt.Sprintf("attempt %d of %s; #%d was %s", rel.Attempt, event, rel.Prev.No, answered(rel.Prev.Outcome))
	case KindOrder:
		switch {
		case f.OrderErr != "":
			return auth.NotVerifiable, f.OrderErr
		case f.Order == nil:
			return auth.NotVerifiable, "no order value"
		case rel.Prev != nil:
			// Late by nature: its order was judged on its first delivery.
			return auth.Pass, fmt.Sprintf("attempt %d of %s: judged on its first delivery", rel.Attempt, event)
		case rel.High == nil:
			return auth.Pass, "first in order"
		case *f.Order < rel.High.Value:
			return auth.Fail, fmt.Sprintf("out of order: %s after %s (#%d)", orderWord(*f.Order, ch), orderWord(rel.High.Value, ch), rel.High.No)
		}
		return auth.Pass, fmt.Sprintf("%s after %s (#%d)", orderWord(*f.Order, ch), orderWord(rel.High.Value, ch), rel.High.No)
	case KindBackoff:
		if f.Missing != "" {
			return auth.NotVerifiable, f.Missing
		}
		if rel.Prev == nil {
			return auth.NotChecked, "first attempt"
		}
		k := rel.Attempt
		if ch.MaxAttempts > 0 && k > ch.MaxAttempts {
			return auth.Fail, fmt.Sprintf("attempt %d: more than %d attempts", k, ch.MaxAttempts)
		}
		gap := at.Sub(rel.Prev.end())
		var ra time.Duration
		if ch.honoursRetryAfter() {
			ra = retryAfter(rel.Prev.Outcome, rel.Prev.end())
		}
		if ra > 0 && gap < ra-time.Second {
			return auth.Fail, fmt.Sprintf("attempt %d after %s; #%d asked Retry-After: %s", k, secs(gap), rel.Prev.No, secs(ra))
		}
		if ch.FirstS == 0 {
			if ra > 0 {
				return auth.Pass, fmt.Sprintf("attempt %d after %s; #%d asked Retry-After: %s", k, secs(gap), rel.Prev.No, secs(ra))
			}
			return auth.NotChecked, fmt.Sprintf("attempt %d: no Retry-After to honour", k)
		}
		want := ch.FirstS * math.Pow(ch.factor(), float64(k-2))
		if ch.MaxS > 0 {
			want = math.Min(want, ch.MaxS)
		}
		expect := time.Duration(want * float64(time.Second))
		if ra > expect {
			expect = ra // Retry-After wins when longer
		}
		tol := time.Duration(float64(expect) * float64(ch.tolerancePct()) / 100)
		words := fmt.Sprintf("attempt %d after %s, expected %s ± %d %%", k, secs(gap), secs(expect), ch.tolerancePct())
		if gap < expect-tol || gap > expect+tol {
			return auth.Fail, words
		}
		return auth.Pass, words
	}
	return auth.NotChecked, ""
}

// answered says what a reply did, for a duplicate's reason.
func answered(o Outcome) string {
	switch o.Action {
	case ActTimeout:
		return "held until a timeout"
	case ActDrop:
		return "dropped"
	case ActAbandoned:
		return "left by its sender"
	case ActRefused:
		return fmt.Sprintf("refused %d", o.Status)
	}
	if o.Status == 0 {
		return "not answered yet"
	}
	return fmt.Sprintf("answered %d", o.Status)
}

// retryAfter is the delay a reply's Retry-After asked for, from when the
// reply ended: seconds, or an HTTP date.
func retryAfter(o Outcome, end time.Time) time.Duration {
	for _, h := range o.Headers {
		if !strings.EqualFold(h.Name, "Retry-After") {
			continue
		}
		v := strings.TrimSpace(h.Value)
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
		if t, err := http.ParseTime(v); err == nil {
			return max(t.Sub(end), 0)
		}
	}
	return 0
}

func secs(d time.Duration) string {
	s := d.Seconds()
	if s == math.Trunc(s) && s < 1e6 {
		return strconv.FormatFloat(s, 'f', 0, 64) + " s"
	}
	return strconv.FormatFloat(s, 'f', 1, 64) + " s"
}

func orderWord(v float64, ch Check) string {
	if ch.Order != nil && ch.Order.As == "time" {
		return time.UnixMilli(int64(v)).UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}
