package catch

import (
	"context"

	"github.com/petroladkin/hookwitness/internal/auth"
	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/store"
)

// Recheck verifies an endpoint's kept requests again with its current
// checks, secrets and schemas, and stores the new verdicts. Only the
// verdicts change: the status and the reply stay what the sender saw. It
// returns how many requests were checked. The requests are read in pages
// bounded by their bodies' bytes (store.PageBytes).
func Recheck(ctx context.Context, st *store.Store, ep store.Endpoint, keys emulate.Keyring) (int, error) {
	cfg, err := emulate.Parse(ep.Config)
	if err != nil {
		return 0, err
	}
	n := 0
	err = st.EachRequest(ctx, ep.ID, func(req store.Request) error {
		vs, _ := cfg.Verify(toEmulate(req), req.ReceivedAt, !req.TooLarge, keys)
		n++
		// Oldest first: a request's repeats are told against the earlier
		// requests' signatures as this re-check recorded them.
		return st.SetVerdicts(ctx, req.ID, emulate.Overall(vs), emulate.VerdictsJSON(vs), signatures(vs, keys), repeated(vs))
	})
	return n, err
}

// signatures are the digests of the signatures the verdicts read, as the
// store keeps them.
func signatures(vs []auth.Verdict, keys emulate.Keyring) []store.Signature {
	var out []store.Signature
	for _, sg := range emulate.Signatures(vs, keys) {
		out = append(out, store.Signature{Check: sg.Check, Digest: sg.Digest})
	}
	return out
}

// repeated notes the verdicts against the earlier requests the store found
// with the same signatures.
func repeated(vs []auth.Verdict) func(map[int]int64) string {
	return func(earlier map[int]int64) string {
		emulate.NoteRepeats(vs, earlier)
		return emulate.VerdictsJSON(vs)
	}
}
