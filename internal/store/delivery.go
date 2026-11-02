package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Delivery is what a request carries for the delivery checks (internal/
// emulate Facts) and Relate, which gets the request's relation to the
// earlier kept requests of its endpoint, in the transaction that stores it,
// and returns the overall verdict and the verdicts to store.
type Delivery struct {
	Event   string   // the event id; "" with ByBody or Missing
	ByBody  bool     // no event source: attempts are counted per body
	Missing bool     // an event source is set and the request has no id
	Order   *float64 // the order check's value; nil: none
	Group   string   // the order check's partition
	Relate  func(Relation) (verdict, verdicts string)
}

// Relation is a request's place among the earlier kept requests.
type Relation struct {
	Attempt int          // 1-based among kept attempts of its event or body; 0 when Missing
	Prev    *PrevAttempt // the previous kept attempt; nil for the first
	High    *OrderMark   // the highest order value of its group among other events; nil: none
}

// PrevAttempt is an earlier kept attempt.
type PrevAttempt struct {
	ID, No  int64
	At      time.Time
	Status  int
	Outcome string
}

// OrderMark is an order value and the request that carried it.
type OrderMark struct {
	Value float64
	No    int64
}

// relate finds request id's relation, records its event row and stores the
// verdicts Relate gives.
func relate(ctx context.Context, tx *sql.Tx, endpoint, id int64, bodySHA []byte, d *Delivery) error {
	if d == nil || endpoint == Unmatched {
		return nil
	}
	var rel Relation
	var prev *sql.Row
	switch {
	case d.Missing:
	case d.ByBody:
		if len(bodySHA) > 0 {
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE endpoint_id = ? AND body_sha256 = ? AND id < ?`,
				endpoint, bodySHA, id).Scan(&rel.Attempt); err != nil {
				return fmt.Errorf("attempts: %w", err)
			}
			prev = tx.QueryRowContext(ctx, `SELECT id, no, received_at, status, outcome FROM requests
				WHERE endpoint_id = ? AND body_sha256 = ? AND id < ? ORDER BY id DESC LIMIT 1`, endpoint, bodySHA, id)
		}
		rel.Attempt++
	default:
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM event_seen WHERE endpoint_id = ? AND event = ? AND request_id < ?`,
			endpoint, d.Event, id).Scan(&rel.Attempt); err != nil {
			return fmt.Errorf("attempts: %w", err)
		}
		rel.Attempt++
		prev = tx.QueryRowContext(ctx, `SELECT r.id, r.no, r.received_at, r.status, r.outcome FROM event_seen e JOIN requests r ON r.id = e.request_id
			WHERE e.endpoint_id = ? AND e.event = ? AND e.request_id < ? ORDER BY e.request_id DESC LIMIT 1`, endpoint, d.Event, id)
	}
	if prev != nil {
		var p PrevAttempt
		var at int64
		err := prev.Scan(&p.ID, &p.No, &at, &p.Status, &p.Outcome)
		switch {
		case err == nil:
			p.At = fromMillis(at)
			rel.Prev = &p
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("previous attempt: %w", err)
		}
	}
	if d.Order != nil {
		q := `SELECT e.order_value, r.no FROM event_seen e INDEXED BY event_seen_by_order JOIN requests r ON r.id = e.request_id
			WHERE e.endpoint_id = ? AND e.order_group = ? AND e.order_value IS NOT NULL AND e.request_id < ?`
		args := []any{endpoint, d.Group, id}
		// Retries of an earlier event are late by nature: other events only.
		switch {
		case d.ByBody && len(bodySHA) > 0:
			q += ` AND r.body_sha256 IS NOT ?`
			args = append(args, bodySHA)
		case !d.ByBody && !d.Missing:
			q += ` AND e.event IS NOT ?`
			args = append(args, d.Event)
		}
		var m OrderMark
		err := tx.QueryRowContext(ctx, q+` ORDER BY e.order_value DESC LIMIT 1`, args...).Scan(&m.Value, &m.No)
		switch {
		case err == nil:
			rel.High = &m
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("order mark: %w", err)
		}
	}
	var event any = d.Event
	if d.Missing {
		event = nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO event_seen (request_id, endpoint_id, event, attempt, order_value, order_group)
		VALUES (?, ?, ?, ?, ?, ?)`, id, endpoint, event, rel.Attempt, d.Order, d.Group); err != nil {
		return fmt.Errorf("record event: %w", err)
	}
	if d.Relate == nil {
		return nil
	}
	verdict, verdicts := d.Relate(rel)
	if _, err := tx.ExecContext(ctx, `UPDATE requests SET verdict = ?, verdicts = ? WHERE id = ?`, verdict, verdicts, id); err != nil {
		return fmt.Errorf("delivery verdicts: %w", err)
	}
	return nil
}

// RetryGroup is what a request's retries are grouped by: its event when one
// was recorded (byEvent), else its body's digest; nil when neither.
func (r Request) RetryGroup() (key []byte, byEvent bool) {
	if r.Event != nil && *r.Event != "" {
		h := sha256.Sum256([]byte("event\x00" + *r.Event))
		return h[:], true
	}
	return r.BodySHA256, false
}
