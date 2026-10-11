package web

import (
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/petroladkin/hookwitness/internal/store"
)

// navView is what the masthead of every page needs.
type navView struct {
	Prefix     string
	Host       string // as the browser addressed us: the catch URLs' host
	Endpoints  []store.Endpoint
	Unmatched  int   // requests in the Unmatched register
	Current    int64 // register shown; -1 for none
	OnRegister bool  // a register page with a live feed
	Asset      func(string) string
}

// registerView is one register: an endpoint's requests or the Unmatched ones.
type registerView struct {
	ID       int64
	Path     string // empty for Unmatched
	Count    int
	Rows     []rowView
	More     bool  // older requests exist beyond Rows
	Newest   int64 // id of the newest row, where the live feed starts
	Base     string
	Selected int64
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
}

// exhibitView is one request in full.
type exhibitView struct {
	ID, No   int64
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
	Unmatch  bool
	Limit    int64
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
func tryCommand(host, path string) string {
	return "curl -X POST http://" + host + path + " \\\n" +
		"  -H 'Content-Type: application/json' \\\n" +
		`  -d '{"hello":"world"}'`
}
