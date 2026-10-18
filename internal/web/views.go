package web

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/petroladkin/hookwitness/internal/auth"
	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/store"
)

// navView is what the masthead of every page needs.
type navView struct {
	Prefix     string
	Catch      string // the catch URLs' base, e.g. http://127.0.0.1:8080 (api.CatchURL)
	Endpoints  []store.Endpoint
	Unmatched  int   // requests in the Unmatched register
	Current    int64 // register shown; -1 for none
	OnRegister bool  // a register page with a live feed
	Asset      func(string) string
	Build      string // the build serving the page: "<version> (<commit>)"
	NoLogin    bool   // login is off: every page says that anyone can use it
	User       string // who is logged in; "" with login off
	Admin      bool   // the logged-in user may manage users
	Bare       bool   // the login page: no endpoints, no addresses
}

// registerView is one register: an endpoint's requests or the Unmatched ones.
type registerView struct {
	ID       int64
	Path     string // empty for Unmatched
	Enabled  bool   // the endpoint is on (always true for Unmatched)
	Count    int
	Rows     []rowView
	More     bool  // older requests exist beyond Rows
	Newest   int64 // id of the newest row, where the live feed starts
	Base     string
	Selected int64
	Keep     int // retention: the register never holds more
	Verdicts verdictsView
	Export   *exportView      // the report dialog, when open
	Draft    *draftDialogView // the schema draft dialog, when open
}

// rowView is one line of the register.
type rowView struct {
	ID, No   int64
	Method   string
	At       time.Time
	Size     int64
	TooLarge bool
	Status   int
	Delivery int    // 1 first delivery of its body, 2 first retry...
	Group    string // body fingerprint: same body, same group
	Detail   string
	Href     string
	Fresh    bool // arrived on the live feed
	Outcome  emulate.Outcome
	Verdict  string // overall authorization verdict, "" without checks
}

// exhibitView is one request in full.
type exhibitView struct {
	ID, No   int64
	Self     string // the exhibit's own page
	Method   string
	Path     string
	RawQuery string
	Proto    string
	At       time.Time
	From     string
	Size     int64
	TooLarge bool
	Status   int
	Delivery int
	Headers  []store.Header
	Query    []queryParam
	Body     bodyView
	Download string
	Kept     int64 // body bytes kept: less than Size when only the head was (Unmatched)
	Unmatch  bool
	Limit    int64
	Outcome  emulate.Outcome
	Verdicts []auth.Verdict
	BadPaths int // body tokens marked by schema problems
	Images   []imageView
}

// imageView is an image the request carries, under its body.
type imageView struct {
	Label    string // the JSON pointer, or "the body"
	Note     string // "PNG · 412 B · 40×30 px · declared by a schema"
	Error    string // why it is not a valid image
	Hint     string // why a valid image is not shown
	Src      string // the decoded image, shown when it can be (PNG, JPEG, GIF, WebP)
	Download string
}

func id(n int64) string { return strconv.FormatInt(n, 10) }

func clock(t time.Time) string { return t.UTC().Format("15:04:05.000") }

func fullTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05.000") + " UTC" }

// size formats a byte count the way the UI shows it.
func size(n int64) string {
	switch {
	case n < 0:
		return "unknown size"
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// host is the address part of host:port.
func host(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

func (n navView) registerURL(regID int64) string {
	if regID == store.Unmatched {
		return n.Prefix + "/e/unmatched"
	}
	return n.Prefix + "/e/" + id(regID)
}

func lastSeen(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return fullTime(t)
}

func title(reg registerView) string {
	if reg.ID == store.Unmatched {
		return "Unmatched"
	}
	return reg.Path
}

// tryCommand is a curl line that sends a test request to an endpoint.
func tryCommand(base, path string) string {
	return "curl -X POST " + base + path + " \\\n" +
		"  -H 'Content-Type: application/json' \\\n" +
		`  -d '{"hello":"world"}'`
}

// mark is how the strip draws a request: "ok" (2xx), "bad" (any other
// status, a refusal, a render error, a refused body), "timeout" or "drop".
func (r rowView) mark() string {
	switch {
	case r.Outcome.HeldUp():
		return "timeout"
	case r.Outcome.Action == emulate.ActDrop:
		return "drop"
	case r.TooLarge || !r.Outcome.Good():
		return "bad"
	}
	return "ok"
}

// tag is the short note on a register row about how it was answered, empty
// for an ordinary 2xx default reply.
func (r rowView) tag() string {
	o := r.Outcome
	switch {
	case o.Action == emulate.ActTimeout:
		return "timeout"
	case o.Action == emulate.ActAbandoned:
		return "sender left"
	case o.Action == emulate.ActDrop:
		return "dropped"
	case o.Action == emulate.ActRefused:
		return strconv.Itoa(o.Status) + " refused"
	case o.Action == emulate.ActError:
		return "500 template error"
	case o.Source == emulate.FromRule:
		return strconv.Itoa(o.Status) + " · rule " + strconv.Itoa(o.Rule)
	case o.Source == emulate.FromOff:
		return "endpoint off"
	case o.Source == emulate.FromDefault && !o.Good():
		return strconv.Itoa(o.Status)
	}
	return ""
}

// someOff reports whether any endpoint is off: the picker then groups them.
func (n navView) someOff() bool {
	for _, e := range n.Endpoints {
		if !e.Enabled {
			return true
		}
	}
	return false
}

// hostOf is a catch base without its "http://" scheme, for the header's
// address; an https base keeps its scheme so it is not mistaken for http.
func hostOf(base string) string { return strings.TrimPrefix(base, "http://") }
