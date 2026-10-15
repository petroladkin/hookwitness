package report

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/store"
)

// bigRegister stores n requests with 2 KiB JSON bodies, verdicts and every
// tenth one a retry, in a real store.
func bigRegister(tb testing.TB, n int) (*store.Store, int64) {
	tb.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(tb.TempDir(), "big.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { st.Close() })
	ep, err := st.CreateEndpoint(ctx, "/orders")
	if err != nil {
		tb.Fatal(err)
	}
	base := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	pad := strings.Repeat("x", 1900)
	for i := range n {
		id := i
		if i%10 == 9 {
			id = i - 1 // a retry of the previous body
		}
		body := fmt.Sprintf(`{"type":"order.paid","order_id":%d,"pad":%q}`, id, pad)
		r := &store.Request{
			EndpointID: ep.ID, ReceivedAt: base.Add(time.Duration(i) * 250 * time.Millisecond),
			Method: "POST", Path: "/orders", Proto: "HTTP/1.1", RemoteAddr: "203.0.113.5:40000",
			Headers: []store.Header{{Name: "Content-Type", Value: "application/json"}, {Name: "X-Hub-Signature-256", Value: "sha256=" + strings.Repeat("ab", 32)}},
			Body:    []byte(body), Size: int64(len(body)), Status: 200,
			Outcome:  `{"source":"default","action":"reply","status":200}`,
			Verdict:  "pass",
			Verdicts: `[{"check":1,"label":"GitHub signature","result":"pass","reason":"signature matches","at":"2026-10-11T12:00:00Z"}]`,
		}
		if err := st.AddRequest(ctx, r, n+1, nil); err != nil {
			tb.Fatal(err)
		}
	}
	return st, ep.ID
}

func storeSource(st *store.Store, ep int64) Source {
	return func(ctx context.Context, withBody bool, through int64, fn func(store.Request) error) error {
		return st.EachSelected(ctx, ep, store.Selection{Through: through}, withBody, fn)
	}
}

// countingWriter counts what has been written.
type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// TestStreamed: Markdown and HTML are written while the requests are read,
// not built whole first: when the last request is read, nearly all of the
// report is already out.
func TestStreamed(t *testing.T) {
	st, ep := bigRegister(t, 300)
	for _, f := range []Format{Markdown, HTML} {
		var out countingWriter
		var atLast int64
		src := storeSource(st, ep)
		seen := 0
		wrapped := func(ctx context.Context, withBody bool, through int64, fn func(store.Request) error) error {
			return src(ctx, withBody, through, func(r store.Request) error {
				if withBody {
					seen++
					if seen == 300 {
						atLast = out.n
					}
				}
				return fn(r)
			})
		}
		if err := Write(context.Background(), &out, f, Options{Endpoint: "/orders", Generated: time.Now()}, wrapped); err != nil {
			t.Fatal(err)
		}
		if atLast < out.n*9/10 {
			t.Fatalf("%s: %d of %d bytes written when the last request was read", f, atLast, out.n)
		}
	}
}

// BenchmarkReport1000 measures each format over 1000 requests with 2 KiB
// bodies read from a real store (AC: under 2 s each on a laptop).
func BenchmarkReport1000(b *testing.B) {
	st, ep := bigRegister(b, 1000)
	for _, f := range []Format{Markdown, HTML, PDF} {
		b.Run(string(f), func(b *testing.B) {
			for b.Loop() {
				var out countingWriter
				if err := Write(context.Background(), &out, f, Options{Endpoint: "/orders", Generated: time.Now()}, storeSource(st, ep)); err != nil {
					b.Fatal(err)
				}
				b.ReportMetric(float64(out.n)/1e6, "MB")
			}
		})
	}
}
