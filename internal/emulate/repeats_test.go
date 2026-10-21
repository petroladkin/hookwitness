package emulate

import (
	"testing"

	"github.com/petroladkin/hookwitness/internal/auth"
)

func (keyring) Digest(value string) string { return "digest:" + value }

func TestSignatures(t *testing.T) {
	vs := []auth.Verdict{
		{Check: 1, Result: auth.Pass, Received: "sha256=aa"},
		{Check: 2, Result: auth.Pass},                        // a bearer token: nothing received to tell
		{Check: 3, Result: auth.Fail, Received: "v1,bb"},     // a failed one is recorded too
		{Check: 4, Result: auth.NotVerifiable, Received: ""}, // nothing read
	}
	got := Signatures(vs, keyring{})
	if len(got) != 2 || got[0] != (Signature{1, "digest:sha256=aa"}) || got[1] != (Signature{3, "digest:v1,bb"}) {
		t.Fatalf("%+v", got)
	}
	if Signatures(vs, nil) != nil {
		t.Fatal("without a key nothing can be digested")
	}
}

func TestNoteRepeats(t *testing.T) {
	vs := []auth.Verdict{
		{Check: 1, Result: auth.Pass, Received: "sha256=aa"},
		{Check: 2, Result: auth.Fail, Received: "v1,bb"},
		{Check: 3, Result: auth.Pass, Received: "x"},
	}
	NoteRepeats(vs, map[int]int64{1: 7, 2: 3})
	if vs[0].Note != "same signature as #7: a replay, or a redelivery of the same signed request" {
		t.Fatalf("note %q", vs[0].Note)
	}
	if vs[1].Note != "" || vs[2].Note != "" {
		t.Fatalf("a failed verdict or one without an earlier request got a note: %+v", vs)
	}
	if vs[0].Result != auth.Pass {
		t.Fatal("the note changed the result")
	}
}
