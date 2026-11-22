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
	if rp, _ := f.s.Replays(ctx, f.normal[0]); len(rp) != 1 || !rp[0].BodyDropped {
		t.Fatalf("the replay in the list: %+v", rp)
	} else if full, _ := f.s.Replay(ctx, rp[0].ID); len(full.Body) != 0 || !full.BodyDropped {
		t.Fatal("the replay's answer body stayed, or is not said to be dropped")
	}
	if fw, _ := f.s.ForwardOf(ctx, f.normal[0]); len(fw.Body) != 0 || !fw.BodyDropped || fw.Status != 200 {
		t.Fatalf("the forward's answer: %d bytes, dropped %v, status %d", len(fw.Body), fw.BodyDropped, fw.Status)
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
		{Storage{Used: 99, Limit: 100, Pinned: 90, Stuck: true}, "every request left is pinned"},
		{Storage{Used: 50, Limit: 100, Stuck: true}, "every request left is pinned"},
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

// AC-B2 while requests arrive: a body that arrived during a pass is dropped
// before any older normal row goes, and the pass is not stuck while
// unpinned requests are left.
func TestEvictTakesInArrivals(t *testing.T) {
	ctx := context.Background()
	f := fillEvict(t)
	var late int64
	f.s.evictStarted = func() {
		r := sample(f.ep.ID, "")
		r.Body, r.Size = bodyOf(500), 100<<10
		if err := f.s.AddRequest(ctx, r, Keep{Normal: 1000, Anomalies: 1000}, nil); err != nil {
			t.Error(err)
		}
		late = r.ID
	}
	used, _ := f.s.Usage(ctx)
	// Under every normal body (the late one too) and a few normal rows.
	e, err := f.s.Evict(ctx, (used-25*100<<10)*100/EvictTarget)
	if err != nil {
		t.Fatal(err)
	}
	if e.Rows == 0 || e.Stuck {
		t.Fatalf("no normal row went, or stuck: %+v", e)
	}
	// The late request's row goes after every older one; while it is kept,
	// its body must be gone.
	switch r, err := f.s.Request(ctx, late); {
	case err == ErrNotFound:
		if rows, _ := f.state(t, f.normal); rows != 0 {
			t.Fatalf("the late row went before %d older ones", rows)
		}
	case err != nil:
		t.Fatal(err)
	case !r.BodyDropped:
		t.Fatalf("an older normal row went while the late body was kept (%+v)", e)
	}
	// Stuck only once nothing unpinned is left, late arrivals taken in.
	f.s.evictStarted = func() {
		r := sample(f.ep.ID, "")
		r.Body, r.Size = bodyOf(501), 100<<10
		f.s.AddRequest(ctx, r, Keep{Normal: 1000, Anomalies: 1000}, nil)
		late = r.ID
	}
	if e, err = f.s.Evict(ctx, 64<<10); err != nil || !e.Stuck {
		t.Fatalf("under the pins: %+v %v", e, err)
	}
	if _, err := f.s.Request(ctx, late); err != ErrNotFound {
		t.Fatalf("stuck with an unpinned request left: %v", err)
	}
}

// The Unmatched register has no lanes: a size eviction moves both its
// marks, as retention does.
func TestEvictUnmatchedMarks(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	for i := range 5 {
		r := sample(Unmatched, "")
		r.Body, r.Size = bodyOf(i), 100<<10
		if err := s.AddRequest(ctx, r, Keep{Normal: 1000, Anomalies: 1000}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if e, err := s.Evict(ctx, 64<<10); err != nil || e.Rows != 5 {
		t.Fatalf("%+v %v", e, err)
	}
	k, err := s.KeptState(ctx, Unmatched)
	if err != nil || k.Pruned != 5 || k.NormalAt.IsZero() || k.AnomalyAt.IsZero() {
		t.Fatalf("unmatched marks %+v %v", k, err)
	}
}

// DropBodies leaves a pinned request whole, its answers too.
func TestDropBodiesLeavesPins(t *testing.T) {
	ctx := context.Background()
	f := fillEvict(t)
	rp := sampleReplay(f.pinned, 200)
	rp.Body = []byte("kept")
	if err := f.s.AddReplay(ctx, rp); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DropBodies(ctx, f.pinned); err != nil {
		t.Fatal(err)
	}
	if full, _ := f.s.Replay(ctx, rp.ID); string(full.Body) != "kept" || full.BodyDropped {
		t.Fatalf("a pinned request's replay: %q dropped %v", full.Body, full.BodyDropped)
	}
	if r, _ := f.s.Request(ctx, f.pinned); r.BodyDropped || len(r.Body) == 0 {
		t.Fatal("a pinned body dropped")
	}
}

// A re-check gives a request whose body was dropped after the marks were
// dropped its order mark back; a request keeps the mark it had when its
// body was dropped before.
func TestOrderMarksAndDroppedBodies(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/o")
	var ids []int64
	for i, v := range []float64{10, 20, 30} {
		r := sample(ep.ID, fmt.Sprintf(`{"n":%d}`, i))
		r.Delivery = &Delivery{Event: fmt.Sprint("e", i), Order: &v, OrderDef: "body /n as number"}
		if err := s.AddRequest(ctx, r, Keep{Normal: 100}, nil); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	mark := func(id int64) (v *float64) {
		s.db.QueryRowContext(ctx, `SELECT order_value FROM event_seen WHERE request_id = ?`, id).Scan(&v)
		return v
	}
	if err := s.DropBodies(ctx, ids[0]); err != nil {
		t.Fatal(err)
	}
	marks, err := s.ForgetOrderMarks(ctx, ep.ID)
	if err != nil || len(marks) != 2 || marks[ids[1]] != 20 || marks[ids[2]] != 30 {
		t.Fatalf("forgot %v %v", marks, err)
	}
	if m := mark(ids[0]); m == nil || *m != 10 {
		t.Fatal("a dropped request lost its mark")
	}
	// Dropped during the re-check: its mark comes back with the note.
	if err := s.DropBodies(ctx, ids[1]); err != nil {
		t.Fatal(err)
	}
	back := marks[ids[1]]
	if err := s.NoteVerdicts(ctx, ids[1], "[]", &back); err != nil {
		t.Fatal(err)
	}
	if m := mark(ids[1]); m == nil || *m != 20 {
		t.Fatalf("mark after the note: %v", m)
	}
}

// Order marks are compared only with marks of the same definition (or
// recorded before definitions were).
func TestOrderMarksOfOtherDefinitions(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/o")
	add := func(i int, v float64, def string) *Relation {
		var got Relation
		r := sample(ep.ID, fmt.Sprintf(`{"n":%d}`, i))
		r.Delivery = &Delivery{Event: fmt.Sprint("e", i), Order: &v, OrderDef: def,
			Relate: func(rel Relation) (string, string) { got = rel; return "", "[]" }}
		if err := s.AddRequest(ctx, r, Keep{Normal: 100}, nil); err != nil {
			t.Fatal(err)
		}
		return &got
	}
	add(0, 1e12, "body /ts as time")
	if rel := add(1, 5, "body /seq as number"); rel.High != nil {
		t.Fatalf("compared with a mark of another definition: %+v", rel.High)
	}
	if rel := add(2, 3, "body /seq as number"); rel.High == nil || rel.High.Value != 5 {
		t.Fatalf("not compared with the same definition: %+v", rel.High)
	}
	s.db.ExecContext(ctx, `UPDATE event_seen SET order_def = '' WHERE order_value = 1e12`) // a row from before
	if rel := add(3, 4, "body /seq as number"); rel.High == nil || rel.High.Value != 1e12 {
		t.Fatalf("a mark from before definitions not compared: %+v", rel.High)
	}
}

// The WAL file is cut back after a checkpoint.
func TestWALLimit(t *testing.T) {
	s := openTemp(t)
	var n int64
	if err := s.db.QueryRowContext(context.Background(), `PRAGMA journal_size_limit`).Scan(&n); err != nil || n != WALLimit {
		t.Fatalf("journal_size_limit %d %v", n, err)
	}
}
