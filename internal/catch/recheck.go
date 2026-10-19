package catch

import (
	"context"

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
		return st.SetVerdicts(ctx, req.ID, emulate.Overall(vs), emulate.VerdictsJSON(vs))
	})
	return n, err
}
