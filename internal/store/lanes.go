package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// anomalyExpr classifies a stored request of an endpoint, in SQL, exactly
// as emulate.Anomalous does from its parts (a test holds them together;
// migration 0010 classified the rows of older versions with it): not
// answered 2xx (no reply, a refusal, a render error, too large, busy), a
// failed verdict, or a signature another kept request of the endpoint
// carried (a redelivery: both deliveries are anomalies). On an endpoint
// that forwards, also a request the receiver missed: kept from it by a
// rule, or answered with the default reply because the receiver failed.
const anomalyExpr = `(too_large
	OR CASE WHEN json_valid(outcome) THEN json_extract(outcome, '$.action') NOT IN ('reply', 'forward')
		OR ifnull(json_extract(outcome, '$.status'), 0) NOT BETWEEN 200 AND 299
		OR ifnull(json_extract(outcome, '$.not_forwarded'), 0) = 1
		OR ifnull(json_extract(outcome, '$.forward.fallback'), 0) = 1
		ELSE status NOT BETWEEN 200 AND 299 END
	OR verdict = 'fail'
	OR EXISTS (SELECT 1 FROM signature_seen s JOIN signature_seen t
		ON t.endpoint_id = s.endpoint_id AND t.digest = s.digest AND t.request_id <> s.request_id
		WHERE s.request_id = requests.id))`

// PruneBatch is the most rows a housekeeping step deletes under the
// writer's lock; catching waits at most one batch.
const PruneBatch = 256

// insertPrune is the most rows one insert prunes (normally 0 or 1; more
// only after a limit was lowered, which the housekeeping catches up).
const insertPrune = 64

// pruneHook, when set (by tests), is told the rows each housekeeping step
// deleted, after the step let the writer go.
var pruneHook func(n int)

// classify sets request id's anomaly flag, and the flag of the requests it
// repeats a signature of (both deliveries of a redelivery are anomalies).
func classify(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE requests SET anomaly = `+anomalyExpr+`
		WHERE id = ? AND endpoint_id IS NOT NULL`, id); err != nil {
		return fmt.Errorf("classify request: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE requests SET anomaly = 1 WHERE anomaly = 0 AND id IN (
		SELECT t.request_id FROM signature_seen s JOIN signature_seen t
			ON t.endpoint_id = s.endpoint_id AND t.digest = s.digest AND t.request_id <> s.request_id
		WHERE s.request_id = ?)`, id); err != nil {
		return fmt.Errorf("classify redelivery: %w", err)
	}
	return nil
}

// policy reads a register's own retention policy.
func policy(ctx context.Context, q queryer, endpoint int64) (Retention, error) {
	var s string
	var err error
	if endpoint == Unmatched {
		err = q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'unmatched.retention'`).Scan(&s)
	} else {
		err = q.QueryRowContext(ctx, `SELECT retention FROM endpoints WHERE id = ?`, endpoint).Scan(&s)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return Retention{}, ErrNotFound
	}
	if err != nil {
		return Retention{}, fmt.Errorf("retention policy: %w", err)
	}
	return ParseRetention(s)
}

// effective is a register's policy resolved; the Unmatched register has no
// anomaly lane.
func effective(p Retention, endpoint int64, d Keep) Effective {
	e := p.Resolve(d)
	if endpoint == Unmatched {
		e.Anomalies, e.AnomalyAge = 0, 0
	}
	return e
}

// lane selects the rows of a register's lane: anomaly 0 or 1, or -1 for
// every unpinned row (a register without an anomaly lane).
type lane int

const (
	laneAll     lane = -1
	laneNormal  lane = 0
	laneAnomaly lane = 1
)

// from is the FROM and WHERE of a lane's rows (the register is the one
// argument), through the index that keeps them in id order.
func (l lane) from() string {
	if l == laneAll {
		return `requests INDEXED BY requests_by_register WHERE endpoint_id IS ? AND pinned = 0`
	}
	return `requests INDEXED BY requests_by_lane WHERE endpoint_id IS ? AND pinned = 0 AND anomaly = ` + strconv.Itoa(int(l))
}

// lanes are the lanes of a resolved policy, each with its count and age.
func (e Effective) lanes() []struct {
	l     lane
	count int
	age   time.Duration
} {
	type ln = struct {
		l     lane
		count int
		age   time.Duration
	}
	if e.Anomalies == 0 {
		return []ln{{laneAll, e.Normal, e.NormalAge}}
	}
	return []ln{{laneNormal, e.Normal, e.NormalAge}, {laneAnomaly, e.Anomalies, e.AnomalyAge}}
}

// pruneRows deletes the given rows of a register and records them.
func pruneRows(ctx context.Context, tx *sql.Tx, endpoint int64, l lane, rows *sql.Rows) (int, error) {
	var ids []any
	var newest int64
	for rows.Next() {
		var id, at int64
		if err := rows.Scan(&id, &at); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
		newest = max(newest, at)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	marks := "?" + repeatString(",?", len(ids)-1)
	if _, err := tx.ExecContext(ctx, `DELETE FROM requests WHERE id IN (`+marks+`)`, ids...); err != nil {
		return 0, fmt.Errorf("prune requests: %w", err)
	}
	if err := recordPruned(ctx, tx, endpoint, l, len(ids), newest); err != nil {
		return 0, err
	}
	return len(ids), nil
}

func repeatString(s string, n int) string {
	b := make([]byte, 0, len(s)*n)
	for range n {
		b = append(b, s...)
	}
	return string(b)
}

// pruneCount deletes, oldest first, at most limit rows of a lane beyond its
// newest count.
func pruneCount(ctx context.Context, tx *sql.Tx, endpoint int64, l lane, count, limit int) (int, error) {
	n, err := laneCount(ctx, tx, endpoint, l)
	if err != nil {
		return 0, err
	}
	if n <= count {
		return 0, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, received_at FROM `+l.from()+` ORDER BY id LIMIT ?`,
		register(endpoint), min(n-count, limit))
	if err != nil {
		return 0, fmt.Errorf("prune rows: %w", err)
	}
	return pruneRows(ctx, tx, endpoint, l, rows)
}

// laneCount is how many unpinned requests a lane of a register holds,
// from the counts the triggers of migration 0010 keep.
func laneCount(ctx context.Context, q queryer, endpoint int64, l lane) (int, error) {
	where := `reg = ? AND pinned = 0`
	if l != laneAll {
		where += ` AND anomaly = ` + strconv.Itoa(int(l))
	}
	var n int
	if err := q.QueryRowContext(ctx, `SELECT ifnull(sum(n), 0) FROM lane_counts WHERE `+where, endpoint).Scan(&n); err != nil {
		return 0, fmt.Errorf("lane count: %w", err)
	}
	return n, nil
}

// pruneAge deletes at most limit rows of a lane received before cutoff.
func pruneAge(ctx context.Context, tx *sql.Tx, endpoint int64, l lane, cutoff time.Time, limit int) (int, error) {
	cond := `endpoint_id IS ? AND pinned = 0`
	if l != laneAll {
		cond += ` AND anomaly = ` + strconv.Itoa(int(l))
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, received_at FROM requests INDEXED BY requests_by_time
		WHERE `+cond+` AND received_at < ? ORDER BY received_at LIMIT ?`,
		register(endpoint), millis(cutoff), limit)
	if err != nil {
		return 0, fmt.Errorf("prune by age: %w", err)
	}
	return pruneRows(ctx, tx, endpoint, l, rows)
}

// recordPruned counts n pruned rows of a lane and moves its watermark to
// the newest of them (both watermarks for a register without lanes).
func recordPruned(ctx context.Context, tx *sql.Tx, endpoint int64, l lane, n int, newest int64) error {
	cols := []string{"normal", "anomaly"}
	switch l {
	case laneNormal:
		cols = cols[:1]
	case laneAnomaly:
		cols = cols[1:]
	}
	if endpoint == Unmatched {
		if _, err := tx.ExecContext(ctx, `UPDATE settings SET value = CAST(CAST(value AS INTEGER) + ? AS TEXT)
			WHERE key = 'unmatched.pruned'`, n); err != nil {
			return err
		}
		for _, c := range cols {
			if _, err := tx.ExecContext(ctx, `UPDATE settings SET value = CAST(max(CAST(value AS INTEGER), ?) AS TEXT)
				WHERE key = ?`, newest, "unmatched.pruned_"+c+"_at"); err != nil {
				return err
			}
		}
		return nil
	}
	set := `pruned = pruned + ?`
	for _, c := range cols {
		set += `, pruned_` + c + `_at = max(pruned_` + c + `_at, ?)`
	}
	args := []any{n}
	for range cols {
		args = append(args, newest)
	}
	_, err := tx.ExecContext(ctx, `UPDATE endpoints SET `+set+` WHERE id = ?`, append(args, endpoint)...)
	return err
}

// check refuses defaults that would prune every request.
func (d Keep) check() error {
	if d.Normal < 1 || d.Anomalies < 0 {
		return fmt.Errorf("keep must be at least 1 normal and 0 anomalies, got %+v", d)
	}
	return nil
}

// pruneOnInsert keeps a register's lanes within their counts after an
// insert: at most insertPrune rows per lane.
func pruneOnInsert(ctx context.Context, tx *sql.Tx, endpoint int64, d Keep) error {
	p, err := policy(ctx, tx, endpoint)
	if err != nil {
		return err
	}
	for _, ln := range effective(p, endpoint, d).lanes() {
		if _, err := pruneCount(ctx, tx, endpoint, ln.l, ln.count, insertPrune); err != nil {
			return err
		}
	}
	return nil
}

// Prune applies every register's retention: ages, and counts left over
// after a limit was lowered, in steps of at most PruneBatch rows each under
// the writer's lock. It reports the registers that lost requests, with how
// many.
func (s *Store) Prune(ctx context.Context, now time.Time, d Keep) (map[int64]int, error) {
	if err := d.check(); err != nil {
		return nil, err
	}
	regs := []int64{Unmatched}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM endpoints ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list endpoints: %w", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		regs = append(regs, id)
	}
	rows.Close()
	out := map[int64]int{}
	for _, reg := range regs {
		n, err := s.pruneRegister(ctx, reg, now, d)
		if n > 0 {
			out[reg] = n
		}
		if errors.Is(err, ErrNotFound) {
			continue // deleted meanwhile
		}
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// pruneRegister prunes one register to its policy, step by step.
func (s *Store) pruneRegister(ctx context.Context, reg int64, now time.Time, d Keep) (int, error) {
	total := 0
	for {
		n, err := s.pruneStep(ctx, reg, now, d)
		total += n
		if err != nil || n == 0 {
			return total, err
		}
		if pruneHook != nil {
			pruneHook(n)
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}

// pruneStep deletes at most PruneBatch rows of a register in one write.
func (s *Store) pruneStep(ctx context.Context, reg int64, now time.Time, d Keep) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	p, err := policy(ctx, tx, reg)
	if err != nil {
		return 0, err
	}
	left := PruneBatch
	for _, ln := range effective(p, reg, d).lanes() {
		if ln.age > 0 && left > 0 {
			n, err := pruneAge(ctx, tx, reg, ln.l, now.Add(-ln.age), left)
			if err != nil {
				return 0, err
			}
			left -= n
		}
		if left > 0 {
			n, err := pruneCount(ctx, tx, reg, ln.l, ln.count, left)
			if err != nil {
				return 0, err
			}
			left -= n
		}
	}
	if left == PruneBatch {
		return 0, nil
	}
	return PruneBatch - left, tx.Commit()
}

// ToPrune counts the requests a register would lose now under policy p.
func (s *Store) ToPrune(ctx context.Context, reg int64, p Retention, now time.Time, d Keep) (int, error) {
	total := 0
	for _, ln := range effective(p, reg, d).lanes() {
		var old int
		n, err := laneCount(ctx, s.db, reg, ln.l)
		if err != nil {
			return 0, err
		}
		over := max(n-ln.count, 0)
		if ln.age > 0 {
			// Rows past the age among the ones the count keeps.
			if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT received_at FROM `+ln.l.from()+`
				ORDER BY id DESC LIMIT ?) WHERE received_at < ?`, register(reg), ln.count, millis(now.Add(-ln.age))).Scan(&old); err != nil {
				return 0, fmt.Errorf("count past the age: %w", err)
			}
		}
		total += over + old
	}
	return total, nil
}

// SetRetention stores a register's own policy (the zero policy: the
// server's defaults).
func (s *Store) SetRetention(ctx context.Context, reg int64, p Retention) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if reg == Unmatched {
		if p.AnomaliesSet || p.Anomalies.MaxAge > 0 {
			return errors.New("the Unmatched register has no anomaly lane")
		}
		_, err := s.db.ExecContext(ctx, `UPDATE settings SET value = ? WHERE key = 'unmatched.retention'`, p.String())
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE endpoints SET retention = ? WHERE id = ?`, p.String(), reg)
	if err != nil {
		return fmt.Errorf("set retention: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PruneNow applies one register's policy at once (after it was lowered).
func (s *Store) PruneNow(ctx context.Context, reg int64, now time.Time, d Keep) (int, error) {
	if err := d.check(); err != nil {
		return 0, err
	}
	return s.pruneRegister(ctx, reg, now, d)
}

// SetPinned pins or unpins a request: a pinned request is never pruned by
// retention.
func (s *Store) SetPinned(ctx context.Context, id int64, pinned bool) error {
	found := false
	err := s.changeRequestTx(ctx, id, func(tx *sql.Tx, seq int64) error {
		res, err := tx.ExecContext(ctx, `UPDATE requests SET pinned = ?, changed_at = ?, change_seq = ? WHERE id = ?`,
			pinned, millis(time.Now()), seq, id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		found = n > 0
		return nil
	})
	if err == nil && !found {
		return ErrNotFound
	}
	return err
}

// Kept is what a register holds and has lost.
type Kept struct {
	Policy    Retention
	Normal    int // unpinned normal requests (every unpinned one without an anomaly lane)
	Anomalies int // unpinned anomalies
	Pinned    int
	Pruned    int64     // requests retention removed, ever
	NormalAt  time.Time // newest normal request pruned; zero: none
	AnomalyAt time.Time // newest anomaly pruned; zero: none
}

// KeptState reads a register's counts per lane and what it lost.
func (s *Store) KeptState(ctx context.Context, reg int64) (Kept, error) {
	var k Kept
	var pol string
	var normalAt, anomalyAt int64
	if reg == Unmatched {
		get := func(key string) (string, error) {
			var v string
			err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
			return v, err
		}
		var err error
		if pol, err = get("unmatched.retention"); err != nil {
			return k, err
		}
		for key, dst := range map[string]*int64{"unmatched.pruned": &k.Pruned, "unmatched.pruned_normal_at": &normalAt,
			"unmatched.pruned_anomaly_at": &anomalyAt} {
			v, err := get(key)
			if err != nil {
				return k, err
			}
			*dst, _ = strconv.ParseInt(v, 10, 64)
		}
	} else {
		err := s.db.QueryRowContext(ctx, `SELECT retention, pruned, pruned_normal_at, pruned_anomaly_at FROM endpoints WHERE id = ?`,
			reg).Scan(&pol, &k.Pruned, &normalAt, &anomalyAt)
		if errors.Is(err, sql.ErrNoRows) {
			return k, ErrNotFound
		}
		if err != nil {
			return k, err
		}
	}
	var err error
	if k.Policy, err = ParseRetention(pol); err != nil {
		return k, err
	}
	if normalAt > 0 {
		k.NormalAt = fromMillis(normalAt)
	}
	if anomalyAt > 0 {
		k.AnomalyAt = fromMillis(anomalyAt)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT pinned, anomaly, n FROM lane_counts WHERE reg = ?`, reg)
	if err != nil {
		return k, fmt.Errorf("count lanes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var pinned, anomaly bool
		var n int
		if err := rows.Scan(&pinned, &anomaly, &n); err != nil {
			return k, err
		}
		switch {
		case pinned:
			k.Pinned += n
		case anomaly:
			k.Anomalies += n
		default:
			k.Normal += n
		}
	}
	return k, rows.Err()
}

// Watermarks are the lowest kept unpinned id of each lane of a register
// (0: the lane is empty): an open page drops a loaded row below its lane's
// mark.
func (s *Store) Watermarks(ctx context.Context, reg int64) (normal, anomaly int64, err error) {
	for _, w := range []struct {
		l   lane
		dst *int64
	}{{laneNormal, &normal}, {laneAnomaly, &anomaly}} {
		var v sql.NullInt64
		if err = s.db.QueryRowContext(ctx, `SELECT min(id) FROM `+w.l.from(), register(reg)).Scan(&v); err != nil {
			return 0, 0, fmt.Errorf("lane watermark: %w", err)
		}
		*w.dst = v.Int64
	}
	return normal, anomaly, nil
}
