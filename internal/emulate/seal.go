package emulate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/petroladkin/hookwitness/internal/auth"
)

// Sealer seals signing keys, hashes tokens and fingerprints values
// (internal/secret.Key).
type Sealer interface {
	Seal(value string) string
	HashToken(value string) string
	Fingerprint(value string) string
}

// SealSecret turns a plain value into a stored secret at once: a signing
// key sealed, a token hashed. The plain value goes no further than the
// caller.
func SealSecret(name, kind, value string, keys Sealer) (Secret, error) {
	s := Secret{Name: name, Kind: kind}
	switch {
	case keys == nil:
		return s, errors.New("no secret key is loaded")
	case value == "":
		return s, errors.New("enter the value")
	case strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n"):
		return s, errors.New("the value has spaces or line breaks at its ends; paste it exactly")
	}
	s.Fingerprint = keys.Fingerprint(value)
	switch kind {
	case SecretKey:
		s.Sealed = keys.Seal(value)
		_, err := auth.StandardKey(value)
		s.Standard = err == nil && strings.HasPrefix(value, "whsec_")
	case SecretToken:
		s.Hash = keys.HashToken(value)
	}
	return s, nil
}

// Version identifies a stored configuration, so an edit based on an older
// one is refused instead of hitting the wrong rule.
func Version(stored string) string {
	sum := sha256.Sum256([]byte(stored))
	return hex.EncodeToString(sum[:6])
}
