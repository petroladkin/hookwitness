package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Unmatched is the register id of requests to paths no endpoint has.
const Unmatched int64 = 0

// Errors returned by the catch storage.
var (
	ErrNotFound      = errors.New("not found")
	ErrDuplicatePath = errors.New("an endpoint with this path already exists")
)

// Endpoint is an exact path that catches requests, with a summary of what it
// holds. The Unmatched register is reported as an Endpoint with ID 0 and an
// empty Path.
type Endpoint struct {
	ID        int64
	Path      string
	CreatedAt time.Time
	Count     int       // requests kept
	LastAt    time.Time // newest kept request; zero when there is none
	Config    string    // how it answers (internal/emulate); empty for the default
	Counter   int64     // requests that passed its checks since the last reset
}

// Header is one header line as received: original case, received order,
// repeated names kept as separate entries.
type Header struct {
	Name, Value string
}

// Request is one caught request. List queries leave Body empty; Request
// fills it.
type Request struct {
	ID         int64
	EndpointID int64 // Unmatched for the Unmatched register
	No         int64 // number within its register
	ReceivedAt time.Time
	Method     string
	Path       string // as received, percent-encoding kept
	RawQuery   string
	Proto      string
	RemoteAddr string
	Headers    []Header
	Body       []byte
	BodySHA256 []byte // nil when there is no body
	Size       int64  // body bytes; for a refused body the declared length, -1 if unknown
	TooLarge   bool
	Status     int    // what hookwitness answered with; 0 when nothing was sent
	Outcome    string // what it did, as JSON (internal/emulate.Outcome)
}

// Decide is called by AddRequest inside its transaction with the request's
// number since the counter was reset; it returns the status and outcome to
// store. Deciding under the same lock as counting keeps "every Nth" exact
// when requests arrive together.
type Decide func(counter int64) (status int, outcome string)

// register turns a register id into the value compared with endpoint_id;
// queries use "endpoint_id IS ?" so that NULL matches the Unmatched register.
func register(id int64) any {
	if id == Unmatched {
		return nil
	}
	return id
}

func millis(t time.Time) int64 { return t.UnixMilli() }

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// CreateEndpoint adds an endpoint at path. The caller validates the path.
func (s *Store) CreateEndpoint(ctx context.Context, path string) (Endpoint, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `INSERT INTO endpoints (path, created_at) VALUES (?, ?)`, path, millis(now))
	if err != nil {
		if isUnique(err) {
			return Endpoint{}, ErrDuplicatePath
		}
		return Endpoint{}, fmt.Errorf("create endpoint: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Endpoint{}, err
	}
	return Endpoint{ID: id, Path: path, CreatedAt: fromMillis(millis(now))}, nil
}

const endpointSummary = `SELECT e.id, e.path, e.created_at,
	(SELECT COUNT(*) FROM requests r WHERE r.endpoint_id = e.id),
	(SELECT MAX(received_at) FROM requests r WHERE r.endpoint_id = e.id),
	e.config, e.counter
	FROM endpoints e`

func scanEndpoint(sc interface{ Scan(...any) error }) (Endpoint, error) {
	var e Endpoint
	var created int64
	var last sql.NullInt64
	if err := sc.Scan(&e.ID, &e.Path, &created, &e.Count, &last, &e.Config, &e.Counter); err != nil {
		return Endpoint{}, err
	}
	e.CreatedAt = fromMillis(created)
	if last.Valid {
		e.LastAt = fromMillis(last.Int64)
	}
	return e, nil
}

// Endpoints lists the endpoints by path, with their request counts.
func (s *Store) Endpoints(ctx context.Context) ([]Endpoint, error) {
	rows, err := s.db.QueryContext(ctx, endpointSummary+` ORDER BY e.path`)
	if err != nil {
		return nil, fmt.Errorf("list endpoints: %w", err)
	}
	defer rows.Close()
	var out []Endpoint
	for rows.Next() {
		e, err := scanEndpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Endpoint returns one endpoint by id; id Unmatched returns the Unmatched
// register's summary.
func (s *Store) Endpoint(ctx context.Context, id int64) (Endpoint, error) {
	if id == Unmatched {
		var e Endpoint
		var last sql.NullInt64
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*), MAX(received_at) FROM requests WHERE endpoint_id IS NULL`).Scan(&e.Count, &last)
		if last.Valid {
			e.LastAt = fromMillis(last.Int64)
		}
		return e, err
	}
	e, err := scanEndpoint(s.db.QueryRowContext(ctx, endpointSummary+` WHERE e.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Endpoint{}, ErrNotFound
	}
	return e, err
}

// EndpointByPath finds the endpoint whose path equals p byte for byte and
// returns its id and configuration.
func (s *Store) EndpointByPath(ctx context.Context, p string) (id int64, config string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT id, config FROM endpoints WHERE path = ?`, p).Scan(&id, &config)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrNotFound
	}
	return id, config, err
}

// SetConfig replaces an endpoint's configuration; the caller validates it.
func (s *Store) SetConfig(ctx context.Context, id int64, config string) error {
	return s.updateEndpoint(ctx, `UPDATE endpoints SET config = ? WHERE id = ?`, config, id)
}

// ResetCounter starts an endpoint's request count again from zero.
func (s *Store) ResetCounter(ctx context.Context, id int64) error {
	return s.updateEndpoint(ctx, `UPDATE endpoints SET counter = 0 WHERE id = ?`, id)
}

func (s *Store) updateEndpoint(ctx context.Context, query string, args ...any) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateOutcome replaces a stored request's outcome once the action has run
// (measured delay, how long a sender waited, a render error).
func (s *Store) UpdateOutcome(ctx context.Context, id int64, status int, outcome string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE requests SET status = ?, outcome = ? WHERE id = ?`, status, outcome, id)
	return err
}

// DeleteEndpoint removes an endpoint and every request it caught.
func (s *Store) DeleteEndpoint(ctx context.Context, id int64) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.ExecContext(ctx, `DELETE FROM endpoints WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete endpoint: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ClearRequests removes every request of a register. Numbering continues.
func (s *Store) ClearRequests(ctx context.Context, endpointID int64) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if endpointID != Unmatched {
		var one int
		err := s.db.QueryRowContext(ctx, `SELECT 1 FROM endpoints WHERE id = ?`, endpointID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM requests WHERE endpoint_id IS ?`, register(endpointID))
	return err
}

// AddRequest stores r in its register (r.EndpointID), numbers it, and prunes
// the register's oldest requests beyond keep. It sets r.ID, r.No and
// r.BodySHA256. With decide (an endpoint's request that passed its checks)
// the endpoint's counter advances and decide sets r.Status and r.Outcome in
// the same transaction. An endpoint deleted meanwhile yields ErrNotFound.
func (s *Store) AddRequest(ctx context.Context, r *Request, keep int, decide Decide) error {
	if keep < 1 {
		return fmt.Errorf("keep must be at least 1, got %d", keep)
	}
	headers, err := json.Marshal(pairs(r.Headers))
	if err != nil {
		return err
	}
	var body, sum any
	r.BodySHA256 = nil
	if !r.TooLarge {
		body = r.Body
		if r.Body == nil {
			body = []byte{} // stored as an empty blob: NULL means "not kept"
		}
		if len(r.Body) > 0 {
			h := sha256.Sum256(r.Body)
			r.BodySHA256 = h[:]
			sum = r.BodySHA256
		}
	}

	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var no int64
	if r.EndpointID == Unmatched {
		var v string
		if err := tx.QueryRowContext(ctx,
			`UPDATE settings SET value = CAST(value AS INTEGER) + 1 WHERE key = 'unmatched.last_no' RETURNING value`).Scan(&v); err != nil {
			return fmt.Errorf("number request: %w", err)
		}
		if no, err = strconv.ParseInt(v, 10, 64); err != nil {
			return fmt.Errorf("number request: %w", err)
		}
	} else {
		err := tx.QueryRowContext(ctx,
			`UPDATE endpoints SET last_no = last_no + 1 WHERE id = ? RETURNING last_no`, r.EndpointID).Scan(&no)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("number request: %w", err)
		}
		if decide != nil {
			var n int64
			if err := tx.QueryRowContext(ctx,
				`UPDATE endpoints SET counter = counter + 1 WHERE id = ? RETURNING counter`, r.EndpointID).Scan(&n); err != nil {
				return fmt.Errorf("count request: %w", err)
			}
			r.Status, r.Outcome = decide(n)
		}
	}

	res, err := tx.ExecContext(ctx, `INSERT INTO requests
		(endpoint_id, no, received_at, method, path, raw_query, proto, remote_addr, headers, body, body_sha256, size, too_large, status, outcome)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		register(r.EndpointID), no, millis(r.ReceivedAt), r.Method, r.Path, r.RawQuery, r.Proto, r.RemoteAddr,
		string(headers), body, sum, r.Size, r.TooLarge, r.Status, r.Outcome)
	if err != nil {
		return fmt.Errorf("store request: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM requests WHERE endpoint_id IS ? AND id <= (
		SELECT id FROM requests WHERE endpoint_id IS ? ORDER BY id DESC LIMIT 1 OFFSET ?)`,
		register(r.EndpointID), register(r.EndpointID), keep); err != nil {
		return fmt.Errorf("prune requests: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.ID, r.No = id, no
	r.ReceivedAt = fromMillis(millis(r.ReceivedAt))
	return nil
}

// pairs is the stored form of headers: [[name, value], ...].
func pairs(hs []Header) [][2]string {
	out := make([][2]string, len(hs))
	for i, h := range hs {
		out[i] = [2]string{h.Name, h.Value}
	}
	return out
}

const requestColumns = `id, endpoint_id, no, received_at, method, path, raw_query, proto, remote_addr,
	headers, body_sha256, size, too_large, status, outcome`

func scanRequest(sc interface{ Scan(...any) error }, withBody bool) (Request, error) {
	var r Request
	var ep sql.NullInt64
	var at int64
	var headers string
	dst := []any{&r.ID, &ep, &r.No, &at, &r.Method, &r.Path, &r.RawQuery, &r.Proto, &r.RemoteAddr,
		&headers, &r.BodySHA256, &r.Size, &r.TooLarge, &r.Status, &r.Outcome}
	if withBody {
		dst = append(dst, &r.Body)
	}
	if err := sc.Scan(dst...); err != nil {
		return Request{}, err
	}
	r.EndpointID = ep.Int64
	r.ReceivedAt = fromMillis(at)
	if len(r.BodySHA256) == 0 {
		r.BodySHA256 = nil
	}
	// SQLite returns an empty blob as nil; nil is kept for "not stored".
	if withBody && r.Body == nil && !r.TooLarge {
		r.Body = []byte{}
	}
	var ps [][2]string
	if err := json.Unmarshal([]byte(headers), &ps); err != nil {
		return Request{}, fmt.Errorf("request %d: headers: %w", r.ID, err)
	}
	r.Headers = make([]Header, len(ps))
	for i, p := range ps {
		r.Headers[i] = Header{p[0], p[1]}
	}
	return r, nil
}

func (s *Store) listRequests(ctx context.Context, query string, args ...any) ([]Request, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list requests: %w", err)
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		r, err := scanRequest(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Requests lists a register's requests newest first, without bodies: at most
// limit of them, older than beforeID when it is above zero.
func (s *Store) Requests(ctx context.Context, endpointID, beforeID int64, limit int) ([]Request, error) {
	if beforeID <= 0 {
		beforeID = 1<<63 - 1
	}
	return s.listRequests(ctx, `SELECT `+requestColumns+` FROM requests
		WHERE endpoint_id IS ? AND id < ? ORDER BY id DESC LIMIT ?`, register(endpointID), beforeID, limit)
}

// RequestsAfter lists a register's requests with an id above afterID, oldest
// first, without bodies: what a live feed missed.
func (s *Store) RequestsAfter(ctx context.Context, endpointID, afterID int64) ([]Request, error) {
	return s.listRequests(ctx, `SELECT `+requestColumns+` FROM requests
		WHERE endpoint_id IS ? AND id > ? ORDER BY id`, register(endpointID), afterID)
}

// Request returns one request with its body.
func (s *Store) Request(ctx context.Context, id int64) (Request, error) {
	r, err := scanRequest(s.db.QueryRowContext(ctx, `SELECT `+requestColumns+`, body FROM requests WHERE id = ?`, id), true)
	if errors.Is(err, sql.ErrNoRows) {
		return Request{}, ErrNotFound
	}
	return r, err
}

// Deliveries counts the requests in r's register, up to and including r, that
// carry the same body: 1 for a first delivery, 2 for the first retry and so
// on. Requests without a body count as first deliveries.
func (s *Store) Deliveries(ctx context.Context, r Request) (int, error) {
	if len(r.BodySHA256) == 0 {
		return 1, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests
		WHERE endpoint_id IS ? AND body_sha256 = ? AND id <= ?`, register(r.EndpointID), r.BodySHA256, r.ID).Scan(&n)
	return n, err
}
