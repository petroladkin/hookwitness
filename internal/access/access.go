// Package access decides who may use the admin once login is on: user
// names and passwords, browser sessions, personal tokens and the machine
// token, the limit on failed logins, and the identity a request carries.
// Login is on once a user exists (or, for the API and /mcp, once a machine
// token is configured); before that everything is open, as it always was.
// Values (passwords, tokens, session ids) are never logged or stored in the
// clear: passwords and tokens as secret.HashToken records, session ids as
// their SHA-256.
package access

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/petroladkin/hookwitness/internal/store"
)

// Password limits: long enough to resist guessing, no composition rules.
const (
	MinPassword = 12
	MaxPassword = 1024 // bytes
)

// Via is how a request proved who it is.
type Via int

const (
	ViaSession Via = iota + 1 // the browser's login cookie
	ViaToken                  // a personal token
	ViaMachine                // the machine token from HOOKWITNESS_API_TOKEN_FILE
)

func (v Via) String() string {
	switch v {
	case ViaSession:
		return "session"
	case ViaToken:
		return "token"
	case ViaMachine:
		return "machine token"
	}
	return "none"
}

// MachineName names the machine token in logs and answers.
const MachineName = "machine"

// Identity is who a request acts as. The role is read from the store on
// every request, so a demoted or removed user's sessions and tokens follow
// at once.
type Identity struct {
	UserID    int64 // 0 for the machine token
	Name      string
	Role      string // store.RoleUser or store.RoleAdmin
	Via       Via
	SessionID int64  // the session's row, for ViaSession
	TokenID   string // the token's public id, for ViaToken
}

// Admin reports whether the identity may manage users.
func (id *Identity) Admin() bool { return id != nil && id.Role == store.RoleAdmin }

type ctxKey struct{}

// With returns ctx carrying id.
func With(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// From returns the identity ctx carries, or nil.
func From(ctx context.Context) *Identity {
	id, _ := ctx.Value(ctxKey{}).(*Identity)
	return id
}

// Who is the log attribute naming who made a change: "user", the name; none
// when nobody is logged in (login off).
func Who(ctx context.Context) []any {
	if id := From(ctx); id != nil {
		return []any{"user", id.Name}
	}
	return nil
}

// ErrInvalid is a name or password that does not meet the rules; its
// message is for the person who typed it.
type ErrInvalid struct{ Field, Message string }

func (e *ErrInvalid) Error() string { return e.Message }

// CheckName returns a user or token name lower-cased when it is 1-64
// characters of a-z 0-9 . _ - (user names compare without case).
func CheckName(name string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" || len(n) > 64 || strings.Trim(n, "abcdefghijklmnopqrstuvwxyz0123456789._-") != "" {
		return "", &ErrInvalid{"name", "a name is 1 to 64 characters: letters, digits, . _ -"}
	}
	return n, nil
}

// CheckEmail returns an email address lower-cased when it is 3-254 bytes,
// exactly one @, no spaces or control characters, and a domain with a dot.
// Provider sign-in compares the whole address without case.
func CheckEmail(email string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(email))
	local, domain, ok := strings.Cut(e, "@")
	if !ok || len(e) < 3 || len(e) > 254 || local == "" || strings.Contains(domain, "@") || !strings.Contains(domain, ".") ||
		strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") ||
		strings.ContainsFunc(e, func(r rune) bool { return r <= 0x20 || r == 0x7f || unicode.IsSpace(r) }) {
		return "", &ErrInvalid{"email", "an email is an address such as ann@example.com"}
	}
	return e, nil
}

// NameFromEmail derives a user name from an email's local part: lower-case,
// characters outside a-z 0-9 . _ - become -, at most 64 ("user" when
// nothing is left). The store adds -2, -3... when it is taken.
func NameFromEmail(email string) string {
	local, _, _ := strings.Cut(strings.ToLower(email), "@")
	var b strings.Builder
	for _, r := range local {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	n := b.String()
	if len(n) > 64 {
		n = n[:64]
	}
	if strings.Trim(n, "-._") == "" {
		return "user"
	}
	return n
}

// CheckTokenName validates the name of a personal token: 1-64 printable
// characters (it is shown, never parsed).
func CheckTokenName(name string) (string, error) {
	n := strings.TrimSpace(name)
	if n == "" || utf8.RuneCountInString(n) > 64 || strings.ContainsFunc(n, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", &ErrInvalid{"name", "a token name is 1 to 64 characters"}
	}
	return n, nil
}

// CheckPassword enforces the length rules.
func CheckPassword(pw string) error {
	switch {
	case utf8.RuneCountInString(pw) < MinPassword:
		return &ErrInvalid{"password", fmt.Sprintf("a password is at least %d characters", MinPassword)}
	case len(pw) > MaxPassword:
		return &ErrInvalid{"password", fmt.Sprintf("a password is at most %d bytes", MaxPassword)}
	}
	return nil
}

// IsInvalid reports a validation error and its field.
func IsInvalid(err error) (*ErrInvalid, bool) {
	var e *ErrInvalid
	ok := errors.As(err, &e)
	return e, ok
}

// MCPExtra is the key of the identity in the MCP SDK's TokenInfo.Extra.
const MCPExtra = "hookwitness.identity"
