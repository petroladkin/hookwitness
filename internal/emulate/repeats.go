package emulate

import (
	"fmt"

	"github.com/petroladkin/hookwitness/internal/auth"
)

// Signature is a signature a request carried, as the keyed digest of the
// value its check read: a repeat can be told without keeping the value.
type Signature struct {
	Check  int // the verdict's check
	Digest string
}

// Signatures digests the signatures the verdicts read (signature checks
// record what they received, whatever their result; bearer and Basic
// checks record nothing: a repeated credential is normal). Nil without a
// key.
func Signatures(vs []auth.Verdict, keys Keyring) []Signature {
	if keys == nil {
		return nil
	}
	var out []Signature
	for _, v := range vs {
		if v.Received != "" {
			out = append(out, Signature{Check: v.Check, Digest: keys.Digest(v.Received)})
		}
	}
	return out
}

// NoteRepeats notes, on each passing verdict whose signature an earlier
// kept request of the endpoint carried (earlier: check → that request's
// number), which request that was. The result stays: a sender's own
// redelivery repeats a signature legitimately.
func NoteRepeats(vs []auth.Verdict, earlier map[int]int64) {
	for i, v := range vs {
		if no, ok := earlier[v.Check]; ok && v.Result == auth.Pass {
			vs[i].Note = RepeatNote(no)
		}
	}
}

// RepeatNote is the note on a verdict whose signature request no carried
// before.
func RepeatNote(no int64) string {
	return fmt.Sprintf("same signature as #%d: a replay, or a redelivery of the same signed request", no)
}
