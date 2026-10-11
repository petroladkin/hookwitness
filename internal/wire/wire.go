// Package wire keeps what net/http throws away: the header lines of each
// request in the order, letter case and repetition they arrived in.
//
// net/http parses headers into a map, which loses their order. Listener wraps
// every accepted connection in a Conn that follows the inbound HTTP/1.x byte
// stream alongside net/http (request heads, then Content-Length or chunked
// bodies) and queues each request head. A handler takes the head of its
// request with Take. net/http still does the real parsing; a stream this
// package cannot follow only switches it off for that connection.
package wire

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Header is one header line as received.
type Header struct {
	Name, Value string
}

// maxHead bounds a request head; net/http refuses larger ones anyway
// (http.DefaultMaxHeaderBytes plus its slack).
const maxHead = http.DefaultMaxHeaderBytes + 4096

// Listener wraps the connections ln accepts so that Take works on their
// requests. Use it together with ConnContext on the http.Server.
func Listener(ln net.Listener) net.Listener { return listener{ln} }

type listener struct{ net.Listener }

func (l listener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &Conn{Conn: c}, nil
}

type ctxKey struct{}

// ConnContext is for http.Server.ConnContext: it makes the connection's
// recorded heads reachable from its requests.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	if wc, ok := c.(*Conn); ok {
		return context.WithValue(ctx, ctxKey{}, wc)
	}
	return ctx
}

// Take returns the received header lines of r and removes them from its
// connection's queue. Every request on a connection must be taken, in order,
// or the following ones are attributed wrongly; the root handler does it.
// ok is false when the order is unknown: no recording connection, or a
// stream that could not be followed.
func Take(r *http.Request) (hs []Header, ok bool) {
	c, _ := r.Context().Value(ctxKey{}).(*Conn)
	if c == nil || r.ProtoMajor != 1 {
		return nil, false
	}
	return c.take(r.Method, r.RequestURI)
}

// Conn records the request heads read through it.
type Conn struct {
	net.Conn
	mu    sync.Mutex
	p     parser
	heads []head
}

type head struct {
	line    string // request line
	headers []Header
}

func (c *Conn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.mu.Lock()
		c.p.feed(b[:n], func(h head) { c.heads = append(c.heads, h) })
		c.mu.Unlock()
	}
	return n, err
}

func (c *Conn) take(method, uri string) ([]Header, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.p.broken || len(c.heads) == 0 {
		return nil, false
	}
	h := c.heads[0]
	c.heads = c.heads[1:]
	if !strings.HasPrefix(h.line, method+" "+uri+" ") {
		// Out of step with net/http: stop guessing for this connection.
		c.p.broken = true
		c.heads = nil
		return nil, false
	}
	return h.headers, true
}

type state int

const (
	inHead state = iota
	inBody
	inChunkSize
	inChunkData
	inChunkEnd
	inTrailer
)

// parser follows HTTP/1.x request framing over a byte stream.
type parser struct {
	st     state
	buf    []byte // head, chunk-size line or trailer being accumulated
	remain int64  // body or chunk bytes still to skip
	broken bool
}

func (p *parser) feed(b []byte, emit func(head)) {
	for len(b) > 0 && !p.broken {
		switch p.st {
		case inHead:
			// Blank lines before a request line are tolerated (RFC 9112, 2.2).
			if len(p.buf) == 0 && (b[0] == '\r' || b[0] == '\n') {
				b = b[1:]
				continue
			}
			end, rest, done := p.line(b, true)
			b = rest
			if !done {
				continue
			}
			h, framing, length, ok := parseHead(end)
			if !ok || len(end) > maxHead {
				p.broken = true
				return
			}
			emit(h)
			switch framing {
			case "chunked":
				p.st = inChunkSize
			case "length":
				if length > 0 {
					p.st, p.remain = inBody, length
				}
			}
		case inBody, inChunkData:
			n := min(int64(len(b)), p.remain)
			b, p.remain = b[n:], p.remain-n
			if p.remain == 0 {
				if p.st == inBody {
					p.st = inHead
				} else {
					p.st = inChunkEnd
				}
			}
		case inChunkEnd:
			// The CRLF after chunk data.
			i := bytes.IndexByte(b, '\n')
			if i < 0 {
				b = nil
				continue
			}
			b, p.st = b[i+1:], inChunkSize
		case inChunkSize:
			line, rest, done := p.line(b, false)
			b = rest
			if !done {
				continue
			}
			size, ok := chunkSize(line)
			if !ok {
				p.broken = true
				return
			}
			if size == 0 {
				p.st = inTrailer
			} else {
				p.st, p.remain = inChunkData, size
			}
		case inTrailer:
			end, rest, done := p.line(b, true)
			b = rest
			if !done {
				continue
			}
			_ = end
			p.st = inHead
		}
	}
}

// line accumulates b into p.buf until a line end (whole == false) or an empty
// line after at least one line (whole == true, a head or a trailer section).
// It returns the accumulated bytes when complete and the unread rest of b.
func (p *parser) line(b []byte, whole bool) (got, rest []byte, done bool) {
	start := len(p.buf)
	p.buf = append(p.buf, b...)
	var end int
	if whole {
		// A trailer section may be empty: just CRLF.
		if bytes.HasPrefix(p.buf, []byte("\r\n")) && p.st == inTrailer {
			end = 2
		} else if bytes.HasPrefix(p.buf, []byte("\n")) && p.st == inTrailer {
			end = 1
		} else if i := bytes.Index(p.buf, []byte("\r\n\r\n")); i >= 0 {
			end = i + 4
		} else if i := bytes.Index(p.buf, []byte("\n\n")); i >= 0 {
			end = i + 2
		}
	} else if i := bytes.IndexByte(p.buf, '\n'); i >= 0 {
		end = i + 1
	}
	if end == 0 {
		if len(p.buf) > maxHead {
			p.broken = true
		}
		return nil, nil, false
	}
	got = p.buf[:end]
	rest = b[end-start:]
	p.buf = nil
	return got, rest, true
}

// parseHead splits a request head into the request line and header lines,
// and works out how its body is framed, as net/http does: chunked when
// Transfer-Encoding ends in chunked, otherwise Content-Length.
func parseHead(raw []byte) (h head, framing string, length int64, ok bool) {
	lines := strings.Split(strings.TrimRight(string(raw), "\r\n"), "\n")
	h.line = strings.TrimSuffix(lines[0], "\r")
	if !strings.Contains(h.line, " HTTP/1.") {
		return head{}, "", 0, false
	}
	framing = "length"
	cl := ""
	for _, l := range lines[1:] {
		l = strings.TrimSuffix(l, "\r")
		if l == "" {
			continue
		}
		if (l[0] == ' ' || l[0] == '\t') && len(h.headers) > 0 {
			// Obsolete line folding: continues the previous value.
			last := &h.headers[len(h.headers)-1]
			last.Value += " " + strings.Trim(l, " \t")
			continue
		}
		name, value, found := strings.Cut(l, ":")
		if !found {
			return head{}, "", 0, false
		}
		value = strings.Trim(value, " \t")
		h.headers = append(h.headers, Header{name, value})
		switch strings.ToLower(name) {
		case "transfer-encoding":
			codings := strings.Split(value, ",")
			if strings.EqualFold(strings.TrimSpace(codings[len(codings)-1]), "chunked") {
				framing = "chunked"
			}
		case "content-length":
			cl = value
		}
	}
	if framing == "length" && cl != "" {
		n, err := strconv.ParseInt(cl, 10, 64)
		if err != nil || n < 0 {
			return head{}, "", 0, false
		}
		length = n
	}
	return h, framing, length, true
}

func chunkSize(line []byte) (int64, bool) {
	s := strings.TrimRight(string(line), "\r\n")
	s, _, _ = strings.Cut(s, ";")
	n, err := strconv.ParseInt(strings.TrimSpace(s), 16, 64)
	return n, err == nil && n >= 0
}
