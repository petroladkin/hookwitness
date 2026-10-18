package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// MaxReplays is how many replays a request keeps; the oldest goes first.
const MaxReplays = 50

// Replay is a caught request sent again to a URL, with what was sent and
// what the target answered. Status 0 means there was no answer; Error then
// says why (ErrorKind: dial, tls, timeout, reset, write, answer).
type Replay struct {
	ID        int64
	RequestID int64
	CreatedAt time.Time
	By        string // who replayed; "" without login
	URL       string
	Options   string   // JSON: host, insecure_tls, timeout_ms
	Line      string   // the request line sent
	Sent      []Header // the header lines written, in order
	Notes     []string // where the request sent differs from the one received

	Proto     string
	Status    int
	Reason    string
	Headers   []Header // the answer's header lines: order, case, repeats
	Body      []byte   // the answer's first bytes; nil in lists
	Size      int64    // answer body bytes; -1 when cut before its end
	Truncated bool
	Duration  time.Duration
	TTFB      time.Duration
	ErrorKind string
	Error     string
}

// AddReplay stores rp and sets its ID; the request's oldest replays beyond
// MaxReplays are removed. A request that is gone yields ErrNotFound.
func (s *Store) AddReplay(ctx context.Context, rp *Replay) error {
	sent, err := json.Marshal(pairs(rp.Sent))
	if err != nil {
		return err
	}
	headers, err := json.Marshal(pairs(rp.Headers))
	if err != nil {
		return err
	}
	notes := rp.Notes
	if notes == nil {
		notes = []string{}
	}
	nb, err := json.Marshal(notes)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM requests WHERE id = ?`, rp.RequestID).Scan(&one); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO replays
		(request_id, created_at, by_user, url, options, line, sent, notes, proto, status, reason, headers, body, size, truncated,
		 duration_ms, ttfb_ms, error_kind, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rp.RequestID, millis(rp.CreatedAt), rp.By, rp.URL, rp.Options, rp.Line, string(sent), string(nb), rp.Proto, rp.Status,
		rp.Reason, string(headers), rp.Body, rp.Size, rp.Truncated, rp.Duration.Milliseconds(), rp.TTFB.Milliseconds(),
		rp.ErrorKind, rp.Error)
	if err != nil {
		return fmt.Errorf("store replay: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM replays WHERE request_id = ? AND id <= (
		SELECT id FROM replays WHERE request_id = ? ORDER BY id DESC LIMIT 1 OFFSET ?)`,
		rp.RequestID, rp.RequestID, MaxReplays); err != nil {
		return fmt.Errorf("prune replays: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	rp.ID = id
	rp.CreatedAt = fromMillis(millis(rp.CreatedAt))
	return nil
}

const replayColumns = `id, request_id, created_at, by_user, url, options, line, sent, notes, proto, status, reason, headers,
	size, truncated, duration_ms, ttfb_ms, error_kind, error`

func scanReplay(sc interface{ Scan(...any) error }, withBody bool) (Replay, error) {
	var rp Replay
	var at, dur, ttfb int64
	var sent, notes, headers string
	dst := []any{&rp.ID, &rp.RequestID, &at, &rp.By, &rp.URL, &rp.Options, &rp.Line, &sent, &notes, &rp.Proto, &rp.Status,
		&rp.Reason, &headers, &rp.Size, &rp.Truncated, &dur, &ttfb, &rp.ErrorKind, &rp.Error}
	if withBody {
		dst = append(dst, &rp.Body)
	}
	if err := sc.Scan(dst...); err != nil {
		return Replay{}, err
	}
	rp.CreatedAt = fromMillis(at)
	rp.Duration, rp.TTFB = time.Duration(dur)*time.Millisecond, time.Duration(ttfb)*time.Millisecond
	if withBody && rp.Body == nil {
		rp.Body = []byte{}
	}
	var err error
	if rp.Sent, err = unpairs(sent); err != nil {
		return Replay{}, fmt.Errorf("replay %d: sent: %w", rp.ID, err)
	}
	if rp.Headers, err = unpairs(headers); err != nil {
		return Replay{}, fmt.Errorf("replay %d: headers: %w", rp.ID, err)
	}
	if err := json.Unmarshal([]byte(notes), &rp.Notes); err != nil {
		return Replay{}, fmt.Errorf("replay %d: notes: %w", rp.ID, err)
	}
	return rp, nil
}

func unpairs(s string) ([]Header, error) {
	var ps [][2]string
	if err := json.Unmarshal([]byte(s), &ps); err != nil {
		return nil, err
	}
	out := make([]Header, len(ps))
	for i, p := range ps {
		out[i] = Header{p[0], p[1]}
	}
	return out, nil
}

// Replays lists a request's replays newest first, without answer bodies.
func (s *Store) Replays(ctx context.Context, requestID int64) ([]Replay, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+replayColumns+` FROM replays WHERE request_id = ? ORDER BY id DESC`, requestID)
	if err != nil {
		return nil, fmt.Errorf("list replays: %w", err)
	}
	defer rows.Close()
	out := []Replay{}
	for rows.Next() {
		rp, err := scanReplay(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, rp)
	}
	return out, rows.Err()
}

// Replay returns one replay with its answer body.
func (s *Store) Replay(ctx context.Context, id int64) (Replay, error) {
	rp, err := scanReplay(s.db.QueryRowContext(ctx, `SELECT `+replayColumns+`, body FROM replays WHERE id = ?`, id), true)
	if errors.Is(err, sql.ErrNoRows) {
		return Replay{}, ErrNotFound
	}
	return rp, err
}

// LastReplayURL is the target of the newest replay of a register's
// requests, or "": the dialog offers it again.
func (s *Store) LastReplayURL(ctx context.Context, endpointID int64) (string, error) {
	var u string
	err := s.db.QueryRowContext(ctx, `SELECT rp.url FROM replays rp JOIN requests r ON r.id = rp.request_id
		WHERE r.endpoint_id IS ? ORDER BY rp.id DESC LIMIT 1`, register(endpointID)).Scan(&u)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return u, err
}
