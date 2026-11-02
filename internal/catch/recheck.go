package catch

import (
	"context"
	"time"

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
	if err := st.ForgetOrderMarks(ctx, ep.ID); err != nil {
		return 0, err
	}
	n := 0
	err = st.EachRequest(ctx, ep.ID, func(req store.Request) error {
		vs, _ := cfg.Verify(toEmulate(req), req.ReceivedAt, !req.TooLarge, keys)
		n++
		// Oldest first: a request's repeats are told against the earlier
		// requests' signatures as this re-check recorded them.
		er := toEmulate(req)
		return st.SetVerdicts(ctx, req.ID, emulate.Overall(vs), emulate.VerdictsJSON(vs), signatures(vs, keys), repeated(vs),
			delivery(cfg, er, vs, req.ReceivedAt))
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

// delivery gives the store a request's facts for the delivery checks and
// the judge that finishes their verdicts in vs against the request's
// relation to the earlier kept ones; nil when the endpoint has none. It
// shares vs with repeated, so the stored verdicts carry both.
func delivery(cfg emulate.Config, er emulate.Request, vs []auth.Verdict, received time.Time) *store.Delivery {
	f := cfg.DeliveryFacts(er)
	if f == nil {
		return nil
	}
	return &store.Delivery{Event: f.Event, ByBody: f.ByBody, Missing: f.Missing != "", Order: f.Order, Group: f.Group,
		Relate: func(rel store.Relation) (string, string) {
			r := emulate.Relation{Attempt: rel.Attempt}
			if rel.Prev != nil {
				r.Prev = &emulate.Attempt{No: rel.Prev.No, At: rel.Prev.At, Outcome: emulate.ParseOutcome(rel.Prev.Outcome, rel.Prev.Status)}
			}
			if rel.High != nil {
				r.High = &emulate.Mark{Value: rel.High.Value, No: rel.High.No}
			}
			cfg.JudgeDelivery(vs, *f, r, received)
			return emulate.Overall(vs), emulate.VerdictsJSON(vs)
		}}
}

// repeated notes the verdicts against the earlier requests the store found
// with the same signatures.
func repeated(vs []auth.Verdict) func(map[int]int64) string {
	return func(earlier map[int]int64) string {
		emulate.NoteRepeats(vs, earlier)
		return emulate.VerdictsJSON(vs)
	}
}
