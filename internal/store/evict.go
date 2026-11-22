package store

import (
	"context"
	"database/sql"
	"fmt"
)

// The size limit (053-B): when the database's used pages go over a budget,
// the housekeeping drops what is oldest, globally, in this order until the
// used size is under 95 % of the budget: the bodies of normal requests,
// then normal requests (only those whose body is already dropped, so no
// normal row goes while a normal body remains), then the bodies of
// anomalies, then anomalies. Pinned requests are never touched. Each batch
// is a write of its own under the writer's lock: catching waits at most one
// batch.

// Eviction batches: at most this many requests, and bodies of at most
// EvictBytes (at least one request), per write.
const (
	EvictBatch = 256
	EvictBytes = 64 << 20
	// EvictTarget is the share of the budget an eviction stops under.
	EvictTarget = 95
	// LargeBody is the body size after whose insert the size is checked at
	// once, not only at the tick.
	LargeBody = 1 << 20
)

// Usage is the bytes in the database's used pages: every page but the free
// ones (the WAL is not counted).
func (s *Store) Usage(ctx context.Context) (int64, error) {
	var pages, free, size int64
	if err := s.db.QueryRowContext(ctx, `SELECT * FROM pragma_page_count, pragma_freelist_count, pragma_page_size`).Scan(&pages, &free, &size); err != nil {
		return 0, fmt.Errorf("database size: %w", err)
	}
	return (pages - free) * size, nil
}

// PinnedBytes is about how much the pinned requests hold: their bodies,
// header lines, outcomes and verdicts, and their replays' and forward's
// answer bodies. Eviction never frees it.
func (s *Store) PinnedBytes(ctx context.Context) (int64, error) {
	var n, more int64
	if err := s.db.QueryRowContext(ctx, `SELECT ifnull(sum(ifnull(length(body), 0) + length(headers) + length(outcome) + length(verdicts)), 0)
		FROM requests INDEXED BY requests_evict WHERE pinned = 1`).Scan(&n); err != nil {
		return 0, fmt.Errorf("pinned size: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT ifnull(sum(ifnull(length(body), 0)), 0) FROM replays WHERE request_id IN (SELECT id FROM requests INDEXED BY requests_evict WHERE pinned = 1)) +
		(SELECT ifnull(sum(ifnull(length(body), 0)), 0) FROM forwards WHERE request_id IN (SELECT id FROM requests INDEXED BY requests_evict WHERE pinned = 1))`).Scan(&more); err != nil {
		return 0, fmt.Errorf("pinned size: %w", err)
	}
	return n + more, nil
}

// DroppedReason says why a kept request has no body.
const DroppedReason = "the body was dropped to stay under the database size limit"

// AnswerDropped says why a replay's or forward's answer body is gone.
const AnswerDropped = "the answer body was dropped with the request's body, to stay under the database size limit"

// Path is the database file's path.
func (s *Store) Path() string { return s.path }

// Nudge asks the housekeeping for a size check now (after a large body);
// it never blocks.
func (s *Store) Nudge() {
	select {
	case s.nudge <- struct{}{}:
	default:
	}
}

// Evicted is what an eviction pass did.
type Evicted struct {
	Before, After int64         // used bytes
	Bodies        int           // request bodies dropped
	Rows          int           // requests deleted
	Registers     map[int64]int // requests deleted per register
	// Stuck: the budget could not be reached: every request left is
	// pinned.
	Stuck bool
}

// dropBodies drops the bodies of the requests ids that are not pinned, and
// their replays' and forward's answer bodies; the rest of each stays. An
// answer is marked dropped only when it had bytes.
func dropBodies(ctx context.Context, tx *sql.Tx, ids []any) error {
	in := "(?" + repeatString(",?", len(ids)-1) + ")"
	unpinned := `(SELECT id FROM requests WHERE pinned = 0 AND id IN ` + in + `)`
	for _, q := range []string{
		// The answers first: the requests' pins are read before they change.
		`UPDATE replays SET body = NULL, body_dropped = 1 WHERE length(body) > 0 AND request_id IN ` + unpinned,
		`UPDATE forwards SET body = NULL, body_dropped = 1 WHERE length(body) > 0 AND request_id IN ` + unpinned,
		`UPDATE requests SET body = NULL, body_dropped = 1 WHERE pinned = 0 AND id IN ` + in,
	} {
		if _, err := tx.ExecContext(ctx, q, ids...); err != nil {
			return fmt.Errorf("drop bodies: %w", err)
		}
	}
	return nil
}

// DropBodies drops the bodies of the given requests, as the size limit
// does (pinned ones are left alone).
func (s *Store) DropBodies(ctx context.Context, ids ...int64) error {
	if len(ids) == 0 {
		return nil
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	if err := dropBodies(ctx, tx, args); err != nil {
		return err
	}
	return tx.Commit()
}

// evictFor runs an eviction for the housekeeping and tells whom it asks.
func (s *Store) evictFor(ctx context.Context, h Housekeeping) {
	if h.MaxSize <= 0 {
		return
	}
	e, err := s.Evict(ctx, h.MaxSize)
	if err == nil {
		s.stuck.Store(e.Stuck)
	}
	if (e.Bodies > 0 || e.Rows > 0 || e.Stuck) && h.Evicted != nil {
		h.Evicted(e)
	}
	if err != nil && ctx.Err() == nil && h.Report != nil {
		h.Report(0, fmt.Errorf("size limit: %w", err))
	}
}

// evict steps, in order.
type evictStep struct {
	anomaly int  // the class: 0 normal, 1 anomaly
	rows    bool // delete requests (whose bodies are dropped) rather than drop bodies
}

var evictOrder = []evictStep{{0, false}, {0, true}, {1, false}, {1, true}}

// dropped is the body_dropped value of the requests the step works on.
func (st evictStep) dropped() int {
	if st.rows {
		return 1
	}
	return 0
}

// evictRaises is how many times a pass moves its newest request up to take
// in requests that arrived during it, before it leaves them to the next
// pass.
const evictRaises = 4

// Evict brings the used size under EvictTarget % of budget when it is over
// budget (see the order above); budget 0 does nothing.
func (s *Store) Evict(ctx context.Context, budget int64) (Evicted, error) {
	var e Evicted
	if budget <= 0 {
		return e, nil
	}
	used, err := s.Usage(ctx)
	if err != nil {
		return e, err
	}
	e.Before, e.After = used, used
	if used <= budget {
		return e, nil
	}
	target := budget / 100 * EvictTarget
	// A pass works on the requests up to last: however fast traffic comes,
	// it ends. A step runs only when the steps before it have nothing left,
	// also among requests that arrived during the pass (no older row goes
	// while a newer body of its class is kept): then last moves up, a few
	// times, before the rest is left to the next pass.
	last, err := s.newestID(ctx)
	if err != nil {
		return e, err
	}
	if s.evictStarted != nil {
		s.evictStarted()
	}
	raises := 0
	for used > target && ctx.Err() == nil {
		acted, ahead := false, false
		for _, st := range evictOrder {
			n, err := s.evictBatch(ctx, st, last, used-target, &e)
			if err != nil {
				return e, err
			}
			if n > 0 {
				acted = true
				break // from the first step again
			}
			if ahead, err = s.evictAhead(ctx, st, last); err != nil {
				return e, err
			}
			if ahead {
				break
			}
		}
		if ahead {
			if raises == evictRaises {
				s.Nudge() // the next pass takes them in
				break
			}
			raises++
			if last, err = s.newestID(ctx); err != nil {
				return e, err
			}
			continue
		}
		if used, err = s.Usage(ctx); err != nil {
			return e, err
		}
		e.After = used
		if !acted {
			e.Stuck = true // nothing unpinned is left
			break
		}
	}
	return e, ctx.Err()
}

// newestID is the newest request's id.
func (s *Store) newestID(ctx context.Context) (int64, error) {
	var last int64
	err := s.db.QueryRowContext(ctx, `SELECT ifnull(max(id), 0) FROM requests`).Scan(&last)
	return last, err
}

// evictAhead reports whether a step has work among requests newer than
// last.
func (s *Store) evictAhead(ctx context.Context, st evictStep, last int64) (bool, error) {
	var yes bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM requests INDEXED BY requests_evict
		WHERE pinned = 0 AND anomaly = ? AND body_dropped = ? AND id > ?)`, st.anomaly, st.dropped(), last).Scan(&yes)
	return yes, err
}

// evictBatch does one batch of a step under the writer's lock, over
// requests up to id last, dropping bodies of about need bytes at most (at
// least one), and returns how many requests it touched.
func (s *Store) evictBatch(ctx context.Context, st evictStep, last, need int64, e *Evicted) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, ifnull(endpoint_id, 0), received_at, ifnull(length(body), 0) FROM requests INDEXED BY requests_evict
		WHERE pinned = 0 AND anomaly = ? AND body_dropped = ? AND id <= ? ORDER BY id LIMIT ?`, st.anomaly, st.dropped(), last, EvictBatch)
	if err != nil {
		return 0, fmt.Errorf("evict: %w", err)
	}
	type row struct{ id, reg, at, size int64 }
	var batch []row
	var bytes int64
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.reg, &r.at, &r.size); err != nil {
			rows.Close()
			return 0, err
		}
		if len(batch) > 0 && !st.rows && (bytes+r.size > EvictBytes || bytes >= need) {
			break
		}
		batch = append(batch, r)
		bytes += r.size
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(batch) == 0 {
		return 0, nil
	}
	ids := make([]any, len(batch))
	for i, r := range batch {
		ids[i] = r.id
	}
	in := "(?" + repeatString(",?", len(ids)-1) + ")"
	if !st.rows {
		if err := dropBodies(ctx, tx, ids); err != nil {
			return 0, err
		}
		e.Bodies += len(batch)
		return len(batch), tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM requests WHERE id IN `+in, ids...); err != nil {
		return 0, fmt.Errorf("evict requests: %w", err)
	}
	type acc struct {
		n      int
		newest int64
	}
	byReg := map[int64]*acc{}
	for _, r := range batch {
		a := byReg[r.reg]
		if a == nil {
			a = &acc{}
			byReg[r.reg] = a
		}
		a.n++
		a.newest = max(a.newest, r.at)
	}
	for reg, a := range byReg {
		// The Unmatched register has no lanes: its mark covers every
		// request, as retention's does.
		l := laneNormal
		switch {
		case reg == Unmatched:
			l = laneAll
		case st.anomaly == 1:
			l = laneAnomaly
		}
		if err := recordPruned(ctx, tx, reg, l, a.n, a.newest); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if e.Registers == nil {
		e.Registers = map[int64]int{}
	}
	for reg, a := range byReg {
		e.Registers[reg] += a.n
	}
	e.Rows += len(batch)
	return len(batch), nil
}
