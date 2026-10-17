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
	"path/filepath"
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

	notes []string // worth a warning from the caller (Notes)
}

// Notes are things about how the key was loaded worth a warning in the
// server's log: a key file that could not be created atomically.
func (k *Key) Notes() []string { return k.notes }

// Load returns the key from env (base64 of 32 bytes) or, when env is empty,
// from the key file at path, created with mode 0600 when it does not exist
// (atomically: two servers starting together read the same key).
func Load(env, path string) (*Key, error) {
	raw, notes, err := loadRaw(env, path)
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
	return &Key{aead: aead, finger: sub("fingerprint"), tokenKey: sub("token"), notes: notes,
		matched: map[[32]byte]bool{}, slots: make(chan struct{}, max(1, runtime.GOMAXPROCS(0)/2))}, nil
}

func loadRaw(env, path string) ([]byte, []string, error) {
	if env != "" {
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(env))
		if err != nil || len(b) != 32 {
			return nil, nil, errors.New("HOOKWITNESS_SECRET_KEY must be 32 bytes in base64 (openssl rand -base64 32)")
		}
		return b, nil, nil
	}
	var notes []string
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		atomic, cerr := createKeyFile(path)
		if cerr != nil {
			return nil, nil, fmt.Errorf("secret key file: %w", cerr)
		}
		if !atomic {
			notes = append(notes, "the key file was created without a hard link (the file system has none): "+
				"two servers starting together on this database could each make one")
		}
		// Whoever created it, the file on disk is the key from now on.
		st, err = os.Stat(path)
	}
	switch {
	case err != nil:
		return nil, nil, fmt.Errorf("secret key file: %w", err)
	case st.Mode().Perm()&0o077 != 0:
		return nil, nil, fmt.Errorf("secret key file %s is readable by others; chmod 600 it", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("secret key file: %w", err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil, fmt.Errorf("secret key file %s is empty: left by an interrupted first start of an older version; "+
			"delete it if no secrets were stored yet, or restore the key", path)
	}
	raw, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(b)))
	if err != nil || len(raw) != 32 {
		return nil, nil, fmt.Errorf("secret key file %s is not 32 bytes in base64", path)
	}
	return raw, notes, nil
}

// linkFile links a finished key into place (a variable for tests).
var linkFile = os.Link

// beforeLink runs just before the link (a variable for tests: a crash there).
var beforeLink = func() {}

// createKeyFile makes a new key at path so that the name only ever holds a
// whole key: it is written and synced to a temporary file beside it, then
// hard-linked to path, which fails if another process got there first (that
// one's key is then read). A file system without hard links falls back to
// creating path exclusively and writing it, which is not atomic: atomic is
// false then.
func createKeyFile(path string) (atomic bool, err error) {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	removeStaleTemps(dir, base)
	raw := make([]byte, 32)
	rand.Read(raw)
	text := base64.StdEncoding.EncodeToString(raw) + "\n"

	f, err := os.CreateTemp(dir, base+".tmp-*") // mode 0600
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, err = f.WriteString(text)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return false, err
	}
	beforeLink()
	switch err := linkFile(tmp, path); {
	case err == nil, errors.Is(err, fs.ErrExist):
		syncDir(dir)
		return true, nil
	}
	// No hard links here: create it in place.
	g, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = g.WriteString(text)
	if err == nil {
		err = g.Sync()
	}
	if cerr := g.Close(); err == nil {
		err = cerr
	}
	return false, err
}

// removeStaleTemps deletes temporary key files a crash left behind (older
// than a minute, so a key being made right now is left alone).
func removeStaleTemps(dir, base string) {
	matches, _ := filepath.Glob(filepath.Join(dir, base+".tmp-*"))
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && time.Since(st.ModTime()) > time.Minute {
			os.Remove(m)
		}
	}
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
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
