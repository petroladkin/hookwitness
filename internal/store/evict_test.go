package store

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// bodyOf is a 100 KiB body that does not compress into fewer pages.
func bodyOf(seed int) []byte {
	b := make([]byte, 100<<10)
	x := uint32(seed*2654435761 + 1)
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x)
	}
	return b
}

type evictFixture struct {
	s                 *Store
	ep                Endpoint
	normal, anomalies []int64
	pinned            int64
}

// fillEvict keeps, oldest first: 20 normal requests, 10 anomalies, then a
// pinned normal one, each with a 100 KiB body; the first normal request
// has a replay and a forward with answer bodies.
func fillEvict(t *testing.T) evictFixture {
	t.Helper()
	ctx := context.Background()
	f := evictFixture{s: openTemp(t)}
	f.ep, _ = f.s.CreateEndpoint(ctx, "/big")
	keep := Keep{Normal: 1000, Anomalies: 1000}
	add := func(i int, status int) int64 {
		r := sample(f.ep.ID, "")
		r.Body, r.Size, r.Status = bodyOf(i), 100<<10, status
		r.Outcome = fmt.Sprintf(`{"source":"default","action":"reply","status":%d}`, status)
		if err := f.s.AddRequest(ctx, r, keep, nil); err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	for i := range 20 {
		f.normal = append(f.normal, add(i, 200))
	}
	for i := range 10 {
		f.anomalies = append(f.anomalies, add(100+i, 500))
	}
	f.pinned = add(200, 200)
	if err := f.s.SetPinned(ctx, f.pinned, true); err != nil {
		t.Fatal(err)
	}
	rp := sampleReplay(f.normal[0], 200)
	rp.Body = bodyOf(300)
	if err := f.s.AddReplay(ctx, rp); err != nil {
		t.Fatal(err)
	}
	fw := sampleForward(f.normal[0], 200)
	fw.Body = bytes.Repeat([]byte("f"), 64<<10)
	if err := f.s.FinishForward(ctx, fw, 200, forwardedOK); err != nil {
		t.Fatal(err)
	}
	return f
}

// state reads which of ids still exist and which have their body.
func (f evictFixture) state(t *testing.T, ids []int64) (rows, bodies int) {
	t.Helper()
	for _, id := range ids {
		r, err := f.s.Request(context.Background(), id)
		if err != nil {
			continue
		}
		rows++
		if !r.BodyDropped && len(r.Body) > 0 {
			bodies++
		}
	}
	return
}

// TestEvictOrder is AC-B1 and AC-B2: over the budget, bodies of normal
// requests go first, oldest first; normal rows only once no normal body is
// left; then anomalies' bodies, then anomalies; pinned never. A dropped
// body keeps the request's metadata, verdicts and digest; replay and
// forward answers go with it.
func TestEvictOrder(t *testing.T) {
	ctx := context.Background()
	f := fillEvict(t)
	used, _ := f.s.Usage(ctx)

	// A budget a few bodies under the size: only the oldest normal bodies go.
	e, err := f.s.Evict(ctx, used-300<<10)
	if err != nil {
		t.Fatal(err)
	}
	if e.Bodies == 0 || e.Rows != 0 || e.After > (used-300<<10)/100*EvictTarget || e.Stuck {
		t.Fatalf("first pass: %+v (used %d)", e, used)
	}
	rows, bodies := f.state(t, f.normal)
	if rows != 20 || bodies != 20-e.Bodies {
		t.Fatalf("normal: %d rows, %d bodies after dropping %d", rows, bodies, e.Bodies)
	}
	for i, id := range f.normal {
		r, _ := f.s.Request(ctx, id)
		if r.BodyDropped != (i < e.Bodies) {
			t.Fatalf("normal #%d dropped %v: not oldest first (%d dropped)", i, r.BodyDropped, e.Bodies)
		}
	}
	first, _ := f.s.Request(ctx, f.normal[0])
	if !first.BodyDropped || first.Body != nil || first.Kept != 0 || first.Size != 100<<10 || len(first.BodySHA256) == 0 ||
		first.Status != 200 || len(first.Headers) == 0 || first.Outcome == "" {
		t.Fatalf("a dropped request lost more than its body: %+v", first)
	}
	if rp, _ := f.s.Replays(ctx, f.normal[0]); len(rp) != 1 {
		t.Fatal("the replay went")
	} else if full, _ := f.s.Replay(ctx, rp[0].ID); len(full.Body) != 0 {
		t.Fatal("the replay's answer body stayed")
	}
	if fw, _ := f.s.ForwardOf(ctx, f.normal[0]); len(fw.Body) != 0 || fw.Status != 200 {
		t.Fatalf("the forward's answer: %d bytes, status %d", len(fw.Body), fw.Status)
	}
	if rows, bodies := f.state(t, f.anomalies); rows != 10 || bodies != 10 {
		t.Fatalf("anomalies touched: %d rows, %d bodies", rows, bodies)
	}

	// Under every normal body and five anomalies' bodies: normal rows go
	// too, then the oldest anomalies' bodies.
	used, _ = f.s.Usage(ctx)
	need := int64(bodies+5) * 100 << 10
	e, err = f.s.Evict(ctx, (used-need)*100/EvictTarget)
	if err != nil {
		t.Fatal(err)
	}
	if rows, _ := f.state(t, f.normal); rows != 0 {
		t.Fatalf("%d normal rows left after anomalies' bodies went (%+v)", rows, e)
	}
	rows, bodies = f.state(t, f.anomalies)
	if rows != 10 || bodies == 10 || bodies == 0 {
		t.Fatalf("anomalies: %d rows, %d bodies (%+v)", rows, bodies, e)
	}
	for i, id := range f.anomalies {
		r, _ := f.s.Request(ctx, id)
		if r.BodyDropped != (i < 10-bodies) {
			t.Fatalf("anomaly #%d dropped %v: not oldest first", i, r.BodyDropped)
		}
	}
	k, _ := f.s.KeptState(ctx, f.ep.ID)
	if k.Pruned != 20 || k.NormalAt.IsZero() {
		t.Fatalf("pruned record %+v", k)
	}

	// Below what pins hold: everything else goes, the pin stays, stuck.
	e, err = f.s.Evict(ctx, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	if !e.Stuck {
		t.Fatalf("not stuck under the pins: %+v", e)
	}
	if rows, _ := f.state(t, f.anomalies); rows != 0 {
		t.Fatalf("%d anomalies left", rows)
	}
	p, err := f.s.Request(ctx, f.pinned)
	if err != nil || p.BodyDropped || !bytes.Equal(p.Body, bodyOf(200)) {
		t.Fatalf("the pinned request: %v, dropped %v", err, p.BodyDropped)
	}
	if pb, _ := f.s.PinnedBytes(ctx); pb < 100<<10 {
		t.Fatalf("pinned bytes %d", pb)
	}
}

// TestEvictUnderBudget: nothing happens under the budget or without one.
func TestEvictUnderBudget(t *testing.T) {
	ctx := context.Background()
	f := fillEvict(t)
	used, _ := f.s.Usage(ctx)
	for _, budget := range []int64{0, used, used * 2} {
		if e, err := f.s.Evict(ctx, budget); err != nil || e.Bodies+e.Rows != 0 || e.Stuck {
			t.Fatalf("budget %d: %+v %v", budget, e, err)
		}
	}
}

// The used size drops as bodies are dropped, and giving the free pages
// back to the file system does not change it.
func TestUsageAfterReclaim(t *testing.T) {
	ctx := context.Background()
	f := fillEvict(t)
	before, _ := f.s.Usage(ctx)
	e, _ := f.s.Evict(ctx, before-1<<20)
	after, _ := f.s.Usage(ctx)
	if after >= before || e.After != after {
		t.Fatalf("used %d → %d (%+v)", before, after, e)
	}
	if _, err := f.s.Reclaim(ctx, 0); err != nil {
		t.Fatal(err)
	}
	// Giving pages back moves a few bookkeeping pages, nothing more.
	if again, _ := f.s.Usage(ctx); again < after-64<<10 || again > after+64<<10 {
		t.Fatalf("used %d after reclaim, %d before", again, after)
	}
}

// TestCatchingDuringEviction is AC-B4: requests are stored while an
// eviction runs (-race); both finish, nothing fails, and the new requests
// are evicted only after the older ones.
func TestCatchingDuringEviction(t *testing.T) {
	ctx := context.Background()
	f := fillEvict(t)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan error, 1)
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			r := sample(f.ep.ID, "")
			r.Body, r.Size = bodyOf(1000+i), 100<<10
			if err := f.s.AddRequest(ctx, r, Keep{Normal: 1000, Anomalies: 1000}, nil); err != nil {
				select {
				case errs <- err:
				default:
				}
				return
			}
		}
	})
	start := time.Now()
	var e Evicted
	var err error
	for range 5 {
		if e, err = f.s.Evict(ctx, 1<<20); err != nil {
			break
		}
	}
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errs:
		t.Fatalf("catching failed during eviction: %v", err)
	default:
	}
	if p, _ := f.s.Request(ctx, f.pinned); p.BodyDropped {
		t.Fatal("the pin was evicted")
	}
	t.Logf("evicted %+v in %s while catching", e, time.Since(start))
}

// A body over LargeBody asks the housekeeping for a size check at once.
func TestLargeBodyNudges(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/n")
	r := sample(ep.ID, "")
	r.Body, r.Size = bytes.Repeat([]byte("x"), LargeBody+1), LargeBody+1
	if err := s.AddRequest(ctx, r, Keep{Normal: 10}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.nudge:
	default:
		t.Fatal("no nudge after a large body")
	}
	r = sample(ep.ID, "small")
	s.AddRequest(ctx, r, Keep{Normal: 10}, nil)
	select {
	case <-s.nudge:
		t.Fatal("a nudge after a small body")
	default:
	}
}

// TestStorageWarnings: the numbers and what they warn of.
func TestStorageWarnings(t *testing.T) {
	ctx := context.Background()
	f := fillEvict(t)
	st, err := f.s.Storage(ctx, 0)
	if err != nil || st.Used <= 0 || st.Pinned < 100<<10 || st.Limit != 0 {
		t.Fatalf("storage %+v %v", st, err)
	}
	if !st.VolumeKnown || st.VolumeTotal <= 0 || st.VolumeFree <= 0 {
		t.Fatalf("the volume of a test database: %+v", st)
	}
	for _, c := range []struct {
		st   Storage
		want string
	}{
		{Storage{Used: 95, Limit: 100}, "at 95 % of its size limit"},
		{Storage{Used: 89, Limit: 100}, ""},
		{Storage{Used: 95, Limit: 100, Pinned: 120}, "pinned requests alone hold more"},
		{Storage{Used: 10, VolumeKnown: true, VolumeFree: 900 << 20, VolumeTotal: 100 << 30}, "only 900.0 MiB free of 100.0 GiB"},
		{Storage{Used: 10, VolumeKnown: true, VolumeFree: 4 << 30, VolumeTotal: 100 << 30}, "only 4.0 GiB free"},
		{Storage{Used: 10, VolumeKnown: true, VolumeFree: 6 << 30, VolumeTotal: 100 << 30}, ""},
		{Storage{Used: 10}, ""},
	} {
		got := strings.Join(c.st.Warnings(), " | ")
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%+v: %q, want %q", c.st, got, c.want)
		}
	}
}
