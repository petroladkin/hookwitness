package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Forward is a request's exchange with the receiver its endpoint forwards
// to: what was sent and what the receiver answered. Status 0 means there
// was no answer; Error then says why.
type Forward struct {
	RequestID int64
	Register  int64 // the request's register; set by FinishForward
	CreatedAt time.Time
	URL       string   // the target at the time
	Options   string   // JSON: host, insecure_tls, timeout_ms, by_rule
	Line      string   // the request line sent; "" when nothing was
	Sent      []Header // the header lines written, in order
	Notes     []string // where the request sent differs from the one received

	Proto   string
	Status  int
	Reason  string
	Headers []Header // the receiver's header lines: order, case, repeats
	Body    []byte   // the receiver's first bytes
	// BodyDropped: the answer's bytes were dropped with the request's body
	// (the size limit); Body is empty.
	BodyDropped bool
	Size        int64 // answer body bytes; -1 when cut before its end
	Truncated   bool
	Duration    time.Duration
	TTFB        time.Duration
	ErrorKind   string
	Error       string
}

// FinishForward stores a request's exchange and its final outcome in one
// transaction: the request takes the next change sequence (open feeds send
// its row again) and its lane follows the outcome. A request that is gone
// (pruned or cleared while it was forwarded) yields ErrNotFound and
// nothing is stored.
func (s *Store) FinishForward(ctx context.Context, fw *Forward, status int, outcome string) error {
	sent, err := json.Marshal(pairs(fw.Sent))
	if err != nil {
		return err
	}
	headers, err := json.Marshal(pairs(fw.Headers))
	if err != nil {
		return err
	}
	notes := fw.Notes
	if notes == nil {
		notes = []string{}
	}
	nb, err := json.Marshal(notes)
	if err != nil {
		return err
	}
	var register int64
	err = s.changeRequestTx(ctx, fw.RequestID, func(tx *sql.Tx, seq int64) error {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(endpoint_id, 0) FROM requests WHERE id = ?`, fw.RequestID).Scan(&register); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO forwards
			(request_id, created_at, url, options, line, sent, notes, proto, status, reason, headers, body, size, truncated,
			 duration_ms, ttfb_ms, error_kind, error)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			fw.RequestID, millis(fw.CreatedAt), fw.URL, fw.Options, fw.Line, string(sent), string(nb), fw.Proto, fw.Status,
			fw.Reason, string(headers), fw.Body, fw.Size, fw.Truncated, fw.Duration.Milliseconds(), fw.TTFB.Milliseconds(),
			fw.ErrorKind, fw.Error); err != nil {
			return fmt.Errorf("store forward: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE requests SET status = ?, outcome = ?, changed_at = ?, change_seq = ? WHERE id = ?`,
			status, outcome, millis(time.Now()), seq, fw.RequestID); err != nil {
			return err
		}
		return classify(ctx, tx, fw.RequestID)
	})
	if err != nil {
		return err
	}
	fw.Register = register
	fw.CreatedAt = fromMillis(millis(fw.CreatedAt))
	return nil
}

const forwardColumns = `request_id, created_at, url, options, line, sent, notes, proto, status, reason, headers,
	size, truncated, duration_ms, ttfb_ms, error_kind, error, body_dropped, body`

// ForwardOf returns a request's exchange with the receiver, with the
// answer's body; ErrNotFound when it has none.
func (s *Store) ForwardOf(ctx context.Context, requestID int64) (Forward, error) {
	var fw Forward
	var at, dur, ttfb int64
	var sent, notes, headers string
	err := s.db.QueryRowContext(ctx, `SELECT `+forwardColumns+` FROM forwards WHERE request_id = ?`, requestID).Scan(
		&fw.RequestID, &at, &fw.URL, &fw.Options, &fw.Line, &sent, &notes, &fw.Proto, &fw.Status, &fw.Reason, &headers,
		&fw.Size, &fw.Truncated, &dur, &ttfb, &fw.ErrorKind, &fw.Error, &fw.BodyDropped, &fw.Body)
	if errors.Is(err, sql.ErrNoRows) {
		return Forward{}, ErrNotFound
	}
	if err != nil {
		return Forward{}, err
	}
	fw.CreatedAt = fromMillis(at)
	fw.Duration, fw.TTFB = time.Duration(dur)*time.Millisecond, time.Duration(ttfb)*time.Millisecond
	if fw.Body == nil {
		fw.Body = []byte{}
	}
	if fw.Sent, err = unpairs(sent); err != nil {
		return Forward{}, fmt.Errorf("forward of %d: sent: %w", requestID, err)
	}
	if fw.Headers, err = unpairs(headers); err != nil {
		return Forward{}, fmt.Errorf("forward of %d: headers: %w", requestID, err)
	}
	if err := json.Unmarshal([]byte(notes), &fw.Notes); err != nil {
		return Forward{}, fmt.Errorf("forward of %d: notes: %w", requestID, err)
	}
	return fw, nil
}

// Forward filters of request lists, read from the outcome's receiver note.
const (
	ForwardOK     = "ok"     // forwarded, and the receiver answered 2xx
	ForwardFailed = "failed" // forwarded, and the receiver failed or answered other than 2xx
	ForwardNone   = "none"   // not forwarded
)

// forwardWhere is the SQL condition of a forward filter.
func forwardWhere(f string) string {
	const note = `CASE WHEN json_valid(outcome) THEN json_extract(outcome, '$.forward') END`
	switch f {
	case ForwardOK:
		return ` AND json_valid(outcome) AND json_extract(outcome, '$.forward.status') BETWEEN 200 AND 299
			AND json_extract(outcome, '$.forward.error_kind') IS NULL`
	case ForwardFailed:
		return ` AND json_valid(outcome) AND json_extract(outcome, '$.forward') IS NOT NULL
			AND ifnull(json_extract(outcome, '$.forward.pending'), 0) = 0
			AND (json_extract(outcome, '$.forward.error_kind') IS NOT NULL
				OR ifnull(json_extract(outcome, '$.forward.status'), 0) NOT BETWEEN 200 AND 299)`
	case ForwardNone:
		return ` AND ` + note + ` IS NULL`
	}
	return ""
}
