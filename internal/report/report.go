// Package report writes caught requests as evidence to attach to a ticket:
// Markdown, one self-contained HTML file, or a PDF. A report has a header
// (endpoint, time range, selection, generation, masking), a summary (answers
// by class, verdicts per check, retry groups, a strip of the range) and
// every request with its headers, body, answer and verdicts.
//
// The requests are read twice through a Source: once without bodies for the
// summary, then one by one with bodies while the report is written, so a
// long register is never held whole (the PDF writer keeps its pages until
// the end). Apart from the generation time, the same requests and options
// always give the same bytes.
package report

import (
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/petroladkin/hookwitness/internal/auth"
	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/payload"
	"github.com/petroladkin/hookwitness/internal/picture"
	"github.com/petroladkin/hookwitness/internal/store"
)

// Format is a report's file format.
type Format string

// The formats, named by their file extension.
const (
	Markdown Format = "md"
	HTML     Format = "html"
	PDF      Format = "pdf"
)

// ParseFormat accepts md, html and pdf.
func ParseFormat(s string) (Format, error) {
	switch f := Format(s); f {
	case Markdown, HTML, PDF:
		return f, nil
	}
	return "", fmt.Errorf("unknown report format %q: md, html or pdf", s)
}

// ContentType is the media type of a report in the format.
func (f Format) ContentType() string {
	switch f {
	case Markdown:
		return "text/markdown; charset=utf-8"
	case HTML:
		return "text/html; charset=utf-8"
	}
	return "application/pdf"
}

// BodyLimit is how much of a body a report shows by default; a longer one
// is cut, with its full size and SHA-256 noted.
const BodyLimit = 64 << 10

// Source calls fn for each reported request, oldest first; withBody asks
// for the bodies too. It is called twice per report.
type Source func(ctx context.Context, withBody bool, fn func(store.Request) error) error

// Options say what a report covers and how it is written.
type Options struct {
	Endpoint  string    // the endpoint's path; empty for the Unmatched register
	Selection string    // how the requests were chosen, in words: "whole register"
	From, To  time.Time // the selected range; zero bounds are taken from the requests
	Mask      []string  // header names whose values are masked (any case)
	Version   string    // the Hookwitness build that writes it
	Generated time.Time // when it is written
	BodyLimit int       // bytes of a body shown; 0 is BodyLimit
	NoImages  bool      // list images without showing them (smaller files)
	// Fonts holds the UI's woff2 files (vendor/<family>/<file>.woff2) that
	// an HTML report embeds; nil writes the HTML with system fonts only.
	Fonts fs.FS
}

// FileName is hookwitness-<endpoint>-<UTC timestamp>.<ext>, the endpoint
// path flattened to letters, digits and dashes.
func FileName(endpoint string, at time.Time, f Format) string {
	name := "unmatched"
	if endpoint != "" {
		var sb strings.Builder
		dash := false
		for _, r := range strings.ToLower(endpoint) {
			if r < utf8.RuneSelf && (r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
				sb.WriteRune(r)
				dash = false
			} else if !dash && sb.Len() > 0 {
				sb.WriteByte('-')
				dash = true
			}
		}
		if n := strings.TrimSuffix(sb.String(), "-"); n != "" {
			name = n
		} else {
			name = "root"
		}
	}
	return "hookwitness-" + name + "-" + at.UTC().Format("20060102T150405Z") + "." + string(f)
}

// Write writes the report of the requests src yields.
func Write(ctx context.Context, w io.Writer, f Format, o Options, src Source) error {
	h, err := summarize(ctx, o, src)
	if err != nil {
		return err
	}
	var wr writer
	switch f {
	case Markdown:
		wr = newMarkdown(w)
	case HTML:
		wr = newHTML(w, o.Fonts)
	case PDF:
		wr, err = newPDF(w)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown report format %q", f)
	}
	if err := wr.head(h); err != nil {
		return err
	}
	err = src(ctx, true, func(r store.Request) error {
		return wr.item(h.item(r))
	})
	if err != nil {
		return err
	}
	return wr.end()
}

// writer is one format's output, fed in order.
type writer interface {
	head(h *Head) error
	item(it Item) error
	end() error
}

// Head is everything above the requests.
type Head struct {
	Endpoint  string // "/orders" or "Unmatched"
	Selection string
	From, To  time.Time // the reported range
	Generated time.Time
	Version   string
	Masked    []string // the masked header names, as given, sorted

	Count    int
	Outcomes []Count // answers by class, in a fixed order
	Checks   []CheckCount
	Retries  []Retry
	Marks    []Mark

	bodyLimit  int
	noImages   bool
	mask       map[string]bool
	deliveries map[int64]int // request id → delivery number, only for retries
}

// Count is a labelled number.
type Count struct {
	Label string
	N     int
}

// CheckCount is one check's verdicts across the reported requests.
type CheckCount struct {
	Check              int
	Label              string
	Pass, Fail, NC, NV int
}

// Retry is one body delivered more than once: the request numbers in order
// and the gaps between consecutive deliveries.
type Retry struct {
	SHA256 string
	Nos    []int64
	Gaps   []time.Duration
}

// Mark is how the strip draws one request.
type Mark struct {
	At     time.Time
	Size   int64
	Kind   string // ok (2xx), bad, timeout, drop
	Failed bool   // a check's verdict is fail
	Group  int    // 1-based retry group, 0 for a single delivery
}

// The answer classes of the summary, in order. 1xx and 3xx are counted as
// "other" and shown only when there are some.
const (
	class2xx     = "2xx"
	class4xx     = "4xx"
	class5xx     = "5xx"
	classTimeout = "timeout"
	classDrop    = "drop"
	classRefused = "refused by a check"
	classOther   = "other"
)

var classOrder = []string{class2xx, class4xx, class5xx, classTimeout, classDrop, classRefused, classOther}

// classify names the summary class of an answer.
func classify(r store.Request, o emulate.Outcome) string {
	switch {
	case o.Action == emulate.ActRefused:
		return classRefused
	case o.HeldUp():
		return classTimeout
	case o.Action == emulate.ActDrop:
		return classDrop
	case r.Status >= 200 && r.Status < 300:
		return class2xx
	case r.Status >= 400 && r.Status < 500:
		return class4xx
	case r.Status >= 500:
		return class5xx
	}
	return classOther
}

// summarize reads the requests without bodies for the head.
func summarize(ctx context.Context, o Options, src Source) (*Head, error) {
	h := &Head{
		Endpoint:   o.Endpoint,
		Selection:  o.Selection,
		From:       o.From.UTC(),
		To:         o.To.UTC(),
		Generated:  o.Generated.UTC(),
		Version:    o.Version,
		bodyLimit:  cmp.Or(o.BodyLimit, BodyLimit),
		noImages:   o.NoImages,
		mask:       map[string]bool{},
		deliveries: map[int64]int{},
	}
	if h.Endpoint == "" {
		h.Endpoint = "Unmatched"
	}
	for _, m := range o.Mask {
		if k := strings.ToLower(strings.TrimSpace(m)); k != "" && !h.mask[k] {
			h.mask[k] = true
			h.Masked = append(h.Masked, strings.TrimSpace(m))
		}
	}
	slices.SortFunc(h.Masked, func(a, b string) int { return cmp.Compare(strings.ToLower(a), strings.ToLower(b)) })

	classes := map[string]int{}
	checks := map[int]*CheckCount{}
	type delivery struct {
		no int64
		at time.Time
	}
	bySHA := map[string][]delivery{}
	var shaOrder []string
	var markSHA []string // per mark, the body digest
	var first, last time.Time
	err := src(ctx, false, func(r store.Request) error {
		h.Count++
		if first.IsZero() {
			first = r.ReceivedAt
		}
		last = r.ReceivedAt
		out := emulate.ParseOutcome(r.Outcome, r.Status)
		classes[classify(r, out)]++
		m := Mark{At: r.ReceivedAt.UTC(), Size: r.Size, Kind: markKind(r, out)}
		for _, v := range parseVerdicts(r.Verdicts) {
			c := checks[v.Check]
			if c == nil {
				c = &CheckCount{Check: v.Check}
				checks[v.Check] = c
			}
			c.Label = v.Label
			switch v.Result {
			case auth.Pass:
				c.Pass++
			case auth.Fail:
				c.Fail++
				m.Failed = true
			case auth.NotChecked:
				c.NC++
			default:
				c.NV++
			}
		}
		k := ""
		if len(r.BodySHA256) > 0 {
			k = hex.EncodeToString(r.BodySHA256)
			if _, ok := bySHA[k]; !ok {
				shaOrder = append(shaOrder, k)
			}
			bySHA[k] = append(bySHA[k], delivery{r.No, r.ReceivedAt})
			if n := len(bySHA[k]); n > 1 {
				h.deliveries[r.ID] = n
			}
		}
		h.Marks = append(h.Marks, m)
		markSHA = append(markSHA, k)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if h.From.IsZero() {
		h.From = first.UTC()
	}
	if h.To.IsZero() {
		h.To = last.UTC()
	}
	for _, c := range classOrder {
		if n := classes[c]; n > 0 || c != classOther {
			h.Outcomes = append(h.Outcomes, Count{c, n})
		}
	}
	for _, c := range checks {
		h.Checks = append(h.Checks, *c)
	}
	slices.SortFunc(h.Checks, func(a, b CheckCount) int { return cmp.Compare(a.Check, b.Check) })
	group := map[string]int{}
	for _, k := range shaOrder {
		ds := bySHA[k]
		if len(ds) < 2 {
			continue
		}
		rt := Retry{SHA256: k}
		for i, d := range ds {
			rt.Nos = append(rt.Nos, d.no)
			if i > 0 {
				rt.Gaps = append(rt.Gaps, d.at.Sub(ds[i-1].at))
			}
		}
		h.Retries = append(h.Retries, rt)
		group[k] = len(h.Retries)
	}
	for i, k := range markSHA {
		h.Marks[i].Group = group[k]
	}
	return h, nil
}

// markKind is how the strip draws a request, as in the UI.
func markKind(r store.Request, o emulate.Outcome) string {
	switch {
	case o.HeldUp():
		return "timeout"
	case o.Action == emulate.ActDrop:
		return "drop"
	case r.TooLarge || !o.Good():
		return "bad"
	}
	return "ok"
}

func parseVerdicts(s string) []auth.Verdict {
	var vs []auth.Verdict
	if s != "" {
		json.Unmarshal([]byte(s), &vs)
	}
	return vs
}

// Item is one request as the report shows it.
type Item struct {
	No       int64
	ID       int64
	At       time.Time
	Method   string
	Path     string
	RawQuery string
	Query    []payload.QueryParam
	Proto    string
	Remote   string
	Headers  []Header
	Body     Body
	Answer   Answer
	Verdicts []auth.Verdict
	Delivery int // 2 for the first retry of a body, 3 for the next…; 0 otherwise
	Images   []ItemImage
}

// ItemImage is an image the request carries; Data holds its decoded bytes
// when the report shows it (a valid PNG, JPEG, GIF or WebP, images on).
type ItemImage struct {
	picture.Found
	Data []byte
}

// Header is a request header line, its value masked when asked.
type Header struct {
	Name, Value string
	Masked      bool
}

// Body kinds.
const (
	BodyEmpty   = "empty"
	BodyNotKept = "not kept" // over the body limit when caught
	BodyJSON    = "json"
	BodyText    = "text"
	BodyBinary  = "binary"
)

// Body is a request body as shown: pretty JSON (also as tokens for
// colour), text, or for binary only its size and digest.
type Body struct {
	Kind   string
	Text   string
	Tokens []payload.Token
	Size   int64
	SHA256 string
	Cut    bool // only the first Shown bytes are in Text
	Shown  int  // the limit a cut body was cut at
}

// Answer is what Hookwitness did.
type Answer struct {
	Stamp    string // "200 sent", "timeout", "dropped", "401 refused"
	Why      string // "rule 2 · every 3rd", "default reply", "check 1: …"
	DelayMs  int64
	WaitedMs int64
	Error    string
	Would    int // the sender left: the status the reply would have had
}

func (h *Head) item(r store.Request) Item {
	it := Item{
		No:       r.No,
		ID:       r.ID,
		At:       r.ReceivedAt.UTC(),
		Method:   r.Method,
		Path:     r.Path,
		RawQuery: r.RawQuery,
		Query:    payload.ParseQuery(r.RawQuery),
		Proto:    r.Proto,
		Remote:   r.RemoteAddr,
		Delivery: h.deliveries[r.ID],
	}
	var masked []string
	for _, hd := range r.Headers {
		if h.mask[strings.ToLower(hd.Name)] {
			masked = append(masked, hd.Value)
			it.Headers = append(it.Headers, Header{hd.Name, maskValue(hd.Value), true})
			continue
		}
		it.Headers = append(it.Headers, Header{Name: hd.Name, Value: hd.Value})
	}
	it.Body = viewBody(r, h.bodyLimit)
	o := emulate.ParseOutcome(r.Outcome, r.Status)
	it.Answer = Answer{Stamp: o.Stamp(), Why: o.Why(), DelayMs: o.DelayMs, WaitedMs: o.WaitedMs, Error: o.Error}
	if o.Action == emulate.ActAbandoned {
		it.Answer.Would = o.Would
	}
	if o.Action == emulate.ActReply && r.Status == 0 && !r.TooLarge {
		it.Answer.Stamp = "nothing sent"
	}
	it.Verdicts = parseVerdicts(r.Verdicts)
	bad := map[string]bool{}
	for _, v := range it.Verdicts {
		if v.Result != auth.Fail {
			continue
		}
		for _, p := range v.Problems {
			if p.Keyword != "json" {
				bad[p.Path] = true
			}
		}
	}
	payload.MarkBad(it.Body.Tokens, bad)
	if !r.TooLarge {
		ct := ""
		for _, hd := range r.Headers {
			if strings.EqualFold(hd.Name, "Content-Type") {
				ct = hd.Value
				break
			}
		}
		paths := map[string]bool{}
		for _, f := range picture.Find(r.Body, ct, it.Verdicts) {
			img := ItemImage{Found: f}
			if !h.noImages && f.Error == "" && f.NotChecked == "" && picture.Decodable(f.Type) {
				img.Data, _ = picture.At(r.Body, f.Path)
			}
			it.Images = append(it.Images, img)
			paths[f.Path] = true
		}
		if it.Body.Kind == BodyJSON && len(paths) > 0 {
			// The images are listed with their digests; their base64 would
			// fill pages of the body, so it is shortened there.
			payload.MarkImages(it.Body.Tokens, paths)
			payload.ElideImages(it.Body.Tokens, 48)
			var sb strings.Builder
			for _, t := range it.Body.Tokens {
				sb.WriteString(t.Text)
				sb.WriteString(t.Elided)
			}
			it.Body.Text = sb.String()
		}
	}
	for i, v := range it.Verdicts {
		// A signature taken from a masked header is masked too, and so is
		// the one expected in its place.
		for _, m := range masked {
			if v.Received != "" && strings.Contains(m, v.Received) {
				it.Verdicts[i].Received = maskValue(v.Received)
				if v.Expected != "" {
					it.Verdicts[i].Expected = maskValue(v.Expected)
				}
				break
			}
		}
	}
	return it
}

// maskValue hides a value but keeps its length: "•••• 71 chars".
func maskValue(v string) string {
	n := utf8.RuneCountInString(v)
	if n == 1 {
		return "•••• 1 char"
	}
	return "•••• " + strconv.Itoa(n) + " chars"
}

func viewBody(r store.Request, limit int) Body {
	b := Body{Size: r.Size}
	if len(r.BodySHA256) > 0 {
		b.SHA256 = hex.EncodeToString(r.BodySHA256)
	}
	switch {
	case r.TooLarge:
		b.Kind = BodyNotKept
		return b
	case len(r.Body) == 0:
		b.Kind = BodyEmpty
		return b
	}
	if len(r.Body) <= limit && json.Valid(r.Body) {
		if toks, err := payload.Pretty(r.Body); err == nil {
			b.Kind, b.Tokens = BodyJSON, toks
			var sb strings.Builder
			for _, t := range toks {
				sb.WriteString(t.Text)
			}
			b.Text = sb.String()
			return b
		}
	}
	shown := r.Body
	if len(shown) > limit {
		shown, b.Cut, b.Shown = shown[:limit], true, limit
		// Do not end inside a UTF-8 sequence.
		for i := 0; i < utf8.UTFMax && len(shown) > 0 && !utf8.Valid(shown); i++ {
			shown = shown[:len(shown)-1]
		}
	}
	if payload.IsText(shown) {
		b.Kind, b.Text = BodyText, string(shown)
		return b
	}
	b.Kind, b.Cut = BodyBinary, false
	return b
}

// Shared wording.

// stamp formats a time as the report shows every time: UTC, milliseconds, Z.
func stamp(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// gap formats a duration between deliveries: "+1.2 s", "+3 min 5 s".
func gap(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "+" + strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + " s"
	case d < time.Hour:
		return fmt.Sprintf("+%d min %d s", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("+%d h %d min", int(d.Hours()), int(d.Minutes())%60)
}

// size formats a byte count: "412 B", "2.0 KiB", "1.5 MiB".
func size(n int64) string {
	switch {
	case n < 0:
		return "unknown size"
	case n < 1024:
		return strconv.FormatInt(n, 10) + " B"
	case n < 1<<20:
		return strconv.FormatFloat(float64(n)/1024, 'f', 1, 64) + " KiB"
	}
	return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MiB"
}

// bodyNote is the line about a body's size and digest.
func bodyNote(b Body) string {
	switch b.Kind {
	case BodyEmpty:
		return "no body"
	case BodyNotKept:
		return size(b.Size) + " declared · not kept: over the body limit"
	}
	s := size(b.Size)
	if b.SHA256 != "" && (b.Kind == BodyBinary || b.Cut) {
		s += " · sha256 " + b.SHA256
	}
	switch {
	case b.Kind == BodyBinary:
		s += " · binary, not shown"
	case b.Cut:
		s += " · first " + size(int64(b.Shown)) + " shown"
	}
	return s
}

// answerLine is the answer in one line.
func answerLine(a Answer) string {
	parts := []string{a.Stamp}
	if a.Why != "" {
		parts = append(parts, a.Why)
	}
	switch {
	case a.Would > 0:
		parts = append(parts, "left after "+ms(a.WaitedMs)+" of a "+ms(a.DelayMs)+" delay; nothing sent, would have been "+strconv.Itoa(a.Would))
	default:
		if a.DelayMs > 0 {
			parts = append(parts, "after "+ms(a.DelayMs))
		}
		if a.WaitedMs > 0 {
			parts = append(parts, "held "+ms(a.WaitedMs))
		}
	}
	if a.Error != "" {
		parts = append(parts, a.Error)
	}
	return strings.Join(parts, " · ")
}

func ms(v int64) string {
	if v < 1000 {
		return strconv.FormatInt(v, 10) + " ms"
	}
	return strconv.FormatFloat(float64(v)/1000, 'f', 1, 64) + " s"
}

// verdictLine is a verdict in one line, with its problems counted.
func verdictLine(v auth.Verdict) string {
	s := v.Label + ": " + string(v.Result)
	if v.Reason != "" {
		s += " — " + v.Reason
	}
	return s
}

// target is the request line's target: path and query.
func target(it Item) string {
	if it.RawQuery == "" {
		return it.Path
	}
	return it.Path + "?" + it.RawQuery
}

// rangeLine is the reported time range.
func rangeLine(h *Head) string {
	if h.Count == 0 && h.From.IsZero() {
		return "no requests"
	}
	return stamp(h.From) + " – " + stamp(h.To)
}

// imageLabel is where an image is: its JSON pointer or "the body".
func imageLabel(f picture.Found) string {
	if f.Path == picture.Body {
		return "the body"
	}
	return f.Path
}

// imageNote is an image's facts in a line: type, size, pixels, origin, or
// why it is not a valid image.
func imageNote(f picture.Found) string {
	var parts []string
	if f.Type != "" {
		parts = append(parts, strings.ToUpper(strings.TrimPrefix(f.Type, "image/")))
	}
	if f.Size > 0 {
		parts = append(parts, size(int64(f.Size)))
	}
	if f.Width > 0 {
		parts = append(parts, strconv.Itoa(f.Width)+"×"+strconv.Itoa(f.Height)+" px")
	}
	switch f.Source {
	case "schema":
		parts = append(parts, "declared "+f.Declared)
	case "data URL":
		parts = append(parts, "a data URL, no schema")
	}
	if f.Error != "" {
		parts = append(parts, "not valid: "+f.Error)
	}
	if f.NotChecked != "" {
		parts = append(parts, "not checked: "+f.NotChecked)
	}
	return strings.Join(parts, " · ")
}
