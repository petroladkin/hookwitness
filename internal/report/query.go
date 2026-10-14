package report

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/petroladkin/hookwitness/internal/store"
)

// MaxPicked bounds a selection by request numbers.
const MaxPicked = 1000

// Query is a report as the UI's dialog and the API ask for it: format,
// which requests, masked headers, images or not.
type Query struct {
	Format    Format
	Selection store.Selection
	Words     string // the selection in words, for the report's header
	Mask      []string
	NoImages  bool
}

// FieldError is a parameter at fault and why.
type FieldError struct{ Field, Message string }

func (e *FieldError) Error() string { return e.Field + ": " + e.Message }

// ParseQuery reads format, scope (all, range with from/to, picked with
// pick), mask (repeated) and mask_other (comma-separated), images
// (shown, listed). Times are UTC: datetime-local values or RFC 3339.
func ParseQuery(q url.Values) (Query, error) {
	var rq Query
	bad := func(field, msg string) (Query, error) { return rq, &FieldError{field, msg} }
	f, err := ParseFormat(q.Get("format"))
	if err != nil {
		return bad("format", "choose Markdown, HTML or PDF")
	}
	rq.Format = f
	rq.NoImages = q.Get("images") == "listed"
	sel := &rq.Selection
	switch q.Get("scope") {
	case "all", "":
		rq.Words = "whole register"
	case "range":
		if sel.From, err = parseTime(q.Get("from")); err != nil {
			return bad("from", "the start "+err.Error())
		}
		if sel.To, err = parseTime(q.Get("to")); err != nil {
			return bad("to", "the end "+err.Error())
		}
		if sel.From.IsZero() && sel.To.IsZero() {
			return bad("from", "give the start, the end or both")
		}
		if !sel.From.IsZero() && !sel.To.IsZero() && sel.To.Before(sel.From) {
			return bad("to", "the end is before the start")
		}
		from, to := "the first request", "the last"
		if !sel.From.IsZero() {
			from = stamp(sel.From)
		}
		if !sel.To.IsZero() {
			to = stamp(sel.To)
		}
		rq.Words = "received from " + from + " to " + to
	case "picked":
		nos, err := ParseNumbers(q.Get("pick"), MaxPicked, "; use a time range")
		if err != nil {
			return bad("pick", err.Error())
		}
		sel.Nos = nos
		rq.Words = "picked requests " + q.Get("pick")
		if len(nos) == 1 {
			rq.Words = "request #" + strconv.FormatInt(nos[0], 10)
		}
	default:
		return bad("scope", "choose what the report covers: all, range or picked")
	}
	for _, n := range q["mask"] {
		if headerName(n) {
			rq.Mask = append(rq.Mask, n)
		}
	}
	for n := range strings.SplitSeq(q.Get("mask_other"), ",") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if !headerName(n) {
			return bad("mask_other", fmt.Sprintf("%q is not a header name", n))
		}
		rq.Mask = append(rq.Mask, n)
	}
	return rq, nil
}

// headerName reports a valid HTTP header name.
func headerName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range []byte(s) {
		if c <= ' ' || c >= 0x7f || strings.IndexByte(`"(),/:;<=>?@[\]{}`, c) >= 0 {
			return false
		}
	}
	return true
}

// parseTime reads a datetime-local value (as UTC) or an RFC 3339 time;
// empty is zero.
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05.000", "2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("is not a date and time")
}

// ParseNumbers reads "3, 5-9" into request numbers, in order, each once;
// more than limit is an error ending with tooMany.
func ParseNumbers(s string, limit int, tooMany string) ([]int64, error) {
	var out []int64
	seen := map[int64]bool{}
	add := func(n int64) error {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
		if len(out) > limit {
			return fmt.Errorf("at most %d requests can be picked%s", limit, tooMany)
		}
		return nil
	}
	for part := range strings.SplitSeq(strings.ReplaceAll(s, " ", ","), ",") {
		if part == "" {
			continue
		}
		a, b, isRange := strings.Cut(strings.TrimPrefix(part, "#"), "-")
		lo, err1 := strconv.ParseInt(a, 10, 64)
		hi, err2 := lo, error(nil)
		if isRange {
			hi, err2 = strconv.ParseInt(strings.TrimPrefix(b, "#"), 10, 64)
		}
		if err1 != nil || err2 != nil || lo < 1 || hi < lo {
			return nil, fmt.Errorf("%q is not a request number or a range like 5-9", part)
		}
		if hi-lo >= int64(limit) {
			return nil, fmt.Errorf("at most %d requests can be picked%s", limit, tooMany)
		}
		for n := lo; n <= hi; n++ {
			if err := add(n); err != nil {
				return nil, err
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("give the request numbers, e.g. 3, 5-9")
	}
	slices.Sort(out)
	return out, nil
}
