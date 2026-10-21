package secret

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKey(t *testing.T) *Key {
	t.Helper()
	k, err := Load(base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")), "")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSealOpen(t *testing.T) {
	k := testKey(t)
	a, b := k.Seal("whsec_test_1"), k.Seal("whsec_test_1")
	if a == b {
		t.Fatal("two seals of one value are equal: the nonce is not random")
	}
	if strings.Contains(a, "whsec_test_1") {
		t.Fatal("the sealed value contains the secret")
	}
	for _, s := range []string{a, b} {
		if got, err := k.Open(s); err != nil || got != "whsec_test_1" {
			t.Fatalf("open: %q %v", got, err)
		}
	}
}

func TestTamperAndWrongKey(t *testing.T) {
	k := testKey(t)
	s := k.Seal("whsec_test_1")
	raw, _ := base64.StdEncoding.DecodeString(s)
	raw[len(raw)-1] ^= 1
	if _, err := k.Open(base64.StdEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("a tampered value opened")
	}
	if _, err := k.Open("***"); err == nil {
		t.Fatal("garbage opened")
	}
	other, _ := Load(base64.StdEncoding.EncodeToString([]byte("fedcba9876543210fedcba9876543210")), "")
	if _, err := other.Open(s); err == nil || !strings.Contains(err.Error(), "wrong secret key") {
		t.Fatalf("wrong key: %v", err)
	}
}

func TestKeyFromEnv(t *testing.T) {
	for _, bad := range []string{"short", base64.StdEncoding.EncodeToString([]byte("16 bytes only..."))} {
		if _, err := Load(bad, ""); err == nil || !strings.Contains(err.Error(), "32 bytes in base64") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hw.db.key")
	k1, err := Load("", path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode: %v %v", st.Mode(), err)
	}
	k2, err := Load("", path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := k2.Open(k1.Seal("x")); err != nil || got != "x" {
		t.Fatal("the key file is not reused")
	}
	os.Chmod(path, 0o644)
	if _, err := Load("", path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("a readable key file: %v", err)
	}
	os.Chmod(path, 0o600)
	os.WriteFile(path, []byte("not a key\n"), 0o600)
	if _, err := Load("", path); err == nil {
		t.Fatal("a broken key file accepted")
	}
}

func TestFingerprintKeyed(t *testing.T) {
	k := testKey(t)
	other, _ := Load(base64.StdEncoding.EncodeToString([]byte("fedcba9876543210fedcba9876543210")), "")
	f := k.Fingerprint("whsec_test_1")
	if len(f) != 8 || f != k.Fingerprint("whsec_test_1") || f == other.Fingerprint("whsec_test_1") || f == k.Fingerprint("whsec_test_2") {
		t.Fatalf("fingerprint %q", f)
	}
}

func TestHashToken(t *testing.T) {
	k := testKey(t)
	h1, h2 := k.HashToken("tok_test_123"), k.HashToken("tok_test_123")
	if h1 == h2 {
		t.Fatal("two hashes of one value are equal: no salt")
	}
	if !strings.HasPrefix(h1, "hk1-pbkdf2-sha256$50000$") || strings.Contains(h1, "tok_test_123") {
		t.Fatalf("record %q", h1)
	}
	for _, c := range []struct {
		record, value string
		want          bool
	}{
		{h1, "tok_test_123", true},
		{h2, "tok_test_123", true},
		{h1, "tok_test_124", false},
		{h1, "", false},
		{"hk1-pbkdf2-sha256$x$y$z", "tok_test_123", false},
		{"", "tok_test_123", false},
	} {
		if got, err := k.MatchToken(c.record, c.value); got != c.want || err != nil {
			t.Errorf("MatchToken(%q, %q) = %v, %v", c.record, c.value, got, err)
		}
	}
	// The hash is bound to the key: another key cannot match it.
	other, _ := Load(base64.StdEncoding.EncodeToString([]byte("fedcba9876543210fedcba9876543210")), "")
	if ok, _ := other.MatchToken(h1, "tok_test_123"); ok {
		t.Fatal("a token hash matched under another key")
	}
}

func TestTokenChecksAreBounded(t *testing.T) {
	k := testKey(t)
	rec := k.HashToken("tok_test_123")
	for range cap(k.slots) {
		k.slots <- struct{}{} // every slot taken
	}
	if ok, err := k.MatchToken(rec, "wrong"); ok || err != ErrBusy {
		t.Fatalf("saturated: %v %v", ok, err)
	}
	for range cap(k.slots) {
		<-k.slots
	}
	if ok, err := k.MatchToken(rec, "tok_test_123"); !ok || err != nil {
		t.Fatalf("free again: %v %v", ok, err)
	}
	for range cap(k.slots) {
		k.slots <- struct{}{}
	}
	if ok, err := k.MatchToken(rec, "tok_test_123"); !ok || err != nil {
		t.Fatalf("a remembered match needs no slot: %v %v", ok, err)
	}
}

func BenchmarkMatchTokenUncached(b *testing.B) {
	k, _ := Load(base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")), "")
	rec := k.HashToken("tok_test_123")
	for b.Loop() {
		k.MatchToken(rec, "wrong") //nolint:errcheck // failures are not cached
	}
}

// TestDigestKeyed: a digest is long, keyed, stable, its own subkey's (not a
// fingerprint's prefix) and does not contain the value.
func TestDigestKeyed(t *testing.T) {
	k := testKey(t)
	other, _ := Load(base64.StdEncoding.EncodeToString([]byte("fedcba9876543210fedcba9876543210")), "")
	d := k.Digest("sha256=abc")
	if len(d) != 64 || d != k.Digest("sha256=abc") || d == other.Digest("sha256=abc") || d == k.Digest("sha256=abd") {
		t.Fatalf("digest %q", d)
	}
	if strings.HasPrefix(d, k.Fingerprint("sha256=abc")) || strings.Contains(d, "abc") {
		t.Fatalf("digest %q shares the fingerprint or shows the value", d)
	}
}
