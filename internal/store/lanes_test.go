package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/petroladkin/hookwitness/internal/emulate"
)

// outcomeJSON is a stored outcome for tests.
func outcomeJSON(action string, status int) string {
	b, _ := json.Marshal(emulate.Outcome{Action: action, Status: status})
	return string(b)
}

// classCase is one request's parts and whether it is an anomaly.
type classCase struct {
	outcome     string
	status      int
	tooLarge    bool
	verdict     string
	redelivered bool
}

var classCases = []classCase{
	{outcomeJSON(emulate.ActReply, 200), 200, false, "pass", false},
	{outcomeJSON(emulate.ActReply, 204), 204, false, "", false},
	{outcomeJSON(emulate.ActReply, 299), 299, false, "n/v", false},
	{outcomeJSON(emulate.ActReply, 300), 300, false, "", false},
	{outcomeJSON(emulate.ActReply, 500), 500, false, "pass", false},
	{outcomeJSON(emulate.ActReply, 404), 404, false, "", false},
	{outcomeJSON(emulate.ActReply, 200), 200, false, "fail", false},
	{outcomeJSON(emulate.ActReply, 200), 200, false, "pass", true},
	{outcomeJSON(emulate.ActTimeout, 0), 0, false, "", false},
	{outcomeJSON(emulate.ActDrop, 0), 0, false, "", false},
	{outcomeJSON(emulate.ActRefused, 401), 401, false, "fail", false},
	{outcomeJSON(emulate.ActError, 500), 500, false, "", false},
	{outcomeJSON(emulate.ActAbandoned, 0), 0, false, "", false},
	{outcomeJSON(emulate.ActReply, 413), 413, true, "", false},
	{"", 200, false, "", false}, // caught before outcomes were recorded
	{"", 500, false, "", false},
	{"not json", 200, false, "", false},
}

// TestAnomalyMatchesGo: the store's SQL class of every case equals
// emulate.Anomalous, for new requests and for rows migration 0010
// classified.
func TestAnomalyMatchesGo(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/class")
	want := map[int64]bool{}
	for i, c := range classCases {
		r := sample(ep.ID, fmt.Sprint(i))
		r.Outcome, r.Status, r.TooLarge, r.Verdict = c.outcome, c.status, c.tooLarge, c.verdict
		if c.redelivered {
			r.Signatures = []Signature{{1, "d-same"}}
			// The earlier delivery of the same signature.
			first := sample(ep.ID, "first")
			first.Outcome, first.Status, first.Signatures = outcomeJSON(emulate.ActReply, 200), 200, []Signature{{1, "d-same"}}
			if err := s.AddRequest(ctx, first, Keep{Normal: 100, Anomalies: 100}, nil); err != nil {
				t.Fatal(err)
			}
			want[first.ID] = true // both deliveries of a redelivery
		}
		if err := s.AddRequest(ctx, r, Keep{Normal: 100, Anomalies: 100}, nil); err != nil {
			t.Fatal(err)
		}
		want[r.ID] = emulate.Anomalous(emulate.ParseOutcome(c.outcome, c.status), c.tooLarge, c.verdict, c.redelivered)
	}
	for id, w := range want {
		var got bool
		if err := s.db.QueryRowContext(ctx, `SELECT anomaly FROM requests WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != w {
			r, _ := s.Request(ctx, id)
			t.Errorf("request %d (outcome %q, status %d, too large %v, verdict %q): anomaly %v, Go says %v",
				id, r.Outcome, r.Status, r.TooLarge, r.Verdict, got, w)
		}
	}
	// Every mark other than "ok" is an anomaly: timeout, drop and bad all
	// fail Good.
	for _, o := range []emulate.Outcome{{Action: emulate.ActTimeout}, {Action: emulate.ActAbandoned}, {Action: emulate.ActDrop},
		{Action: emulate.ActReply, Status: 503}} {
		if !emulate.Anomalous(o, false, "", false) {
			t.Errorf("%+v marked on the strip but not an anomaly", o)
		}
	}
}

// lanesOf counts a register's unpinned rows per lane and pinned rows.
func lanesOf(t *testing.T, s *Store, reg int64) Kept {
	t.Helper()
	k, err := s.KeptState(context.Background(), reg)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestLanesExact: after every insert each lane holds at most its count,
// the newest by id; pinned requests stay whatever comes.
func TestLanesExact(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/lanes")
	keep := Keep{Normal: 30, Anomalies: 10}
	rng := rand.New(rand.NewPCG(1, 2))
	var pinned []int64
	ids := map[bool][]int64{} // per class, in order
	for i := range 600 {
		r := sample(ep.ID, fmt.Sprint(i))
		bad := rng.IntN(5) == 0
		if bad {
			r.Outcome, r.Status = outcomeJSON(emulate.ActReply, 500), 500
		} else {
			r.Outcome = outcomeJSON(emulate.ActReply, 200)
		}
		if err := s.AddRequest(ctx, r, keep, nil); err != nil {
			t.Fatal(err)
		}
		if i%150 == 7 {
			if err := s.SetPinned(ctx, r.ID, true); err != nil {
				t.Fatal(err)
			}
			pinned = append(pinned, r.ID)
			continue
		}
		ids[bad] = append(ids[bad], r.ID)
		k := lanesOf(t, s, ep.ID)
		if k.Normal > keep.Normal || k.Anomalies > keep.Anomalies {
			t.Fatalf("after insert %d: %d normal, %d anomalies, limits %+v", i, k.Normal, k.Anomalies, keep)
		}
	}
	for bad, lim := range map[bool]int{false: keep.Normal, true: keep.Anomalies} {
		wantIDs := ids[bad][len(ids[bad])-lim:]
		rows, err := s.db.QueryContext(ctx, `SELECT id FROM requests WHERE endpoint_id = ? AND pinned = 0 AND anomaly = ? ORDER BY id`, ep.ID, bad)
		if err != nil {
			t.Fatal(err)
		}
		var got []int64
		for rows.Next() {
			var id int64
			rows.Scan(&id)
			got = append(got, id)
		}
		rows.Close()
		if fmt.Sprint(got) != fmt.Sprint(wantIDs) {
			t.Errorf("lane anomaly=%v kept %v, want the newest %v", bad, got, wantIDs)
		}
	}
	for _, id := range pinned {
		if _, err := s.Request(ctx, id); err != nil {
			t.Errorf("pinned request %d lost: %v", id, err)
		}
	}
	k := lanesOf(t, s, ep.ID)
	if k.Pinned != len(pinned) || k.Pruned != int64(600-len(pinned)-keep.Normal-keep.Anomalies) {
		t.Errorf("kept state %+v, want %d pinned and the rest pruned", k, len(pinned))
	}
	if k.NormalAt.IsZero() || k.AnomalyAt.IsZero() {
		t.Errorf("watermarks not moved: %+v", k)
	}
	// Unpinning returns a request to its lane: the next insert prunes it.
	if err := s.SetPinned(ctx, pinned[0], false); err != nil {
		t.Fatal(err)
	}
	r := sample(ep.ID, "after")
	r.Outcome = outcomeJSON(emulate.ActReply, 200)
	s.AddRequest(ctx, r, keep, nil)
	if _, err := s.Request(ctx, pinned[0]); err != ErrNotFound {
		t.Errorf("an unpinned old request survived the next insert: %v", err)
	}
	if err := s.SetPinned(ctx, 1<<40, true); err != ErrNotFound {
		t.Errorf("pin of a missing request: %v", err)
	}
}

// TestNoAnomalyLane: with no anomaly lane a register keeps its newest
// requests whatever their class, exactly as before lanes.
func TestNoAnomalyLane(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/one")
	for i := range 50 {
		r := sample(ep.ID, fmt.Sprint(i))
		r.Outcome, r.Status = outcomeJSON(emulate.ActReply, 200+300*(i%2)), 200+300*(i%2)
		if err := s.AddRequest(ctx, r, Keep{Normal: 7}, nil); err != nil {
			t.Fatal(err)
		}
	}
	reqs, _ := s.Requests(ctx, ep.ID, 0, 100)
	if len(reqs) != 7 || reqs[0].No != 50 || reqs[6].No != 44 {
		t.Fatalf("kept %d, newest first from #%d", len(reqs), reqs[0].No)
	}
	if k := lanesOf(t, s, ep.ID); k.NormalAt.IsZero() || !k.NormalAt.Equal(k.AnomalyAt) {
		t.Fatalf("one lane moves both watermarks: %+v", k)
	}
}

// TestClassChangeMovesLane: an outcome update or a re-check moves the
// request between lanes; a redelivery moves its earlier delivery.
func TestClassChangeMovesLane(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/move")
	keep := Keep{Normal: 3, Anomalies: 3}
	add := func(status int, sigs ...Signature) *Request {
		r := sample(ep.ID, fmt.Sprint(status, sigs))
		r.Outcome, r.Status, r.Signatures = outcomeJSON(emulate.ActReply, status), status, sigs
		if err := s.AddRequest(ctx, r, keep, nil); err != nil {
			t.Fatal(err)
		}
		return r
	}
	held := add(200)
	// The hold ended in a timeout: an anomaly now.
	if err := s.UpdateOutcome(ctx, held.ID, 0, outcomeJSON(emulate.ActTimeout, 0)); err != nil {
		t.Fatal(err)
	}
	failing := add(200)
	if err := s.SetVerdicts(ctx, failing.ID, "fail", "[]", nil, nil); err != nil {
		t.Fatal(err)
	}
	signed := add(200, Signature{1, "d-x"})
	for range 10 { // a flood of good requests
		add(200)
	}
	for name, id := range map[string]int64{"timed out": held.ID, "re-checked failing": failing.ID} {
		if _, err := s.Request(ctx, id); err != nil {
			t.Errorf("the %s request was pruned with the normal lane: %v", name, err)
		}
	}
	if _, err := s.Request(ctx, signed.ID); err != ErrNotFound {
		t.Errorf("a normal request outlived its lane: %v", err)
	}
	// A redelivery: the earlier delivery joins the anomaly lane with it.
	first := add(200, Signature{1, "d-y"})
	add(200, Signature{1, "d-y"})
	for range 10 {
		add(200)
	}
	if _, err := s.Request(ctx, first.ID); err != nil {
		t.Errorf("a redelivered request's first delivery was pruned as normal: %v", err)
	}
}

// TestPruneByAge: the housekeeping pass removes what is past a lane's age,
// in steps of at most PruneBatch rows; catching goes on between steps.
func TestPruneByAge(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/age")
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	keep := Keep{Normal: 100_000, Anomalies: 100_000}
	for i := range 700 {
		r := sample(ep.ID, fmt.Sprint(i))
		r.ReceivedAt = base.Add(time.Duration(i) * time.Minute)
		r.Outcome = outcomeJSON(emulate.ActReply, 200)
		if i%10 == 0 {
			r.Outcome, r.Status = outcomeJSON(emulate.ActReply, 500), 500
		}
		if err := s.AddRequest(ctx, r, keep, nil); err != nil {
			t.Fatal(err)
		}
	}
	pin := int64(1) // the first request, normal and the oldest
	s.SetPinned(ctx, pin, true)
	var p Retention
	if err := json.Unmarshal([]byte(`{"normal":{"max_age":"2h"},"anomalies":{"max_age":"10h"}}`), &p); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRetention(ctx, ep.ID, p); err != nil {
		t.Fatal(err)
	}
	now := base.Add(700 * time.Minute) // the newest arrived a minute ago
	n, err := s.ToPrune(ctx, ep.ID, p, now, keep)
	if err != nil {
		t.Fatal(err)
	}
	var steps []int
	pruneHook = func(n int) {
		steps = append(steps, n)
		// Between steps the writer is free.
		r := sample(ep.ID, "between")
		r.ReceivedAt, r.Outcome = now, outcomeJSON(emulate.ActReply, 200)
		if err := s.AddRequest(ctx, r, keep, nil); err != nil {
			t.Error(err)
		}
	}
	defer func() { pruneHook = nil }()
	pruned, err := s.Prune(ctx, now, keep)
	if err != nil {
		t.Fatal(err)
	}
	pruneHook = nil
	if pruned[ep.ID] != n {
		t.Errorf("pruned %d, ToPrune promised %d", pruned[ep.ID], n)
	}
	for _, st := range steps {
		if st > PruneBatch {
			t.Errorf("a step deleted %d rows, over %d", st, PruneBatch)
		}
	}
	if len(steps) < 2 {
		t.Errorf("steps %v: want the work split", steps)
	}
	var old int
	s.db.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE endpoint_id = ? AND pinned = 0 AND (
		(anomaly = 0 AND received_at < ?) OR (anomaly = 1 AND received_at < ?))`,
		ep.ID, millis(now.Add(-2*time.Hour)), millis(now.Add(-10*time.Hour))).Scan(&old)
	if old != 0 {
		t.Errorf("%d requests past their lane's age remain", old)
	}
	if _, err := s.Request(ctx, pin); err != nil {
		t.Errorf("the pinned request went with its age: %v", err)
	}
	k := lanesOf(t, s, ep.ID)
	if got := k.NormalAt; !got.Before(now.Add(-2*time.Hour)) || got.Before(now.Add(-2*time.Hour-2*time.Minute)) {
		t.Errorf("normal watermark %v, want just before %v", got, now.Add(-2*time.Hour))
	}
	// Nothing more to do: a second pass deletes nothing.
	if again, _ := s.Prune(ctx, now, keep); again[ep.ID] != 0 {
		t.Errorf("a second pass pruned %d", again[ep.ID])
	}
}

// TestLoweredLimit: a lowered count is applied at once, step by step, and
// ToPrune says how many go before.
func TestLoweredLimit(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/lower")
	keep := Keep{Normal: 1000, Anomalies: 1000}
	for i := range 600 {
		r := sample(ep.ID, fmt.Sprint(i))
		r.Outcome = outcomeJSON(emulate.ActReply, 200)
		s.AddRequest(ctx, r, keep, nil)
	}
	p := Retention{Normal: Lane{Count: 50}}
	now := time.Now()
	if n, _ := s.ToPrune(ctx, ep.ID, p, now, keep); n != 550 {
		t.Fatalf("ToPrune %d, want 550", n)
	}
	if err := s.SetRetention(ctx, ep.ID, p); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneNow(ctx, ep.ID, now, keep); n != 550 || err != nil {
		t.Fatalf("PruneNow %d, %v", n, err)
	}
	if k := lanesOf(t, s, ep.ID); k.Normal != 50 || k.Pruned != 550 || k.Policy.Normal.Count != 50 {
		t.Fatalf("after lowering: %+v", k)
	}
	if err := s.SetRetention(ctx, Unmatched, Retention{Anomalies: Lane{Count: 3}, AnomaliesSet: true}); err == nil {
		t.Error("an anomaly lane for Unmatched was accepted")
	}
	if err := s.SetRetention(ctx, 1<<40, p); err != ErrNotFound {
		t.Errorf("a missing endpoint: %v", err)
	}
}

// TestUnmatchedOneLane: the Unmatched register has one lane, its own
// policy and counters.
func TestUnmatchedOneLane(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	if err := s.SetRetention(ctx, Unmatched, Retention{Normal: Lane{Count: 4}}); err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		r := sample(Unmatched, fmt.Sprint(i))
		r.Status, r.Outcome = 404, outcomeJSON(emulate.ActReply, 404)
		if err := s.AddRequest(ctx, r, Keep{Normal: 1000, Anomalies: 1000}, nil); err != nil {
			t.Fatal(err)
		}
	}
	k := lanesOf(t, s, Unmatched)
	if k.Normal != 4 || k.Anomalies != 0 || k.Pruned != 6 || k.NormalAt.IsZero() || k.Policy.Normal.Count != 4 {
		t.Fatalf("Unmatched: %+v", k)
	}
	s.ClearRequests(ctx, Unmatched)
	if k := lanesOf(t, s, Unmatched); k.NormalAt.Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("a clear did not move the watermark: %+v", k)
	}
}

// TestCatchWhilePruning: catching goes on while the housekeeping deletes a
// large backlog; every insert succeeds.
func TestCatchWhilePruning(t *testing.T) {
	if testing.Short() {
		t.Skip("large")
	}
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/busy")
	keep := Keep{Normal: 100_000, Anomalies: 1000}
	tx, _ := s.db.BeginTx(ctx, nil)
	for i := range 6_000 {
		tx.ExecContext(ctx, `INSERT INTO requests (endpoint_id, no, received_at, method, path, raw_query, proto, remote_addr, headers,
			body, size, status, outcome) VALUES (?, ?, ?, 'POST', '/busy', '', 'HTTP/1.1', '', '[]', x'', 0, 200, ?)`,
			ep.ID, i+1, millis(time.Now()), outcomeJSON(emulate.ActReply, 200))
	}
	tx.ExecContext(ctx, `UPDATE endpoints SET last_no = 6000 WHERE id = ?`, ep.ID)
	tx.Commit()
	s.SetRetention(ctx, ep.ID, Retention{Normal: Lane{Count: 10}})
	var wg sync.WaitGroup
	wg.Go(func() {
		if _, err := s.Prune(ctx, time.Now(), keep); err != nil {
			t.Error(err)
		}
	})
	for i := range 200 {
		r := sample(ep.ID, fmt.Sprint(i))
		r.Outcome = outcomeJSON(emulate.ActReply, 200)
		if err := s.AddRequest(ctx, r, keep, nil); err != nil {
			t.Fatalf("insert %d during the prune: %v", i, err)
		}
	}
	wg.Wait()
	s.Prune(ctx, time.Now(), keep)
	if k := lanesOf(t, s, ep.ID); k.Normal != 10 {
		t.Fatalf("after the prune: %+v", k)
	}
}

// TestRetentionMigration: a 0.2 database opens, keeps every row, its rows
// classified as the Go function would, and the strip's "not kept" time.
func TestRetentionMigration(t *testing.T) {
	ctx := context.Background()
	p := tempDB(t)
	old := fstest.MapFS{}
	ms, err := loadMigrations(migrationFS)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.version < 10 {
			old["migrations/"+m.name] = &fstest.MapFile{Data: []byte(m.sql)}
		}
	}
	s, err := open(ctx, p, old)
	if err != nil {
		t.Fatal(err)
	}
	var ep int64
	s.db.QueryRowContext(ctx, `INSERT INTO endpoints (path, created_at, last_no) VALUES ('/old', 0, 40) RETURNING id`).Scan(&ep)
	want := map[int64]bool{}
	for i, c := range classCases {
		var id int64
		// Numbers from 3 on: the first two were pruned by 0.2.
		if err := s.db.QueryRowContext(ctx, `INSERT INTO requests (endpoint_id, no, received_at, method, path, raw_query, proto,
			remote_addr, headers, body, size, too_large, status, outcome, verdict) VALUES (?, ?, ?, 'POST', '/old', '', 'HTTP/1.1',
			'', '[]', x'', 0, ?, ?, ?, ?) RETURNING id`, ep, i+3, 1000+i, c.tooLarge, c.status, c.outcome, c.verdict).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if c.redelivered {
			var first int64
			s.db.QueryRowContext(ctx, `INSERT INTO requests (endpoint_id, no, received_at, method, path, raw_query, proto,
				remote_addr, headers, body, size, status, outcome) VALUES (?, 99, 999, 'POST', '/old', '', 'HTTP/1.1', '', '[]', x'', 0,
				200, ?) RETURNING id`, ep, outcomeJSON(emulate.ActReply, 200)).Scan(&first)
			for _, rid := range []int64{first, id} {
				s.db.ExecContext(ctx, `INSERT INTO signature_seen (request_id, endpoint_id, check_no, digest) VALUES (?, ?, 1, 'd')`, rid, ep)
			}
			want[first] = true
		}
		want[id] = emulate.Anomalous(emulate.ParseOutcome(c.outcome, c.status), c.tooLarge, c.verdict, c.redelivered)
	}
	var rows int
	s.db.QueryRowContext(ctx, `SELECT count(*) FROM requests`).Scan(&rows)
	s.Close()

	s, err = Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var after int
	s.db.QueryRowContext(ctx, `SELECT count(*) FROM requests`).Scan(&after)
	if after != rows {
		t.Fatalf("%d rows after the migration, %d before", after, rows)
	}
	for id, w := range want {
		var got bool
		s.db.QueryRowContext(ctx, `SELECT anomaly FROM requests WHERE id = ?`, id).Scan(&got)
		if got != w {
			t.Errorf("migrated request %d: anomaly %v, Go says %v", id, got, w)
		}
	}
	k, err := s.KeptState(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	// The oldest kept row (id order) is #3, at 1000 ms: before it 0.2 had
	// lost requests.
	if !k.NormalAt.Equal(fromMillis(1000)) || !k.AnomalyAt.Equal(fromMillis(1000)) || k.Pruned != 0 {
		t.Errorf("migrated kept state %+v, want both watermarks at the oldest kept request", k)
	}
	// The first insert after the upgrade, with 0.2's limit, prunes nothing.
	r := sample(ep, "new")
	r.Outcome = outcomeJSON(emulate.ActReply, 200)
	if err := s.AddRequest(ctx, r, Keep{Normal: 1000, Anomalies: 1000}, nil); err != nil {
		t.Fatal(err)
	}
	s.db.QueryRowContext(ctx, `SELECT count(*) FROM requests`).Scan(&after)
	if after != rows+1 {
		t.Errorf("the first insert after the upgrade pruned: %d rows, want %d", after, rows+1)
	}
}

// TestLaneQueriesUseIndexes: no pruning, counting or watermark query scans
// the requests table.
func TestLaneQueriesUseIndexes(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	queries := map[string][]any{
		`SELECT id FROM ` + laneNormal.from() + ` ORDER BY id DESC LIMIT 1 OFFSET ?`:                                                                                              {int64(1), 10},
		`SELECT id FROM ` + laneAll.from() + ` ORDER BY id DESC LIMIT 1 OFFSET ?`:                                                                                                 {int64(1), 10},
		`SELECT id, received_at FROM ` + laneAnomaly.from() + ` AND id <= ? ORDER BY id LIMIT ?`:                                                                                  {int64(1), 5, 64},
		`SELECT id, received_at FROM ` + laneAll.from() + ` AND id <= ? ORDER BY id LIMIT ?`:                                                                                      {int64(1), 5, 64},
		`SELECT min(id) FROM ` + laneNormal.from():                                                                                                                                {int64(1)},
		`SELECT pinned, anomaly, count(*) FROM requests INDEXED BY requests_by_lane WHERE endpoint_id IS ? GROUP BY pinned, anomaly`:                                              {int64(1)},
		`SELECT id, received_at FROM requests INDEXED BY requests_by_time WHERE endpoint_id IS ? AND pinned = 0 AND anomaly = 0 AND received_at < ? ORDER BY received_at LIMIT ?`: {int64(1), 5, 256},
		`SELECT count(*) FROM requests r WHERE r.endpoint_id = ?`:                                                                                                                 {int64(1)},
		endpointSummary + ` WHERE e.id = ?`:                                                                                                                                       {int64(1)},
	}
	for q, args := range queries {
		rows, err := s.db.QueryContext(ctx, `EXPLAIN QUERY PLAN `+q, args...)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			rows.Scan(&id, &parent, &notused, &detail)
			plan = append(plan, detail)
		}
		rows.Close()
		for _, d := range plan {
			if strings.HasPrefix(d, "SCAN requests") && !strings.Contains(d, "INDEX") {
				t.Errorf("%s\n  scans the table: %v", q, plan)
			}
			if strings.Contains(d, "TEMP B-TREE") {
				t.Errorf("%s\n  sorts: %v", q, plan)
			}
		}
	}
}

var _ = sql.ErrNoRows

// bigRegister fills a register with n requests straight into the table (a
// tenth of them anomalies), the way a long capture leaves it.
func bigRegister(b testing.TB, s *Store, n int) int64 {
	b.Helper()
	ctx := context.Background()
	ep, err := s.CreateEndpoint(ctx, "/big")
	if err != nil {
		b.Fatal(err)
	}
	tx, _ := s.db.BeginTx(ctx, nil)
	st, err := tx.PrepareContext(ctx, `INSERT INTO requests (endpoint_id, no, received_at, method, path, raw_query, proto, remote_addr,
		headers, body, size, status, outcome, anomaly) VALUES (?, ?, ?, 'POST', '/big', '', 'HTTP/1.1', '', '[]', x'7b7d', 2, ?, ?, ?)`)
	if err != nil {
		b.Fatal(err)
	}
	base := time.Now().Add(-time.Duration(n) * time.Second)
	for i := range n {
		status, anomaly := 200, 0
		if i%10 == 0 {
			status, anomaly = 500, 1
		}
		if _, err := st.ExecContext(ctx, ep.ID, i+1, millis(base.Add(time.Duration(i)*time.Second)), status,
			outcomeJSON(emulate.ActReply, status), anomaly); err != nil {
			b.Fatal(err)
		}
	}
	tx.ExecContext(ctx, `UPDATE endpoints SET last_no = ? WHERE id = ?`, n, ep.ID)
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return ep.ID
}

// BenchmarkInsertWithPrune: one request caught into a register of 500 000
// kept requests at its limit, so every insert prunes the oldest.
//
//	go test ./internal/store -run '^$' -bench InsertWithPrune -benchtime 200x
func BenchmarkInsertWithPrune(b *testing.B) {
	s := openTemp(b)
	id := bigRegister(b, s, 500_000)
	keep := Keep{Normal: 450_000, Anomalies: 50_000}
	ctx := context.Background()
	b.ResetTimer()
	for i := range b.N {
		r := sample(id, fmt.Sprint(i))
		r.Outcome = outcomeJSON(emulate.ActReply, 200)
		if err := s.AddRequest(ctx, r, keep, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPrunePass: the minute's housekeeping over a register of 500 000
// requests with an age limit that leaves nothing to delete, and one that
// deletes the oldest hour (3600 rows) step by step.
func BenchmarkPrunePass(b *testing.B) {
	s := openTemp(b)
	id := bigRegister(b, s, 500_000)
	keep := Keep{Normal: 1_000_000, Anomalies: 1_000_000}
	ctx := context.Background()
	if err := s.SetRetention(ctx, id, Retention{Normal: Lane{MaxAge: 3650 * 24 * time.Hour}}); err != nil {
		b.Fatal(err)
	}
	b.Run("nothing-to-do", func(b *testing.B) {
		for range b.N {
			if _, err := s.Prune(ctx, time.Now(), keep); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("an-hour-past-its-age", func(b *testing.B) {
		age := 500_000*time.Second - time.Hour
		for i := range b.N {
			// Each round moves the age limit an hour further.
			p := Retention{Normal: Lane{MaxAge: age - time.Duration(i)*time.Hour}, Anomalies: Lane{MaxAge: age - time.Duration(i)*time.Hour}}
			p.Normal.MaxAge = p.Normal.MaxAge.Truncate(time.Hour)
			p.Anomalies.MaxAge = p.Normal.MaxAge
			if err := s.SetRetention(ctx, id, p); err != nil {
				b.Fatal(err)
			}
			if _, err := s.Prune(ctx, time.Now(), keep); err != nil {
				b.Fatal(err)
			}
		}
	})
}
