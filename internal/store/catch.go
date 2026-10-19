package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Unmatched is the register id of requests to paths no endpoint has.
const Unmatched int64 = 0

// Errors returned by the catch storage.
var (
	ErrNotFound      = errors.New("not found")
	ErrDuplicatePath = errors.New("an endpoint with this path already exists")
	// ErrOff: the endpoint was turned off after its request was looked up.
	ErrOff = errors.New("the endpoint is off")
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
	Enabled   bool      // on: it catches; off: requests to its path go to Unmatched
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
	Kept       int64  // body bytes stored: less than Size when only the head was kept (Unmatched)
	TooLarge   bool
	Status     int    // what hookwitness answered with; 0 when nothing was sent
	Outcome    string // what it did, as JSON (internal/emulate.Outcome)
	Verdict    string // overall authorization verdict: pass, fail, n/v, or "" without checks
	Verdicts   string // per check, as JSON (internal/auth.Verdict)
	Replays    int    // replays kept of it

	// Written only: the signatures it carried (keyed digests), and with
	// them Repeated, which gets each check's earliest earlier kept request
	// of the endpoint that carried the same one (check → its number) and
	// returns the verdicts to store instead.
	Signatures []Signature
	Repeated   func(earlier map[int]int64) (verdicts string)
}

// Signature is a signature a request carried, as the keyed digest of the
// value a check read.
type Signature struct {
	Check  int
	Digest string
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
	return Endpoint{ID: id, Path: path, CreatedAt: fromMillis(millis(now)), Enabled: true}, nil
}

const endpointSummary = `SELECT e.id, e.path, e.created_at,
	(SELECT COUNT(*) FROM requests r WHERE r.endpoint_id = e.id),
	(SELECT MAX(received_at) FROM requests r WHERE r.endpoint_id = e.id),
	e.config, e.counter, e.enabled
	FROM endpoints e`

func scanEndpoint(sc interface{ Scan(...any) error }) (Endpoint, error) {
	var e Endpoint
	var created int64
	var last sql.NullInt64
	if err := sc.Scan(&e.ID, &e.Path, &created, &e.Count, &last, &e.Config, &e.Counter, &e.Enabled); err != nil {
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
// returns its id, configuration and whether it is on.
func (s *Store) EndpointByPath(ctx context.Context, p string) (id int64, config string, enabled bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT id, config, enabled FROM endpoints WHERE path = ?`, p).Scan(&id, &config, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, ErrNotFound
	}
	return id, config, enabled, err
}

// SetConfig replaces an endpoint's configuration; the caller validates it.
func (s *Store) SetConfig(ctx context.Context, id int64, config string) error {
	return s.updateEndpoint(ctx, `UPDATE endpoints SET config = ? WHERE id = ?`, config, id)
}

// SetConfigIf replaces an endpoint's configuration only while it is still
// old, and reports whether it did: two clients that read the same version
// cannot both save over it.
func (s *Store) SetConfigIf(ctx context.Context, id int64, old, config string) (bool, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.ExecContext(ctx, `UPDATE endpoints SET config = ? WHERE id = ? AND config = ?`, config, id, old)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return true, nil
	}
	var one int
	err = s.db.QueryRowContext(ctx, `SELECT 1 FROM endpoints WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	return false, err
}

// SetPath moves an endpoint to another path; its requests stay with it.
func (s *Store) SetPath(ctx context.Context, id int64, path string) error {
	return s.UpdateEndpoint(ctx, id, EndpointChange{Path: &path})
}

// EndpointChange is what UpdateEndpoint changes; nil fields stay.
type EndpointChange struct {
	Path    *string // the caller validates it
	Enabled *bool
}

// UpdateEndpoint changes an endpoint's path and on/off state together: a
// taken path changes nothing. Turning one off keeps everything it has.
func (s *Store) UpdateEndpoint(ctx context.Context, id int64, c EndpointChange) error {
	err := s.updateEndpoint(ctx, `UPDATE endpoints SET path = COALESCE(?, path), enabled = COALESCE(?, enabled) WHERE id = ?`,
		c.Path, c.Enabled, id)
	if err != nil && isUnique(err) {
		return ErrDuplicatePath
	}
	return err
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
	return s.changeRequest(ctx, id, `UPDATE requests SET status = ?, outcome = ?, changed_at = ?, change_seq = ? WHERE id = ?`,
		status, outcome, millis(time.Now()))
}

// changeRequest runs the update of request id, whose query ends in
// "change_seq = ? WHERE id = ?" (args are the ones before), with the next
// value of the change sequence.
func (s *Store) changeRequest(ctx context.Context, id int64, query string, args ...any) error {
	return s.changeRequestTx(ctx, id, func(tx *sql.Tx, seq int64) error {
		_, err := tx.ExecContext(ctx, query, append(args, seq, id)...)
		return err
	})
}

// changeRequestTx runs fn, a change to one request, in one transaction with
// the next value of the change sequence: one writer at a time, so the
// sequence orders changes as they were committed.
func (s *Store) changeRequestTx(ctx context.Context, id int64, fn func(tx *sql.Tx, seq int64) error) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var seq int64
	if err := tx.QueryRowContext(ctx, `UPDATE settings SET value = CAST(value AS INTEGER) + 1
		WHERE key = 'feed.change_seq' RETURNING CAST(value AS INTEGER)`).Scan(&seq); err != nil {
		return fmt.Errorf("change sequence: %w", err)
	}
	if err := fn(tx, seq); err != nil {
		return err
	}
	return tx.Commit()
}

// ChangeSeq is the newest value of the change sequence: a page rendered
// now has seen every change up to it.
func (s *Store) ChangeSeq(ctx context.Context) (int64, error) {
	return changeSeq(ctx, s.db)
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func changeSeq(ctx context.Context, q queryer) (int64, error) {
	var v int64
	err := q.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM settings WHERE key = 'feed.change_seq'`).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("change sequence: %w", err)
	}
	return v, nil
}

// KeyCheck records the check value of the secret key the first time and
// reports whether a later key gives the same one; a database used with
// another key cannot open its secrets.
func (s *Store) KeyCheck(ctx context.Context, check string) (same bool, err error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO settings (key, value) VALUES ('secret.key_check', ?)`, check); err != nil {
		return false, err
	}
	var stored string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'secret.key_check'`).Scan(&stored); err != nil {
		return false, err
	}
	return stored == check, nil
}

// SetVerdicts replaces a stored request's verdicts (a re-check); the status
// and outcome, what the sender saw, stay as they are. Its signatures are
// replaced by sigs, and repeated (when given) notes the verdicts against the
// earlier kept requests, as AddRequest does.
func (s *Store) SetVerdicts(ctx context.Context, id int64, verdict, verdicts string, sigs []Signature, repeated func(map[int]int64) string) error {
	return s.changeRequestTx(ctx, id, func(tx *sql.Tx, seq int64) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM signature_seen WHERE request_id = ?`, id); err != nil {
			return err
		}
		var endpoint int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(endpoint_id, 0) FROM requests WHERE id = ?`, id).Scan(&endpoint); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil // gone meanwhile
			}
			return err
		}
		if v, err := noteRepeats(ctx, tx, endpoint, id, sigs, repeated); err != nil {
			return err
		} else if v != nil {
			verdicts = *v
		}
		_, err := tx.ExecContext(ctx, `UPDATE requests SET verdict = ?, verdicts = ?, changed_at = ?, change_seq = ? WHERE id = ?`,
			verdict, verdicts, millis(time.Now()), seq, id)
		return err
	})
}

// noteRepeats records request id's signatures and, when an earlier kept
// request of the endpoint carried one of them, returns the verdicts
// repeated gives for them (nil: none repeated).
func noteRepeats(ctx context.Context, tx *sql.Tx, endpoint, id int64, sigs []Signature, repeated func(map[int]int64) string) (*string, error) {
	if endpoint == Unmatched || len(sigs) == 0 {
		return nil, nil
	}
	earlier := map[int]int64{}
	for _, sg := range sigs {
		var no int64
		err := tx.QueryRowContext(ctx, `SELECT r.no FROM signature_seen s JOIN requests r ON r.id = s.request_id
			WHERE s.endpoint_id = ? AND s.digest = ? AND s.request_id < ? ORDER BY s.request_id LIMIT 1`,
			endpoint, sg.Digest, id).Scan(&no)
		if err == nil {
			earlier[sg.Check] = no
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("earlier signatures: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO signature_seen (request_id, endpoint_id, check_no, digest) VALUES (?, ?, ?, ?)`,
			id, endpoint, sg.Check, sg.Digest); err != nil {
			return nil, fmt.Errorf("record signatures: %w", err)
		}
	}
	if len(earlier) == 0 || repeated == nil {
		return nil, nil
	}
	v := repeated(earlier)
	return &v, nil
}

// VerdictCounts counts an endpoint's kept requests by overall verdict;
// requests without one are left out.
func (s *Store) VerdictCounts(ctx context.Context, endpointID int64) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT verdict, COUNT(*) FROM requests
		WHERE endpoint_id IS ? AND verdict <> '' GROUP BY verdict`, register(endpointID))
	if err != nil {
		return nil, fmt.Errorf("count verdicts: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var v string
		var n int
		if err := rows.Scan(&v, &n); err != nil {
			return nil, err
		}
		out[v] = n
	}
	return out, rows.Err()
}

// EachRequest calls fn for every request of a register with its body,
// oldest first, reading them in pages.
func (s *Store) EachRequest(ctx context.Context, endpointID int64, fn func(Request) error) error {
	return s.EachSelected(ctx, endpointID, Selection{}, true, fn)
}

// Selection picks requests of a register: received within [From, To] (a
// zero bound is open) and, when Nos is not empty, only the requests with
// those numbers in the register.
type Selection struct {
	From, To time.Time
	Nos      []int64
	Through  int64 // no request with a larger id; 0: no bound
}

// PageBytes bounds the bodies EachSelected holds at once: a page ends once
// its bodies reach it (at least one request a page). A variable for tests.
var PageBytes = 64 << 20

// pageHook, when set (by tests), is told each page's size.
var pageHook func(rows, bodyBytes int)

// EachSelected calls fn for the selected requests of a register, oldest
// first, reading them in pages so that a long register is never held whole
// (at most 200 requests and, with bodies, about PageBytes of them a page);
// withBody reads the bodies too.
func (s *Store) EachSelected(ctx context.Context, endpointID int64, sel Selection, withBody bool, fn func(Request) error) error {
	const page = 200
	cols := requestColumns
	if withBody {
		cols += ", body"
	}
	where := "endpoint_id IS ? AND id > ?"
	args := []any{register(endpointID), int64(0)}
	if !sel.From.IsZero() {
		where += " AND received_at >= ?"
		args = append(args, millis(sel.From))
	}
	if !sel.To.IsZero() {
		where += " AND received_at <= ?"
		args = append(args, millis(sel.To))
	}
	if sel.Through > 0 {
		where += " AND id <= ?"
		args = append(args, sel.Through)
	}
	if len(sel.Nos) > 0 {
		where += " AND no IN (?" + strings.Repeat(",?", len(sel.Nos)-1) + ")"
		for _, n := range sel.Nos {
			args = append(args, n)
		}
	}
	query := `SELECT ` + cols + ` FROM requests WHERE ` + where + ` ORDER BY id LIMIT ` + strconv.Itoa(page)
	for {
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("read requests: %w", err)
		}
		var batch []Request
		held, cut := 0, false
		for rows.Next() {
			r, err := scanRequest(rows, withBody)
			if err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, r)
			if held += len(r.Body); held >= PageBytes {
				cut = true // the rest of the page in the next query
				break
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if pageHook != nil {
			pageHook(len(batch), held)
		}
		for _, r := range batch {
			if err := fn(r); err != nil {
				return err
			}
			args[1] = r.ID
		}
		if len(batch) < page && !cut {
			return nil
		}
	}
}

// RequestsChanged lists a register's requests up to upToID whose outcome
// or verdicts changed after the change sequence value afterSeq, oldest
// first, without bodies, and the value they are complete up to: the
// sequence is read first, so every change up to it is committed and listed;
// a later one is left for the next call (also when it changed a listed row
// again).
func (s *Store) RequestsChanged(ctx context.Context, endpointID, upToID, afterSeq int64) ([]Request, int64, error) {
	seq, err := s.ChangeSeq(ctx)
	if err != nil {
		return nil, afterSeq, err
	}
	if seq <= afterSeq { // nothing new; a value from another database is pulled back
		return nil, seq, nil
	}
	reqs, err := s.listRequests(ctx, `SELECT `+requestColumns+` FROM requests
		WHERE endpoint_id IS ? AND id <= ? AND change_seq > ? AND change_seq <= ? ORDER BY id`,
		register(endpointID), upToID, afterSeq, seq)
	if err != nil {
		return nil, afterSeq, err
	}
	return reqs, seq, nil
}

// DeleteEndpoint removes an endpoint and every request it caught, and
// gives the space back (Reclaim).
func (s *Store) DeleteEndpoint(ctx context.Context, id int64) error {
	if err := s.deleteEndpoint(ctx, id); err != nil {
		return err
	}
	_, err := s.Reclaim(ctx, 0)
	return err
}

func (s *Store) deleteEndpoint(ctx context.Context, id int64) error {
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

// KeptFrom is when the oldest kept request of a register arrived, when
// requests older than it are no longer kept (pruned past the retention, or
// cleared): before it, the register cannot say what arrived. ok is false
// while the register still holds its first request, or nothing.
func (s *Store) KeptFrom(ctx context.Context, endpointID int64) (time.Time, bool, error) {
	var no, at int64
	err := s.db.QueryRowContext(ctx, `SELECT no, received_at FROM requests WHERE endpoint_id IS ? ORDER BY id LIMIT 1`,
		register(endpointID)).Scan(&no, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("oldest request: %w", err)
	}
	return fromMillis(at), no > 1, nil
}

// ClearRequests removes every request of a register and gives the space
// back (Reclaim). Numbering continues.
func (s *Store) ClearRequests(ctx context.Context, endpointID int64) error {
	if err := s.clearRequests(ctx, endpointID); err != nil {
		return err
	}
	_, err := s.Reclaim(ctx, 0)
	return err
}

func (s *Store) clearRequests(ctx context.Context, endpointID int64) error {
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
// the same transaction. An endpoint deleted meanwhile yields ErrNotFound,
// one turned off meanwhile ErrOff.
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
			`UPDATE endpoints SET last_no = last_no + 1 WHERE id = ? AND enabled RETURNING last_no`, r.EndpointID).Scan(&no)
		if errors.Is(err, sql.ErrNoRows) {
			var one int
			if tx.QueryRowContext(ctx, `SELECT 1 FROM endpoints WHERE id = ?`, r.EndpointID).Scan(&one) == nil {
				return ErrOff
			}
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
		(endpoint_id, no, received_at, method, path, raw_query, proto, remote_addr, headers, body, body_sha256, size, too_large, status, outcome, verdict, verdicts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		register(r.EndpointID), no, millis(r.ReceivedAt), r.Method, r.Path, r.RawQuery, r.Proto, r.RemoteAddr,
		string(headers), body, sum, r.Size, r.TooLarge, r.Status, r.Outcome, r.Verdict, r.Verdicts)
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
	// After the prune: a request no longer kept is not named.
	if v, err := noteRepeats(ctx, tx, r.EndpointID, id, r.Signatures, r.Repeated); err != nil {
		return err
	} else if v != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE requests SET verdicts = ? WHERE id = ?`, *v, id); err != nil {
			return fmt.Errorf("note repeated signatures: %w", err)
		}
		r.Verdicts = *v
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
	headers, body_sha256, size, too_large, status, outcome, verdict, verdicts, ifnull(length(body), 0),
	(SELECT COUNT(*) FROM replays WHERE replays.request_id = requests.id)`

func scanRequest(sc interface{ Scan(...any) error }, withBody bool) (Request, error) {
	var r Request
	var ep sql.NullInt64
	var at int64
	var headers string
	dst := []any{&r.ID, &ep, &r.No, &at, &r.Method, &r.Path, &r.RawQuery, &r.Proto, &r.RemoteAddr,
		&headers, &r.BodySHA256, &r.Size, &r.TooLarge, &r.Status, &r.Outcome, &r.Verdict, &r.Verdicts, &r.Kept, &r.Replays}
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

// ListQuery is a page of a register's requests: newest first older than
// Before (0: from the newest), or with HasAfter the ones above After, oldest
// first; at most Limit; only those with the Verdict and Status when set,
// whose body contains Contains, and that Match accepts.
type ListQuery struct {
	Before, After int64
	HasAfter      bool
	Limit         int
	Verdict       string // pass, fail, n/v, n/c; "" for any
	Status        int    // 0 for any; -1 for "nothing sent" (status 0)
	Contains      []byte // a byte substring of the kept body; nil for any
	// Match, when set, is asked about each kept body that passed the other
	// filters; requests without a kept body never match.
	Match    func(body []byte) bool
	WithBody bool // fill Body (always read when Match is set)
}

// List lists a register's requests per q; bodies only with q.WithBody.
func (s *Store) List(ctx context.Context, endpointID int64, q ListQuery) ([]Request, error) {
	where := "endpoint_id IS ?"
	args := []any{register(endpointID)}
	order := "id DESC"
	if q.HasAfter {
		where += " AND id > ?"
		args = append(args, q.After)
		order = "id"
	} else if q.Before > 0 {
		where += " AND id < ?"
		args = append(args, q.Before)
	}
	if q.Verdict != "" {
		where += " AND verdict = ?"
		args = append(args, q.Verdict)
	}
	switch {
	case q.Status > 0:
		where += " AND status = ?"
		args = append(args, q.Status)
	case q.Status < 0: // nothing was sent
		where += " AND status = 0"
	}
	if q.Contains != nil {
		where += " AND instr(body, ?) > 0" // both blobs: a byte search
		args = append(args, q.Contains)
	}
	if q.Match != nil {
		where += " AND NOT too_large"
	}
	withBody := q.WithBody || q.Match != nil
	cols := requestColumns
	if withBody {
		cols += ", body"
	}
	query := `SELECT ` + cols + ` FROM requests WHERE ` + where + ` ORDER BY ` + order
	if q.Limit > 0 && q.Match == nil { // with Match the rows are counted as they pass
		query += ` LIMIT ` + strconv.Itoa(q.Limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list requests: %w", err)
	}
	defer rows.Close()
	var out []Request
	for rows.Next() && (q.Limit <= 0 || len(out) < q.Limit) {
		r, err := scanRequest(rows, withBody)
		if err != nil {
			return nil, err
		}
		if q.Match != nil && !q.Match(r.Body) {
			continue
		}
		if !q.WithBody {
			r.Body = nil
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Request returns one request with its body.
func (s *Store) Request(ctx context.Context, id int64) (Request, error) {
	r, err := scanRequest(s.db.QueryRowContext(ctx, `SELECT `+requestColumns+`, body FROM requests WHERE id = ?`, id), true)
	if errors.Is(err, sql.ErrNoRows) {
		return Request{}, ErrNotFound
	}
	return r, err
}

// DeliveryNumbers is Deliveries for every request of reqs (a page of a
// register, the requests a live feed sends) in one query, by id.
func (s *Store) DeliveryNumbers(ctx context.Context, reqs []Request) (map[int64]int, error) {
	out := make(map[int64]int, len(reqs))
	hashes := map[int64][]any{} // per register, each hash once
	seen := map[string]bool{}
	var ids []any
	for _, r := range reqs {
		out[r.ID] = 1 // without a body: a first delivery
		if len(r.BodySHA256) == 0 {
			continue
		}
		ids = append(ids, r.ID)
		if k := fmt.Sprint(r.EndpointID) + string(r.BodySHA256); !seen[k] {
			seen[k] = true
			hashes[r.EndpointID] = append(hashes[r.EndpointID], r.BodySHA256)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	marks := func(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }
	var where []string
	var args []any
	for reg, hs := range hashes {
		where = append(where, `(endpoint_id IS ? AND body_sha256 IN (`+marks(len(hs))+`))`)
		args = append(append(args, register(reg)), hs...)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, n FROM (
		SELECT id, ROW_NUMBER() OVER (PARTITION BY endpoint_id, body_sha256 ORDER BY id) AS n
		FROM requests WHERE `+strings.Join(where, " OR ")+`
	) WHERE id IN (`+marks(len(ids))+`)`, append(args, ids...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
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
