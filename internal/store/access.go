package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Errors of the access storage.
var (
	ErrExists    = errors.New("already exists")
	ErrLastAdmin = errors.New("the last admin cannot be removed or demoted")
)

// Roles of a user.
const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

// User is a person who may use the admin once login is on. Password is the
// stored hash record; it never leaves the server.
type User struct {
	ID                int64
	Name              string
	Role              string
	Password          string
	CreatedAt         time.Time
	PasswordChangedAt time.Time
	Tokens            int // personal tokens (Users only)
}

// Session is a browser login, with its user as stored now.
type Session struct {
	ID         int64
	UserID     int64
	Name, Role string
	CreatedAt  time.Time
	LastSeenAt time.Time
}

// Token is a personal token without its hash; Hash is filled only by
// TokenByPublicID, which verification needs.
type Token struct {
	ID         int64
	PublicID   string
	UserID     int64
	Owner      string // the owner's name
	Role       string // the owner's role as stored now
	Name       string
	Hash       string
	CreatedAt  time.Time
	LastUsedAt time.Time // zero when never used
}

// HasUsers reports whether any user exists: login is on.
func (s *Store) HasUsers(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users)`).Scan(&n)
	return n == 1, err
}

// AddUser creates a user; name is already lower-case and valid.
func (s *Store) AddUser(ctx context.Context, name, role, password string) (User, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `INSERT INTO users (name, role, password, created_at, password_changed_at) VALUES (?, ?, ?, ?, ?)`,
		name, role, password, millis(now), millis(now))
	if err != nil {
		if isUnique(err) {
			return User{}, ErrExists
		}
		return User{}, fmt.Errorf("add user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, err
	}
	t := fromMillis(millis(now))
	return User{ID: id, Name: name, Role: role, Password: password, CreatedAt: t, PasswordChangedAt: t}, nil
}

const userCols = `id, name, role, password, created_at, password_changed_at`

func scanUser(sc interface{ Scan(...any) error }) (User, error) {
	var u User
	var created, changed int64
	err := sc.Scan(&u.ID, &u.Name, &u.Role, &u.Password, &created, &changed)
	u.CreatedAt, u.PasswordChangedAt = fromMillis(created), fromMillis(changed)
	return u, err
}

// UserByName finds a user by its lower-case name.
func (s *Store) UserByName(ctx context.Context, name string) (User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// UserByID finds a user by id.
func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// Users lists every user by name, with the count of their tokens.
func (s *Store) Users(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+`, (SELECT COUNT(*) FROM tokens t WHERE t.user_id = users.id)
		FROM users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var created, changed int64
		if err := rows.Scan(&u.ID, &u.Name, &u.Role, &u.Password, &created, &changed, &u.Tokens); err != nil {
			return nil, err
		}
		u.CreatedAt, u.PasswordChangedAt = fromMillis(created), fromMillis(changed)
		out = append(out, u)
	}
	return out, rows.Err()
}

// otherAdmins counts the admins other than id inside tx.
func otherAdmins(ctx context.Context, tx *sql.Tx, id int64) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = 'admin' AND id <> ?`, id).Scan(&n)
	return n, err
}

// changeUser runs fn on user name inside a write transaction; guardAdmin
// refuses when the user is the last admin.
func (s *Store) changeUser(ctx context.Context, name string, guardAdmin bool, fn func(*sql.Tx, User) error) (User, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil) // IMMEDIATE (_txlock): other processes wait
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	u, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	if guardAdmin && u.Role == RoleAdmin {
		n, err := otherAdmins(ctx, tx, u.ID)
		if err != nil {
			return User{}, err
		}
		if n == 0 {
			return User{}, ErrLastAdmin
		}
	}
	if err := fn(tx, u); err != nil {
		return User{}, err
	}
	return u, tx.Commit()
}

// SetRole changes a user's role and ends their sessions (their rights
// changed under them). Demoting the last admin is ErrLastAdmin.
func (s *Store) SetRole(ctx context.Context, name, role string) (User, error) {
	u, err := s.changeUser(ctx, name, role != RoleAdmin, func(tx *sql.Tx, u User) error {
		if u.Role == role {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`, role, u.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, u.ID)
		return err
	})
	u.Role = role
	return u, err
}

// SetPassword replaces a user's password record and ends their sessions
// except keep (the session that changed it; 0 ends all).
func (s *Store) SetPassword(ctx context.Context, name, password string, keep int64) (User, error) {
	return s.changeUser(ctx, name, false, func(tx *sql.Tx, u User) error {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET password = ?, password_changed_at = ? WHERE id = ?`,
			password, millis(time.Now()), u.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND id <> ?`, u.ID, keep)
		return err
	})
}

// RemoveUser deletes a user with their sessions and tokens. Removing the
// last admin is ErrLastAdmin.
func (s *Store) RemoveUser(ctx context.Context, name string) (User, error) {
	return s.changeUser(ctx, name, true, func(tx *sql.Tx, u User) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, u.ID)
		return err
	})
}

// AddSession records a login: hash is the SHA-256 of the session id.
func (s *Store) AddSession(ctx context.Context, hash []byte, userID int64) (int64, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	now := millis(time.Now())
	res, err := s.db.ExecContext(ctx, `INSERT INTO sessions (hash, user_id, created_at, last_seen_at) VALUES (?, ?, ?, ?)`,
		hash, userID, now, now)
	if err != nil {
		return 0, fmt.Errorf("add session: %w", err)
	}
	return res.LastInsertId()
}

// SessionByHash finds a session with its user's current name and role.
func (s *Store) SessionByHash(ctx context.Context, hash []byte) (Session, error) {
	var se Session
	var created, seen int64
	err := s.db.QueryRowContext(ctx, `SELECT s.id, s.user_id, u.name, u.role, s.created_at, s.last_seen_at
		FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.hash = ?`, hash).
		Scan(&se.ID, &se.UserID, &se.Name, &se.Role, &created, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	se.CreatedAt, se.LastSeenAt = fromMillis(created), fromMillis(seen)
	return se, err
}

// TouchSession records activity on a session.
func (s *Store) TouchSession(ctx context.Context, id int64, at time.Time) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE id = ?`, millis(at), id)
	return err
}

// EndSession deletes one session.
func (s *Store) EndSession(ctx context.Context, id int64) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// SweepSessions deletes the sessions idle since before idle or created
// before created.
func (s *Store) SweepSessions(ctx context.Context, idle, created time.Time) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE last_seen_at < ? OR created_at < ?`, millis(idle), millis(created))
	return err
}

// AddToken records a personal token; a name the owner already uses is
// ErrExists.
func (s *Store) AddToken(ctx context.Context, publicID string, userID int64, name, hash string) (Token, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `INSERT INTO tokens (public_id, user_id, name, hash, created_at) VALUES (?, ?, ?, ?, ?)`,
		publicID, userID, name, hash, millis(now))
	if err != nil {
		if isUnique(err) {
			return Token{}, ErrExists
		}
		return Token{}, fmt.Errorf("add token: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Token{}, err
	}
	return Token{ID: id, PublicID: publicID, UserID: userID, Name: name, CreatedAt: fromMillis(millis(now))}, nil
}

const tokenCols = `t.id, t.public_id, t.user_id, u.name, u.role, t.name, t.created_at, COALESCE(t.last_used_at, 0)`

func scanToken(sc interface{ Scan(...any) error }, extra ...any) (Token, error) {
	var t Token
	var created, used int64
	err := sc.Scan(append([]any{&t.ID, &t.PublicID, &t.UserID, &t.Owner, &t.Role, &t.Name, &created, &used}, extra...)...)
	t.CreatedAt = fromMillis(created)
	if used > 0 {
		t.LastUsedAt = fromMillis(used)
	}
	return t, err
}

// TokenByPublicID finds a token with its hash and its owner as stored now.
func (s *Store) TokenByPublicID(ctx context.Context, publicID string) (Token, error) {
	var hash string
	t, err := scanToken(s.db.QueryRowContext(ctx, `SELECT `+tokenCols+`, t.hash
		FROM tokens t JOIN users u ON u.id = t.user_id WHERE t.public_id = ?`, publicID), &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, ErrNotFound
	}
	t.Hash = hash
	return t, err
}

// Tokens lists a user's tokens (userID 0: everyone's), without hashes,
// newest first.
func (s *Store) Tokens(ctx context.Context, userID int64) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+tokenCols+` FROM tokens t JOIN users u ON u.id = t.user_id
		WHERE ? = 0 OR t.user_id = ? ORDER BY u.name, t.created_at DESC, t.id DESC`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken deletes a token by its public id; owner 0 means any owner.
// A token of another owner is ErrNotFound.
func (s *Store) RevokeToken(ctx context.Context, publicID string, owner int64) (Token, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	t, err := scanToken(s.db.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM tokens t JOIN users u ON u.id = t.user_id
		WHERE t.public_id = ? AND (? = 0 OR t.user_id = ?)`, publicID, owner, owner))
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, ErrNotFound
	}
	if err != nil {
		return Token{}, err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM tokens WHERE id = ?`, t.ID)
	return t, err
}

// TouchToken records a token's use.
func (s *Store) TouchToken(ctx context.Context, id int64, at time.Time) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE id = ?`, millis(at), id)
	return err
}
