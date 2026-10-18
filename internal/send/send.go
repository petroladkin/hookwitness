// Package send sends one HTTP/1.1 request exactly as given — header lines in
// their order and letter case, repeated names kept, the body's bytes as they
// are — and reads the answer the same way. net/http cannot: it canonicalises
// names, writes headers from a map, adds User-Agent and Accept-Encoding,
// decompresses answers, follows redirects and may switch to HTTP/2.
//
// Every connection goes through a Policy, checked on the address being
// dialled, so a target that names or resolves to a link-local or cloud
// metadata address is refused (server-side request forgery).
package send

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/petroladkin/hookwitness/internal/wire"
)

// Head is what goes before the body: the request line's method and target
// (path and query, as sent) and the header lines in the order and case to
// write them.
type Head struct {
	Method, Target string
	Headers        []wire.Header
}

// Resolver looks a host name up; net.DefaultResolver is one.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Options are the target and the limits of one exchange.
type Options struct {
	URL         *url.URL      // scheme, host and port of the target; its path and query are not used (Head.Target is)
	Timeout     time.Duration // the whole exchange; 0 means DefaultTimeout
	MaxBody     int64         // answer body bytes kept; 0 means DefaultMaxBody
	Policy      Policy        // which addresses may be dialled
	InsecureTLS bool          // https: accept any certificate
	// KeepHost sends the received Host line as it is; by default its value
	// becomes the target's host and port, in the same place.
	KeepHost bool

	Resolver Resolver       // nil: net.DefaultResolver
	RootCAs  *x509.CertPool // nil: the system's roots
	// dialControl, when set (tests), replaces the policy check of the
	// dialer's Control hook.
	dialControl func(network, address string, c syscall.RawConn) error
}

// Limits.
const (
	DefaultTimeout = 30 * time.Second
	DefaultMaxBody = 1 << 20
	ConnectTimeout = 10 * time.Second
	maxAnswerHead  = 64 << 10
)

// Response is the target's answer, and what was sent to get it.
type Response struct {
	Proto   string // HTTP/1.1, as the status line says
	Status  int    // 0 when there was no answer
	Reason  string
	Headers []wire.Header // as received: order, case, repetition
	Body    []byte        // the first MaxBody bytes, never decompressed
	// Size is the body's bytes the target sent; -1 when the answer was cut
	// before its end (a timeout or a reset while reading it).
	Size      int64
	Truncated bool // Body is shorter than the body

	Line  string        // the request line sent
	Sent  []wire.Header // the header lines written, in order
	Notes []string      // where the request differs from the one received

	Duration time.Duration // first byte written to last byte read
	TTFB     time.Duration // first byte written to first byte read
}

// Error is a failed exchange: Kind is dial, tls, timeout, reset, write or
// answer. A Response returned with it holds what was sent and whatever
// arrived before the failure.
type Error struct {
	Kind string
	Err  error
}

func (e *Error) Error() string { return e.Kind + ": " + oneLine(e.Err) }
func (e *Error) Unwrap() error { return e.Err }

func oneLine(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}

// Send sends head and body to the target and reads its answer. A policy
// refusal wraps ErrRefused and nothing is dialled; any other failure is an
// *Error, with the Response filled as far as the exchange got.
func Send(ctx context.Context, h Head, body []byte, o Options) (Response, error) {
	var res Response
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.MaxBody <= 0 {
		o.MaxBody = DefaultMaxBody
	}
	if err := o.Policy.CheckURL(o.URL); err != nil {
		return res, err
	}
	msg, err := compose(h, body, o, &res)
	if err != nil {
		return res, err
	}

	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	conn, err := dial(ctx, o)
	if err != nil {
		if errors.Is(err, ErrRefused) {
			return res, err
		}
		return res, classify(ctx, "dial", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	if o.URL.Scheme == "https" {
		host, _ := Endpoint(o.URL)
		// ServerName is also checked against an address's IP SANs; it goes
		// out as SNI only when it is a name.
		cfg := &tls.Config{ServerName: host, NextProtos: []string{"http/1.1"}, RootCAs: o.RootCAs,
			InsecureSkipVerify: o.InsecureTLS, MinVersion: tls.VersionTLS12}
		tc := tls.Client(conn, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			return res, classify(ctx, "tls", err)
		}
		if p := tc.ConnectionState().NegotiatedProtocol; p != "" && p != "http/1.1" {
			return res, &Error{"tls", fmt.Errorf("the target chose %s; only HTTP/1.1 is sent", p)}
		}
		conn = tc
	}

	start := time.Now()
	if _, err := conn.Write(msg); err != nil {
		res.Duration = time.Since(start)
		return res, classify(ctx, "write", err)
	}
	cr := &firstRead{r: conn}
	err = readAnswer(bufio.NewReader(cr), h.Method, o.MaxBody, &res)
	end := time.Now()
	res.Duration = end.Sub(start)
	if !cr.at.IsZero() {
		res.TTFB = cr.at.Sub(start)
	}
	if err != nil {
		return res, classify(ctx, "answer", err)
	}
	return res, nil
}

// compose builds the bytes to write and records the request line, the
// header lines and the notes in res.
func compose(h Head, body []byte, o Options, res *Response) ([]byte, error) {
	if !token(h.Method) {
		return nil, &Error{"write", fmt.Errorf("the method %q cannot be sent", h.Method)}
	}
	if h.Target == "" || strings.ContainsAny(h.Target, " \r\n\x00") {
		return nil, &Error{"write", fmt.Errorf("the request target %q cannot be sent", h.Target)}
	}
	authority := o.URL.Host // as written in the URL: the port only when given
	var lines []wire.Header
	hostSeen, chunked, length := false, false, false
	for _, hd := range h.Headers {
		if !token(hd.Name) || strings.ContainsAny(hd.Value, "\r\n\x00") {
			return nil, &Error{"write", fmt.Errorf("the header line %q cannot be sent as it is", hd.Name)}
		}
		switch {
		case strings.EqualFold(hd.Name, "Host"):
			if hostSeen {
				res.Notes = append(res.Notes, "a second Host line is left out: it would make the request invalid")
				continue
			}
			hostSeen = true
			if !o.KeepHost {
				hd.Value = authority
			}
		case strings.EqualFold(hd.Name, "Transfer-Encoding"):
			chunked = chunked || strings.Contains(strings.ToLower(hd.Value), "chunked")
		case strings.EqualFold(hd.Name, "Content-Length"):
			length = true
		}
		lines = append(lines, hd)
	}
	if !hostSeen && !o.KeepHost {
		lines = append([]wire.Header{{Name: "Host", Value: authority}}, lines...)
		res.Notes = append(res.Notes, "Host added: the request had none, and HTTP/1.1 needs it")
	}
	if !chunked && !length && len(body) > 0 {
		lines = append(lines, wire.Header{Name: "Content-Length", Value: strconv.Itoa(len(body))})
		res.Notes = append(res.Notes, "Content-Length added: the request had no framing for its body")
	}
	if chunked {
		res.Notes = append(res.Notes, "the chunked body is sent as one chunk, without chunk extensions or trailers")
	}

	var b bytes.Buffer
	res.Line = h.Method + " " + h.Target + " HTTP/1.1"
	b.WriteString(res.Line + "\r\n")
	for _, hd := range lines {
		b.WriteString(hd.Name + ": " + hd.Value + "\r\n")
	}
	b.WriteString("\r\n")
	if chunked {
		if len(body) > 0 {
			b.WriteString(strconv.FormatInt(int64(len(body)), 16) + "\r\n")
			b.Write(body)
			b.WriteString("\r\n")
		}
		b.WriteString("0\r\n\r\n")
	} else {
		b.Write(body)
	}
	res.Sent = lines
	return b.Bytes(), nil
}

// token reports whether s is an HTTP token (a method or a header name).
func token(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c >= 0x7f || strings.IndexByte(`"(),/:;<=>?@[\]{}`, c) >= 0 {
			return false
		}
	}
	return true
}

// dial connects to the target's first address the policy allows. The
// dialer's Control hook checks the address once more right before
// connecting, so nothing outside the policy is ever connected to.
func dial(ctx context.Context, o Options) (net.Conn, error) {
	host, port := Endpoint(o.URL)
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		r := o.Resolver
		if r == nil {
			r = net.DefaultResolver
		}
		lctx, cancel := context.WithTimeout(ctx, ConnectTimeout)
		addrs, err = r.LookupNetIP(lctx, "ip", host)
		cancel()
		if err != nil {
			return nil, err
		}
	}
	control := o.dialControl
	if control == nil {
		control = func(network, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return refuse("the address %q cannot be checked", address)
			}
			return o.Policy.CheckAddr(host, port, ap.Addr())
		}
	}
	d := net.Dialer{Timeout: ConnectTimeout, Control: control}
	var first error
	for _, a := range addrs {
		if err := o.Policy.CheckAddr(host, port, a); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		c, err := d.DialContext(ctx, "tcp", netip.AddrPortFrom(a.WithZone(""), uint16(port)).String())
		if err == nil {
			return c, nil
		}
		if first == nil || errors.Is(first, ErrRefused) && !errors.Is(err, ErrRefused) {
			first = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	if first == nil {
		first = fmt.Errorf("%s has no address", host)
	}
	return nil, first
}

// classify names a failure: a timeout when the exchange's time ran out, a
// reset when the target closed the connection abruptly, else kind.
func classify(ctx context.Context, kind string, err error) error {
	var ne net.Error
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return &Error{"timeout", fmt.Errorf("no complete answer within the timeout (%s)", oneLine(err))}
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return &Error{"reset", err}
	case kind == "answer" && (errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)):
		return &Error{"reset", fmt.Errorf("the target closed the connection before the answer ended: %w", err)}
	}
	return &Error{kind, err}
}

// firstRead notes when the first answer byte arrives.
type firstRead struct {
	r  io.Reader
	at time.Time
}

func (f *firstRead) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if n > 0 && f.at.IsZero() {
		f.at = time.Now()
	}
	return n, err
}

// readAnswer reads the final answer: interim 1xx answers (but 101) are
// skipped; the head is read line by line to keep its order and case, then
// net/http reads the body's framing (length, chunked or until close),
// without decompressing it.
func readAnswer(br *bufio.Reader, method string, maxBody int64, res *Response) error {
	for {
		raw, err := readHead(br)
		if err != nil {
			return err
		}
		if err := parseHead(raw, res); err != nil {
			return err
		}
		if res.Status >= 200 || res.Status == http.StatusSwitchingProtocols {
			hr, err := http.ReadResponse(bufio.NewReader(io.MultiReader(bytes.NewReader(raw), br)), &http.Request{Method: method})
			if err != nil {
				return err
			}
			defer hr.Body.Close()
			return readBody(hr.Body, maxBody, res)
		}
		res.Headers = nil
	}
}

// readHead returns the head's bytes up to and including the blank line.
func readHead(br *bufio.Reader) ([]byte, error) {
	var raw []byte
	for {
		line, err := br.ReadSlice('\n')
		raw = append(raw, line...)
		if len(raw) > maxAnswerHead {
			return nil, fmt.Errorf("the answer's head is over %d KiB", maxAnswerHead>>10)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if len(raw) == 0 && errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("the target closed the connection without answering: %w", io.ErrUnexpectedEOF)
			}
			return nil, err
		}
		if len(raw) > 0 && (bytes.Equal(line, []byte("\r\n")) || bytes.Equal(line, []byte("\n"))) && len(raw) > len(line) {
			return raw, nil
		}
	}
}

func parseHead(raw []byte, res *Response) error {
	lines := strings.Split(strings.TrimRight(string(raw), "\r\n"), "\n")
	status := strings.TrimRight(lines[0], "\r")
	proto, rest, _ := strings.Cut(status, " ")
	code, reason, _ := strings.Cut(rest, " ")
	n, err := strconv.Atoi(code)
	if !strings.HasPrefix(proto, "HTTP/") || err != nil || len(code) != 3 {
		return fmt.Errorf("the answer is not HTTP: %.60q", status)
	}
	res.Proto, res.Status, res.Reason = proto, n, reason
	res.Headers = nil
	for _, l := range lines[1:] {
		l = strings.TrimRight(l, "\r")
		if l == "" {
			continue
		}
		if (l[0] == ' ' || l[0] == '\t') && len(res.Headers) > 0 { // an obsolete folded line
			last := &res.Headers[len(res.Headers)-1]
			last.Value += " " + strings.TrimSpace(l)
			continue
		}
		name, value, ok := strings.Cut(l, ":")
		if !ok {
			return fmt.Errorf("the answer has a header line without a colon: %.60q", l)
		}
		res.Headers = append(res.Headers, wire.Header{Name: name, Value: strings.TrimSpace(value)})
	}
	return nil
}

// readBody keeps the first maxBody bytes and counts the rest.
func readBody(r io.Reader, maxBody int64, res *Response) error {
	var b bytes.Buffer
	n, err := io.Copy(&b, io.LimitReader(r, maxBody))
	res.Body = b.Bytes()
	res.Size = n
	if err == nil && n == maxBody {
		var rest int64
		rest, err = io.Copy(io.Discard, r)
		res.Size += rest
		res.Truncated = rest > 0
	}
	if err != nil {
		res.Size = -1
		res.Truncated = true
		return err
	}
	return nil
}
