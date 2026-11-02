package emulate

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/petroladkin/hookwitness/internal/send"
)

// Forward makes an endpoint a recorder in the middle: each request that
// passes its checks and that no rule takes is sent to URL, byte for byte
// (internal/send), and the receiver's answer goes back to the sender and is
// kept with the request (internal/forward).
type Forward struct {
	URL         string `json:"url"`                    // http(s)://host[:port]/path; no query, no userinfo
	KeepHost    bool   `json:"keep_host,omitempty"`    // send the received Host; by default the target's
	InsecureTLS bool   `json:"insecure_tls,omitempty"` // https: accept any certificate
	TimeoutMs   int64  `json:"timeout_ms,omitempty"`   // 0: DefaultForwardTimeout, or the cap if shorter
	OnFailure   string `json:"on_failure,omitempty"`   // OnFailureGateway (""), OnFailureDefault
}

// What the sender gets when the receiver fails.
const (
	OnFailureGateway = "gateway" // 502, 504 or 508 naming the failure
	OnFailureDefault = "default" // the endpoint's default reply
)

// Forward timeouts.
const (
	DefaultForwardTimeout = 30 * time.Second
	MinForwardTimeout     = 100 * time.Millisecond
)

// The forward's action and sources. A rule that takes a request keeps
// FromRule; its Forward note says whether the receiver got it too.
const (
	ActForward  = "forward" // the receiver's answer (or the failure answer) was sent
	FromForward = "forward"
	FromLoop    = "loop" // the request came back on the server's own forward: 508
)

// Receiver failure kinds, as ForwardNote.ErrorKind: send's (dial, tls,
// timeout, reset, write, answer) and these.
const (
	FailRefused  = "refused"   // the target policy refused it (or sending is off)
	FailTooLarge = "too_large" // the answer is over the limit passed back
	FailUpgrade  = "upgrade"   // the receiver switched protocols (101): no tunnels
	FailBusy     = "busy"      // no forward slot or answer budget was free
	FailLoop     = "loop"
)

// ForwardNote is the receiver's side of a request, in its outcome; the
// whole exchange is kept in the store's forwards table.
type ForwardNote struct {
	Target       string `json:"target"`               // origin and path at the time
	Status       int    `json:"status,omitempty"`     // the receiver's; 0 when none
	ErrorKind    string `json:"error_kind,omitempty"` // dial, tls, timeout, reset, write, answer, refused, too_large, upgrade, busy, loop
	Fallback     bool   `json:"fallback,omitempty"`   // the sender got the failure answer
	SenderLeftMs int64  `json:"sender_left_ms,omitempty"`
	Ms           int64  `json:"ms"`                // the exchange's duration
	Pending      bool   `json:"pending,omitempty"` // stored before the exchange; cleared after
}

// Target is the forward's URL, parsed.
func (f *Forward) Target() (*url.URL, error) { return send.ParseTarget(f.URL) }

// Timeout is the exchange's timeout under the cap maxWait.
func (f *Forward) Timeout(maxWait time.Duration) time.Duration {
	if f.TimeoutMs > 0 {
		return min(time.Duration(f.TimeoutMs)*time.Millisecond, maxWait)
	}
	return min(DefaultForwardTimeout, maxWait)
}

// Where names the target for people: scheme, host, port when given, path.
func (f *Forward) Where() string {
	u, err := f.Target()
	if err != nil {
		return f.URL
	}
	return u.Scheme + "://" + u.Host + u.EscapedPath()
}

// Origin is the target's scheme, host and port, for badges and logs.
func (f *Forward) Origin() string {
	u, err := f.Target()
	if err != nil {
		return ""
	}
	host, port := send.Endpoint(u)
	return u.Scheme + "://" + net.JoinHostPort(host, strconv.Itoa(port))
}

func (f *Forward) validate(maxWait time.Duration) error {
	u, err := send.ParseTarget(f.URL)
	if err != nil {
		return err
	}
	if u.RawQuery != "" || u.ForceQuery {
		return errors.New("the URL must not have a query: the sender's own query is sent with each request")
	}
	if f.TimeoutMs != 0 && (f.TimeoutMs < MinForwardTimeout.Milliseconds() || time.Duration(f.TimeoutMs)*time.Millisecond > maxWait) {
		return fmt.Errorf("the timeout is from %d ms to %d ms (-max-wait)", MinForwardTimeout.Milliseconds(), maxWait.Milliseconds())
	}
	switch f.OnFailure {
	case "", OnFailureGateway, OnFailureDefault:
	default:
		return fmt.Errorf(`on_failure is "gateway" or "default", not %q`, f.OnFailure)
	}
	return nil
}

// FallsBackToDefault reports that the sender gets the default reply when
// the receiver fails.
func (f *Forward) FallsBackToDefault() bool { return f.OnFailure == OnFailureDefault }

// Summary describes the forward's switches: "Host of the target · timeout
// 30 s · 502/504 when it fails".
func (f *Forward) Summary(maxWait time.Duration) string {
	parts := []string{"Host of the target"}
	if f.KeepHost {
		parts[0] = "Host as received"
	}
	if f.InsecureTLS {
		parts = append(parts, "any certificate")
	}
	parts = append(parts, "timeout "+Duration(f.Timeout(maxWait).Milliseconds()))
	if f.FallsBackToDefault() {
		parts = append(parts, "the default reply when it fails")
	} else {
		parts = append(parts, "502/504 when it fails")
	}
	return strings.Join(parts, " · ")
}

// GatewayStatus is the status a failure kind answers with OnFailureGateway.
func GatewayStatus(kind string) int {
	switch kind {
	case "timeout":
		return 504
	case FailLoop:
		return 508
	case FailBusy:
		return 503
	}
	return 502
}

// FailureWords says a receiver failure for people: "the receiver did not
// answer within the timeout".
func FailureWords(kind string) string {
	switch kind {
	case "dial":
		return "the receiver could not be reached"
	case "tls":
		return "the TLS handshake with the receiver failed"
	case "timeout":
		return "the receiver did not answer within the timeout"
	case "reset":
		return "the receiver closed the connection before its answer ended"
	case "write":
		return "the request could not be written to the receiver"
	case "answer":
		return "the receiver's answer could not be read"
	case FailRefused:
		return "the target policy refused the receiver"
	case FailTooLarge:
		return "the receiver's answer is over the size passed back"
	case FailUpgrade:
		return "the receiver switched protocols (101), which is not passed back"
	case FailBusy:
		return "too many forwards at once"
	case FailLoop:
		return "the request came back on the server's own forward"
	}
	return "the forward failed (" + kind + ")"
}

// forwardStamp is the stamp of a forwarded request: "200 from receiver",
// "502 receiver down", "504 receiver timeout", "forwarding…".
func (o Outcome) forwardStamp() string {
	n := o.Forward
	switch {
	case n.Pending:
		return "forwarding…"
	case n.ErrorKind == "":
		return strconv.Itoa(o.Status) + " from receiver"
	case n.Fallback && o.Status != GatewayStatus(n.ErrorKind):
		return strconv.Itoa(o.Status) + " default · receiver " + downWord(n.ErrorKind)
	}
	return strconv.Itoa(o.Status) + " receiver " + downWord(n.ErrorKind)
}

func downWord(kind string) string {
	switch kind {
	case "timeout":
		return "timeout"
	case FailLoop:
		return "loop"
	case FailRefused:
		return "refused"
	case FailBusy:
		return "busy"
	}
	return "down"
}

// ReceiverMark is the small mark of the receiver's side on a register row:
// "receiver 200", "receiver down", "not forwarded", "" when the endpoint
// does not forward.
func (o Outcome) ReceiverMark() string {
	switch {
	case o.NotForwarded:
		return "not forwarded"
	case o.Forward == nil:
		return ""
	case o.Forward.Pending:
		return "forwarding…"
	case o.Forward.ErrorKind != "":
		return "receiver " + downWord(o.Forward.ErrorKind)
	}
	return "receiver " + strconv.Itoa(o.Forward.Status)
}
