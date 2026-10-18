package access

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/petroladkin/hookwitness/internal/secret"
	"github.com/petroladkin/hookwitness/internal/store"
)

// Errors of authentication.
var (
	ErrWrongLogin   = errors.New("wrong name or password")
	ErrInvalidToken = errors.New("the token is not valid")
	ErrNoSession    = errors.New("no session") // absent, unknown or ended
	ErrBusy         = secret.ErrBusy           // too many hash checks at once
)

// Authenticator checks credentials against the store.
type Authenticator struct {
	store   *store.Store
	keys    *secret.Key
	machine []byte // HMAC of the machine token under mkey; nil: none
	mkey    []byte // a random key of this process
	now     func() time.Time
	dummy   string // a password record checked for unknown names, so timing tells nothing
}

// New returns an authenticator; machineToken is the machine token's value
// ("" for none).
func New(st *store.Store, keys *secret.Key, machineToken string) *Authenticator {
	a := &Authenticator{store: st, keys: keys, now: time.Now}
	if machineToken != "" {
		// Only the HMAC under a key of this process is kept, compared in
		// constant time.
		a.mkey = make([]byte, 32)
		rand.Read(a.mkey)
		a.machine = a.mac(machineToken)
	}
	a.dummy = keys.HashToken("not a password of anyone")
	return a
}

func (a *Authenticator) mac(v string) []byte {
	m := hmac.New(sha256.New, a.mkey)
	m.Write([]byte(v))
	return m.Sum(nil)
}

// SetClock replaces the clock (tests).
func (a *Authenticator) SetClock(now func() time.Time) { a.now = now }

// Now is the authenticator's clock.
func (a *Authenticator) Now() time.Time { return a.now() }

// Machine reports whether a machine token is configured.
func (a *Authenticator) Machine() bool { return a.machine != nil }

// LoginOn reports whether the UI requires a login: some user exists.
func (a *Authenticator) LoginOn(ctx context.Context) (bool, error) {
	return a.store.HasUsers(ctx)
}

// APILoginOn reports whether the API and /mcp require credentials: a user
// exists, or a machine token is configured.
func (a *Authenticator) APILoginOn(ctx context.Context) (bool, error) {
	if a.machine != nil {
		return true, nil
	}
	return a.store.HasUsers(ctx)
}

// HashPassword is the stored form of a password.
func (a *Authenticator) HashPassword(pw string) string { return a.keys.HashToken(pw) }

// Login checks a name and password: the user, or ErrWrongLogin (one error
// for both), or ErrBusy. An unknown name costs a hash too.
func (a *Authenticator) Login(ctx context.Context, name, pw string) (store.User, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	u, err := a.store.UserByName(ctx, n)
	record := u.Password
	switch {
	case errors.Is(err, store.ErrNotFound):
		record = a.dummy
	case err != nil:
		return store.User{}, err
	}
	ok, err := a.keys.MatchToken(record, pw)
	if err != nil {
		return store.User{}, err
	}
	if !ok || u.ID == 0 {
		return store.User{}, ErrWrongLogin
	}
	return u, nil
}

// StartSession records a login for u and returns the cookie's id.
func (a *Authenticator) StartSession(ctx context.Context, u store.User) (id string, sessionID int64, err error) {
	now := a.now()
	// Ended sessions go now and then: on every login.
	if err := a.store.SweepSessions(ctx, now.Add(-IdleLimit), now.Add(-AbsoluteLimit)); err != nil {
		return "", 0, err
	}
	id, hash := NewSessionID()
	sid, err := a.store.AddSession(ctx, hash, u.ID)
	return id, sid, err
}

// Session resolves a cookie value: the identity, or ErrNoSession when the
// session is unknown or has ended (an ended one is deleted). Activity is
// recorded at most once a minute.
func (a *Authenticator) Session(ctx context.Context, cookie string) (*Identity, error) {
	if !validSessionID(cookie) {
		return nil, ErrNoSession
	}
	se, err := a.store.SessionByHash(ctx, HashSession(cookie))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, err
	}
	now := a.now()
	if Expired(se.CreatedAt, se.LastSeenAt, now) {
		a.store.EndSession(ctx, se.ID)
		return nil, ErrNoSession
	}
	if now.Sub(se.LastSeenAt) >= touchEvery {
		if err := a.store.TouchSession(ctx, se.ID, now); err != nil {
			return nil, err
		}
	}
	return &Identity{UserID: se.UserID, Name: se.Name, Role: se.Role, Via: ViaSession, SessionID: se.ID}, nil
}

// Bearer resolves the value of an Authorization header: no identity and
// false when it is not a Bearer header; ErrInvalidToken for a token that is
// not valid (never saying why); ErrBusy.
func (a *Authenticator) Bearer(ctx context.Context, header string) (*Identity, bool, error) {
	tok, present := bearer(header)
	if !present {
		return nil, false, nil
	}
	id, err := a.Token(ctx, tok)
	return id, true, err
}

// Token resolves a token value: the machine token or a personal token.
func (a *Authenticator) Token(ctx context.Context, tok string) (*Identity, error) {
	if tok == "" {
		return nil, ErrInvalidToken
	}
	if a.machine != nil && hmac.Equal(a.mac(tok), a.machine) {
		return &Identity{Name: MachineName, Role: store.RoleUser, Via: ViaMachine}, nil
	}
	public, ok := ParseToken(tok)
	if !ok {
		return nil, ErrInvalidToken
	}
	t, err := a.store.TokenByPublicID(ctx, public)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, err
	}
	match, err := a.keys.MatchToken(t.Hash, tok)
	if err != nil {
		return nil, err
	}
	if !match {
		return nil, ErrInvalidToken
	}
	now := a.now()
	if t.LastUsedAt.IsZero() || now.Sub(t.LastUsedAt) >= touchEvery {
		if err := a.store.TouchToken(ctx, t.ID, now); err != nil {
			return nil, err
		}
	}
	return &Identity{UserID: t.UserID, Name: t.Owner, Role: t.Role, Via: ViaToken, TokenID: t.PublicID}, nil
}

// MintFor makes a personal token for a user and stores its hash: the value
// is returned once and never kept.
func (a *Authenticator) MintFor(ctx context.Context, userID int64, name string) (string, store.Token, error) {
	value, public := MintToken()
	t, err := a.store.AddToken(ctx, public, userID, name, a.keys.HashToken(value))
	return value, t, err
}

// ReadMachineToken reads the machine token file: one line of at least 32
// characters. warn is set when the file is readable by everyone (a
// container's secret mount often has to be: it is worth a log line, not a
// refusal).
func ReadMachineToken(path string) (tok, warn string, err error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", "", fmt.Errorf("HOOKWITNESS_API_TOKEN_FILE: %w", err)
	}
	if st.Mode().Perm()&0o004 != 0 {
		warn = "the machine token file is readable by everyone on this machine: chmod 600 it if the machine is shared"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("HOOKWITNESS_API_TOKEN_FILE: %w", err)
	}
	tok = strings.TrimRight(string(b), "\r\n")
	if strings.ContainsAny(tok, "\r\n") || len(tok) < 32 || strings.TrimSpace(tok) != tok {
		return "", "", fmt.Errorf("HOOKWITNESS_API_TOKEN_FILE %s must hold one line of at least 32 characters without spaces around it", path)
	}
	return tok, warn, nil
}
