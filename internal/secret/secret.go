// Package secret keeps endpoint secrets at rest. Signing keys are sealed
// with AES-256-GCM, because verifying a signature needs the key itself;
// tokens and passwords are kept only as salted PBKDF2-SHA256 of their HMAC
// under the key, because checking them needs only a comparison. The key
// comes from HOOKWITNESS_SECRET_KEY or a key file next to the database; a
// copied database without it reveals nothing and cannot be brute-forced.
// Separate subkeys (HKDF) seal, fingerprint and hash.
package secret

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Iterations of PBKDF2 for new token hashes: about 6 ms per comparison on
// a 2026 laptop. A record keeps its own count, so this can grow.
const Iterations = 50000

// tokenPrefix marks a token record: PBKDF2-SHA256 over HMAC-SHA256(token
// subkey, value).
const tokenPrefix = "hk1-pbkdf2-sha256"

// ErrBusy is returned when too many token comparisons run at once: each
// costs a few milliseconds of CPU, so a flood of wrong tokens is bounded.
var ErrBusy = errors.New("too many token checks at once")

// Key seals, opens, fingerprints and hashes secrets.
type Key struct {
	aead     cipher.AEAD
	finger   []byte // subkey of fingerprints
	tokenKey []byte // subkey of token hashes

	mu      sync.Mutex
	matched map[[32]byte]bool // sha256(record, value) of successful token matches
	slots   chan struct{}     // concurrent PBKDF2 runs
}

// Load returns the key from env (base64 of 32 bytes) or, when env is empty,
// from the key file at path, created with mode 0600 when it does not exist.
func Load(env, path string) (*Key, error) {
	raw, err := loadRaw(env, path)
	if err != nil {
		return nil, err
	}
	sub := func(purpose string) []byte {
		k, _ := hkdf.Key(sha256.New, raw, nil, "hookwitness "+purpose, 32)
		return k
	}
	block, err := aes.NewCipher(sub("seal"))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Key{aead: aead, finger: sub("fingerprint"), tokenKey: sub("token"),
		matched: map[[32]byte]bool{}, slots: make(chan struct{}, max(1, runtime.GOMAXPROCS(0)/2))}, nil
}

func loadRaw(env, path string) ([]byte, error) {
	if env != "" {
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(env))
		if err != nil || len(b) != 32 {
			return nil, errors.New("HOOKWITNESS_SECRET_KEY must be 32 bytes in base64 (openssl rand -base64 32)")
		}
		return b, nil
	}
	st, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		raw := make([]byte, 32)
		rand.Read(raw)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, fmt.Errorf("secret key file: %w", err)
		}
		_, err = f.WriteString(base64.StdEncoding.EncodeToString(raw) + "\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, fmt.Errorf("secret key file: %w", err)
		}
		return raw, nil
	case err != nil:
		return nil, fmt.Errorf("secret key file: %w", err)
	case st.Mode().Perm()&0o077 != 0:
		return nil, fmt.Errorf("secret key file %s is readable by others; chmod 600 it", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("secret key file: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(b)))
	if err != nil || len(raw) != 32 {
		return nil, fmt.Errorf("secret key file %s is not 32 bytes in base64", path)
	}
	return raw, nil
}

// Seal encrypts a signing key for storage.
func (k *Key) Seal(value string) string {
	nonce := make([]byte, k.aead.NonceSize())
	rand.Read(nonce)
	return base64.StdEncoding.EncodeToString(k.aead.Seal(nonce, nonce, []byte(value), nil))
}

// Open decrypts a sealed signing key.
func (k *Key) Open(sealed string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(sealed)
	n := k.aead.NonceSize()
	if err != nil || len(b) < n {
		return "", errors.New("is damaged")
	}
	out, err := k.aead.Open(nil, b[:n], b[n:], nil)
	if err != nil {
		return "", errors.New("cannot be decrypted (wrong secret key?)")
	}
	return string(out), nil
}

// Fingerprint identifies a value without revealing it: keyed, so a weak
// secret cannot be guessed from it without the key; 8 hex characters.
func (k *Key) Fingerprint(value string) string {
	m := hmac.New(sha256.New, k.finger)
	m.Write([]byte(value))
	return hex.EncodeToString(m.Sum(nil))[:8]
}

// Check is a value that identifies the key without revealing it, stored
// with the database to notice a different key at startup.
func (k *Key) Check() string { return k.Fingerprint("hookwitness key check") }

// keyed is the value bound to this key before hashing: without the key a
// stored hash cannot even be guessed at.
func (k *Key) keyed(value string) string {
	m := hmac.New(sha256.New, k.tokenKey)
	m.Write([]byte(value))
	return string(m.Sum(nil))
}

// HashToken is the stored form of a token or password:
// "hk1-pbkdf2-sha256$<iterations>$<salt>$<hash>", base64.
func (k *Key) HashToken(value string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	h, _ := pbkdf2.Key(sha256.New, k.keyed(value), salt, Iterations, 32)
	return tokenPrefix + "$" + strconv.Itoa(Iterations) + "$" +
		base64.StdEncoding.EncodeToString(salt) + "$" + base64.StdEncoding.EncodeToString(h)
}

// MatchToken reports whether value is the token a record was made from.
// Successful matches are remembered for the life of the process, so
// regular traffic does not pay the hash on every request; failures are not,
// and at most a few hashes run at once: a caller that waits over a second
// for a slot gets ErrBusy.
func (k *Key) MatchToken(record, value string) (bool, error) {
	id := sha256.Sum256([]byte(record + "\x00" + value))
	k.mu.Lock()
	hit := k.matched[id]
	k.mu.Unlock()
	if hit {
		return true, nil
	}
	parts := strings.Split(record, "$")
	if len(parts) != 4 || parts[0] != tokenPrefix {
		return false, nil
	}
	iter, err1 := strconv.Atoi(parts[1])
	salt, err2 := base64.StdEncoding.DecodeString(parts[2])
	want, err3 := base64.StdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil || err3 != nil || iter < 1 || iter > 10_000_000 || len(want) == 0 {
		return false, nil
	}
	select {
	case k.slots <- struct{}{}:
	case <-time.After(time.Second):
		return false, ErrBusy
	}
	got, err := pbkdf2.Key(sha256.New, k.keyed(value), salt, iter, len(want))
	<-k.slots
	if err != nil || subtle.ConstantTimeCompare(got, want) != 1 {
		return false, nil
	}
	k.mu.Lock()
	if len(k.matched) >= 1024 {
		clear(k.matched)
	}
	k.matched[id] = true
	k.mu.Unlock()
	return true, nil
}
