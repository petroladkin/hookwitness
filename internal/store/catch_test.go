package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), tempDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func sample(endpointID int64, body string) *Request {
	return &Request{
		EndpointID: endpointID,
		ReceivedAt: time.Date(2026, 10, 11, 12, 0, 0, 123_000_000, time.UTC),
		Method:     "POST",
		Path:       "/hooks/a",
		RawQuery:   "x=1&x=2",
		Proto:      "HTTP/1.1",
		RemoteAddr: "203.0.113.5:40000",
		Headers:    []Header{{"Content-Type", "application/json"}, {"x-b", "1"}, {"X-A", "2"}, {"x-b", "3"}},
		Body:       []byte(body),
		Size:       int64(len(body)),
		Status:     200,
	}
}

func TestEndpointLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	a, err := s.CreateEndpoint(ctx, "/stripe/webhooks")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateEndpoint(ctx, "/stripe/webhooks"); !errors.Is(err, ErrDuplicatePath) {
		t.Fatalf("duplicate path: err = %v, want ErrDuplicatePath", err)
	}
	b, err := s.CreateEndpoint(ctx, "/a/")
	if err != nil {
		t.Fatal(err)
	}
	if id, _, err := s.EndpointByPath(ctx, "/a/"); err != nil || id != b.ID {
		t.Fatalf("by path /a/: %d, %v", id, err)
	}
	if _, _, err := s.EndpointByPath(ctx, "/a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("/a must not match /a/: %v", err)
	}

	for range 3 {
		if err := s.AddRequest(ctx, sample(a.ID, "{}"), 1000, nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Endpoint(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Count != 3 || got.LastAt.IsZero() {
		t.Fatalf("summary = %+v, want 3 requests and a last time", got)
	}
	list, err := s.Endpoints(ctx)
	if err != nil || len(list) != 2 || list[0].Path != "/a/" || list[1].Count != 3 {
		t.Fatalf("Endpoints = %+v, %v", list, err)
	}

	if err := s.ClearRequests(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	r := sample(a.ID, "{}")
	if err := s.AddRequest(ctx, r, 1000, nil); err != nil {
		t.Fatal(err)
	}
	if r.No != 4 {
		t.Fatalf("numbering after a clear: No = %d, want 4 (numbers are not reused)", r.No)
	}

	if err := s.DeleteEndpoint(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Request(ctx, r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("request of a deleted endpoint: %v, want ErrNotFound", err)
	}
	if err := s.DeleteEndpoint(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if err := s.AddRequest(ctx, sample(a.ID, ""), 1000, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("request to a deleted endpoint: %v, want ErrNotFound", err)
	}
}

func TestRequestRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/b")
	binary := []byte{0, 1, 2, 0xff, 0xfe, '\r', '\n', 0}
	in := sample(ep.ID, string(binary))
	if err := s.AddRequest(ctx, in, 1000, nil); err != nil {
		t.Fatal(err)
	}
	out, err := s.Request(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Body, binary) {
		t.Fatalf("body = %v, want %v", out.Body, binary)
	}
	if fmt.Sprint(out.Headers) != fmt.Sprint(in.Headers) {
		t.Fatalf("headers = %v, want %v (order, case and repeats kept)", out.Headers, in.Headers)
	}
	if !out.ReceivedAt.Equal(in.ReceivedAt) || out.ReceivedAt.Location() != time.UTC {
		t.Fatalf("received at %v, want %v in UTC", out.ReceivedAt, in.ReceivedAt)
	}
	if out.Method != "POST" || out.Path != "/hooks/a" || out.RawQuery != "x=1&x=2" || out.Proto != "HTTP/1.1" ||
		out.RemoteAddr != "203.0.113.5:40000" || out.Size != int64(len(binary)) || out.Status != 200 || out.No != 1 {
		t.Fatalf("round trip lost fields: %+v", out)
	}

	empty := sample(ep.ID, "")
	empty.Body = nil // as io.ReadAll never returns, but a caller might
	if err := s.AddRequest(ctx, empty, 1000, nil); err != nil {
		t.Fatal(err)
	}
	var isNull bool
	if err := s.db.QueryRowContext(ctx, `SELECT body IS NULL FROM requests WHERE id = ?`, empty.ID).Scan(&isNull); err != nil || isNull {
		t.Fatalf("empty body stored as NULL (%v); NULL is reserved for a body that was not kept", err)
	}
	if got, _ := s.Request(ctx, empty.ID); got.Body == nil || len(got.Body) != 0 || got.BodySHA256 != nil {
		t.Fatalf("empty body: %#v / sha %v, want empty, no hash", got.Body, got.BodySHA256)
	}
}

func TestTooLargeKeepsNoBody(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/big")
	r := sample(ep.ID, "partial bytes that must not be kept")
	r.TooLarge, r.Size, r.Status = true, 20<<20, 413
	if err := s.AddRequest(ctx, r, 1000, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Request(ctx, r.ID)
	if got.Body != nil || !got.TooLarge || got.Size != 20<<20 || got.Status != 413 {
		t.Fatalf("too large: body %q, %+v", got.Body, got)
	}
}

func TestRetentionPrunesOldest(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/r")
	other, _ := s.CreateEndpoint(ctx, "/other")
	if err := s.AddRequest(ctx, sample(other.ID, "x"), 3, nil); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err := s.AddRequest(ctx, sample(ep.ID, "x"), 3, nil); err != nil {
			t.Fatal(err)
		}
	}
	rs, err := s.Requests(ctx, ep.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var nos []int64
	for _, r := range rs {
		nos = append(nos, r.No)
	}
	if fmt.Sprint(nos) != "[5 4 3]" {
		t.Fatalf("kept %v, want [5 4 3] newest first", nos)
	}
	if o, _ := s.Endpoint(ctx, other.ID); o.Count != 1 {
		t.Fatalf("pruning touched another endpoint: %d left", o.Count)
	}
}

func TestUnmatchedRegister(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/known")
	if err := s.AddRequest(ctx, sample(ep.ID, "x"), 1000, nil); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		r := sample(Unmatched, "y")
		r.Status = 404
		if err := s.AddRequest(ctx, r, 3, nil); err != nil {
			t.Fatal(err)
		}
	}
	u, err := s.Endpoint(ctx, Unmatched)
	if err != nil || u.Count != 3 {
		t.Fatalf("Unmatched summary %+v, %v; want 3 after pruning", u, err)
	}
	rs, _ := s.Requests(ctx, Unmatched, 0, 10)
	if len(rs) != 3 || rs[0].No != 4 || rs[0].EndpointID != Unmatched {
		t.Fatalf("Unmatched requests %+v", rs)
	}
	if e, _ := s.Endpoint(ctx, ep.ID); e.Count != 1 {
		t.Fatalf("endpoint count %d, want 1", e.Count)
	}
	if err := s.ClearRequests(ctx, Unmatched); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.Endpoint(ctx, Unmatched); u.Count != 0 {
		t.Fatalf("Unmatched after clear: %d", u.Count)
	}
	if e, _ := s.Endpoint(ctx, ep.ID); e.Count != 1 {
		t.Fatalf("clearing Unmatched touched an endpoint")
	}
	if err := s.ClearRequests(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("clear unknown endpoint: %v", err)
	}
}

func TestPagingAndAfter(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/p")
	var ids []int64
	for range 5 {
		r := sample(ep.ID, "x")
		if err := s.AddRequest(ctx, r, 1000, nil); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	page, _ := s.Requests(ctx, ep.ID, ids[3], 2)
	if len(page) != 2 || page[0].ID != ids[2] || page[1].ID != ids[1] {
		t.Fatalf("page before %d: %+v", ids[3], page)
	}
	after, _ := s.RequestsAfter(ctx, ep.ID, ids[2])
	if len(after) != 2 || after[0].ID != ids[3] || after[1].ID != ids[4] {
		t.Fatalf("after %d: %+v", ids[2], after)
	}
	if page[0].Body != nil {
		t.Fatal("list queries must not load bodies")
	}
}

func TestDeliveries(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/d")
	var rs []*Request
	for _, b := range []string{"evt-1", "evt-2", "evt-1", "", "evt-1"} {
		r := sample(ep.ID, b)
		if err := s.AddRequest(ctx, r, 1000, nil); err != nil {
			t.Fatal(err)
		}
		rs = append(rs, r)
	}
	var got []int
	for _, r := range rs {
		n, err := s.Deliveries(ctx, *r)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, n)
	}
	if fmt.Sprint(got) != "[1 1 2 1 3]" {
		t.Fatalf("deliveries %v, want [1 1 2 1 3]", got)
	}
}

// TestConcurrentAdds is AC-9 at the storage level: 100 concurrent writers to
// one endpoint, every request stored with a distinct number.
func TestConcurrentAdds(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/c")
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for range 100 {
		wg.Go(func() {
			if err := s.AddRequest(ctx, sample(ep.ID, "x"), 1000, nil); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	rs, _ := s.Requests(ctx, ep.ID, 0, 1000)
	seen := map[int64]bool{}
	for _, r := range rs {
		seen[r.No] = true
	}
	if len(rs) != 100 || len(seen) != 100 {
		t.Fatalf("%d stored, %d distinct numbers; want 100 and 100", len(rs), len(seen))
	}
}

func TestConfigAndCounter(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/cfg")
	if err := s.SetConfig(ctx, ep.ID, `{"default":{"status":418}}`); err != nil {
		t.Fatal(err)
	}
	if _, cfg, _ := s.EndpointByPath(ctx, "/cfg"); cfg != `{"default":{"status":418}}` {
		t.Fatalf("config %q", cfg)
	}
	if err := s.SetConfig(ctx, 999, "{}"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("config of an unknown endpoint: %v", err)
	}

	var seen []int64
	decide := func(n int64) (int, string) { seen = append(seen, n); return 500, fmt.Sprintf(`{"counter":%d}`, n) }
	for range 3 {
		if err := s.AddRequest(ctx, sample(ep.ID, "x"), 1000, decide); err != nil {
			t.Fatal(err)
		}
	}
	refused := sample(ep.ID, "x")
	refused.Status, refused.Outcome = 400, `{"action":"refused"}`
	if err := s.AddRequest(ctx, refused, 1000, nil); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.Endpoint(ctx, ep.ID); e.Counter != 3 {
		t.Fatalf("counter %d, want 3 (a request without decide does not count)", e.Counter)
	}
	if err := s.ResetCounter(ctx, ep.ID); err != nil {
		t.Fatal(err)
	}
	r := sample(ep.ID, "x")
	if err := s.AddRequest(ctx, r, 1000, decide); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(seen) != "[1 2 3 1]" || r.Status != 500 || r.Outcome != `{"counter":1}` {
		t.Fatalf("decide saw %v; request %d %q", seen, r.Status, r.Outcome)
	}
	if err := s.UpdateOutcome(ctx, r.ID, 503, `{"delay_ms":12}`); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Request(ctx, r.ID); got.Status != 503 || got.Outcome != `{"delay_ms":12}` {
		t.Fatalf("after update: %d %q", got.Status, got.Outcome)
	}
	changed, _ := s.RequestsChanged(ctx, ep.ID, r.ID, time.Now().Add(-time.Minute).UnixMilli())
	if len(changed) != 1 || changed[0].ID != r.ID {
		t.Fatalf("changed rows %+v, want only the updated one", changed)
	}
	if later, _ := s.RequestsChanged(ctx, ep.ID, r.ID, time.Now().Add(time.Minute).UnixMilli()); len(later) != 0 {
		t.Fatal("rows changed before since must not be listed")
	}
	if got, _ := s.Request(ctx, refused.ID); got.Outcome != `{"action":"refused"}` || got.Status != 400 {
		t.Fatalf("refused stored as %d %q", got.Status, got.Outcome)
	}
}

// TestDecideIsExactUnderConcurrency is AC-9 of response emulation at the
// storage level: 99 concurrent requests, every 3rd decided, exactly 33.
func TestDecideIsExactUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/every3")
	var wg sync.WaitGroup
	for range 99 {
		wg.Go(func() {
			err := s.AddRequest(ctx, sample(ep.ID, "x"), 1000, func(n int64) (int, string) {
				if n%3 == 0 {
					return 500, "rule"
				}
				return 200, "default"
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	rs, _ := s.Requests(ctx, ep.ID, 0, 1000)
	rule := 0
	for _, r := range rs {
		if r.Outcome == "rule" {
			rule++
		}
	}
	if len(rs) != 99 || rule != 33 {
		t.Fatalf("%d stored, %d rule matches; want 99 and 33", len(rs), rule)
	}
}

func TestSetConfigIf(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, err := s.CreateEndpoint(ctx, "/cas")
	if err != nil {
		t.Fatal(err)
	}
	// Two writers that both read the same (empty) configuration: one wins.
	ok1, err1 := s.SetConfigIf(ctx, ep.ID, "", `{"default":{"status":201}}`)
	ok2, err2 := s.SetConfigIf(ctx, ep.ID, "", `{"default":{"status":202}}`)
	if err1 != nil || err2 != nil || !ok1 || ok2 {
		t.Fatalf("first %v %v, second %v %v", ok1, err1, ok2, err2)
	}
	got, _ := s.Endpoint(ctx, ep.ID)
	if got.Config != `{"default":{"status":201}}` {
		t.Fatalf("config %q", got.Config)
	}
	if _, err := s.SetConfigIf(ctx, 999, "", "{}"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing endpoint: %v", err)
	}
}
