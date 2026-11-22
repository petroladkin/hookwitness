package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Errors of emails and provider sign-in.
var (
	ErrEmailTaken      = errors.New("another user has this email")
	ErrLoginOff        = errors.New("login is off: no users yet")
	ErrUnverified      = errors.New("the provider did not confirm the email")
	ErrNoAccount       = errors.New("no account for this email")
	ErrLinkedElsewhere = errors.New("the account is linked to another identity at this provider")
	ErrLastWayIn       = errors.New("the user would have no way to log in")
)

// Identity binds a user to an identity at an OpenID Connect provider, by
// issuer and subject. Provider is the configured name at the last sign-in
// (for display), Email the provider's at the last sign-in.
type Identity struct {
	ID          int64
	UserID      int64
	Issuer      string
	Subject     string
	Provider    string
	Email       string
	LinkedAt    time.Time
	LastLoginAt time.Time
}

const identityCols = `id, user_id, issuer, subject, provider, COALESCE(email, ''), linked_at, last_login_at`

func scanIdentity(sc interface{ Scan(...any) error }) (Identity, error) {
	var id Identity
	var linked, last int64
	err := sc.Scan(&id.ID, &id.UserID, &id.Issuer, &id.Subject, &id.Provider, &id.Email, &linked, &last)
	id.LinkedAt, id.LastLoginAt = fromMillis(linked), fromMillis(last)
	return id, err
}

// UserByEmail finds the user with an email (compared lower-case).
func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE email = ?`, strings.ToLower(email)))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// SetEmail sets a user's email (a valid address, stored lower-case) or
// clears it (""). An email another user has is ErrEmailTaken.
func (s *Store) SetEmail(ctx context.Context, name, email string) (User, error) {
	email = strings.ToLower(email)
	u, err := s.changeUser(ctx, name, false, func(tx *sql.Tx, u User) error {
		_, err := tx.ExecContext(ctx, `UPDATE users SET email = ? WHERE id = ?`, nullable(email), u.ID)
		if isUnique(err) {
			return ErrEmailTaken
		}
		return err
	})
	u.Email = email
	return u, err
}

// SignInInput is a verified identity from a provider.
type SignInInput struct {
	Issuer, Subject string
	Provider        string // the configured name, for display
	Email           string // as the provider sent it; "" when none
	Verified        bool   // the provider confirmed the email
	// AllowCreate: the email's domain is one this provider may create users
	// for; Name is then the new user's name, made unique with -2, -3...
	AllowCreate bool
	Name        string
}

// SignInResult is the user a sign-in resolved to.
type SignInResult struct {
	User    User
	Linked  bool // the identity was linked by this sign-in
	Created bool // and the user created for it
}

// SignIn resolves a provider identity to a user, in one write transaction
// (two first sign-ins of one identity make one user):
//
//  1. an identity with (issuer, subject) signs in its user, whatever the
//     email is now; the identity's email and last sign-in are updated;
//  2. else the email must be verified (ErrUnverified);
//  3. a user with that email is linked, unless already linked to another
//     identity at this issuer (ErrLinkedElsewhere);
//  4. else, with AllowCreate, a user with role user and no password is
//     created and linked;
//  5. else ErrNoAccount.
//
// With no users at all it is ErrLoginOff: a provider never makes the first
// account.
func (s *Store) SignIn(ctx context.Context, in SignInInput) (SignInResult, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SignInResult{}, err
	}
	defer tx.Rollback()
	var users int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users)`).Scan(&users); err != nil {
		return SignInResult{}, err
	}
	if users == 0 {
		return SignInResult{}, ErrLoginOff
	}
	now := millis(time.Now())
	email := strings.ToLower(in.Email)
	var userID int64
	err = tx.QueryRowContext(ctx, `SELECT user_id FROM identities WHERE issuer = ? AND subject = ?`, in.Issuer, in.Subject).Scan(&userID)
	switch {
	case err == nil:
		if _, err := tx.ExecContext(ctx, `UPDATE identities SET provider = ?, email = ?, last_login_at = ? WHERE issuer = ? AND subject = ?`,
			in.Provider, nullable(email), now, in.Issuer, in.Subject); err != nil {
			return SignInResult{}, err
		}
		u, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, userID))
		if err != nil {
			return SignInResult{}, err
		}
		return SignInResult{User: u}, tx.Commit()
	case !errors.Is(err, sql.ErrNoRows):
		return SignInResult{}, err
	}
	if email == "" || !in.Verified {
		return SignInResult{}, ErrUnverified
	}
	res := SignInResult{Linked: true}
	u, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE email = ?`, email))
	switch {
	case err == nil:
		var linked int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM identities WHERE user_id = ? AND issuer = ?)`, u.ID, in.Issuer).Scan(&linked); err != nil {
			return SignInResult{}, err
		}
		if linked == 1 {
			return SignInResult{User: u}, ErrLinkedElsewhere
		}
	case !errors.Is(err, sql.ErrNoRows):
		return SignInResult{}, err
	case !in.AllowCreate:
		return SignInResult{}, ErrNoAccount
	default:
		name, err := freeName(ctx, tx, in.Name)
		if err != nil {
			return SignInResult{}, err
		}
		if u, err = addUser(ctx, tx, name, RoleUser, "", email, time.Now()); err != nil {
			return SignInResult{}, err
		}
		res.Created = true
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO identities (user_id, issuer, subject, provider, email, linked_at, last_login_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, u.ID, in.Issuer, in.Subject, in.Provider, nullable(email), now, now); err != nil {
		return SignInResult{}, fmt.Errorf("link identity: %w", err)
	}
	res.User = u
	return res, tx.Commit()
}

// freeName is base, or base-2, base-3... when taken (cut so the name stays
// at most 64 characters).
func freeName(ctx context.Context, tx *sql.Tx, base string) (string, error) {
	for i := 1; ; i++ {
		name := base
		if i > 1 {
			suffix := "-" + strconv.Itoa(i)
			name = base[:min(len(base), 64-len(suffix))] + suffix
		}
		var taken int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE name = ?)`, name).Scan(&taken); err != nil {
			return "", err
		}
		if taken == 0 {
			return name, nil
		}
	}
}

// Identities lists a user's identities, oldest link first.
func (s *Store) Identities(ctx context.Context, userID int64) ([]Identity, error) {
	return s.identities(ctx, `WHERE user_id = ?`, userID)
}

// AllIdentities lists every identity by user.
func (s *Store) AllIdentities(ctx context.Context) (map[int64][]Identity, error) {
	ids, err := s.identities(ctx, ``)
	if err != nil {
		return nil, err
	}
	out := map[int64][]Identity{}
	for _, id := range ids {
		out[id.UserID] = append(out[id.UserID], id)
	}
	return out, nil
}

func (s *Store) identities(ctx context.Context, where string, args ...any) ([]Identity, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+identityCols+` FROM identities `+where+` ORDER BY linked_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		id, err := scanIdentity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Unlink removes a user's identity at an issuer and ends all the user's
// sessions. With self (users unlinking their own), leaving them without a
// password and without another identity is ErrLastWayIn. A missing user or
// identity is ErrNotFound.
func (s *Store) Unlink(ctx context.Context, name, issuer string, self bool) (Identity, error) {
	var gone Identity
	_, err := s.changeUser(ctx, name, false, func(tx *sql.Tx, u User) error {
		id, err := scanIdentity(tx.QueryRowContext(ctx, `SELECT `+identityCols+` FROM identities WHERE user_id = ? AND issuer = ?`, u.ID, issuer))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if self && !u.HasPassword() {
			var others int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM identities WHERE user_id = ? AND id <> ?`, u.ID, id.ID).Scan(&others); err != nil {
				return err
			}
			if others == 0 {
				return ErrLastWayIn
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM identities WHERE id = ?`, id.ID); err != nil {
			return err
		}
		gone = id
		_, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, u.ID)
		return err
	})
	return gone, err
}

// SessionCreatedAt is when a session started (a first password needs a
// fresh one).
func (s *Store) SessionCreatedAt(ctx context.Context, id int64) (time.Time, error) {
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT created_at FROM sessions WHERE id = ?`, id).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrNotFound
	}
	return fromMillis(at), err
}
