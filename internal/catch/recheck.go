package catch

import (
	"context"

	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/store"
)

// Recheck verifies an endpoint's kept requests again with its current
// checks, secrets and schemas, and stores the new verdicts. Only the
// verdicts change: the status and the reply stay what the sender saw. It
// returns how many requests were checked.
func Recheck(ctx context.Context, st *store.Store, ep store.Endpoint, keys emulate.Keyring) (int, error) {
	cfg, err := emulate.Parse(ep.Config)
	if err != nil {
		return 0, err
	}
	n := 0
	err = st.EachRequest(ctx, ep.ID, func(req store.Request) error {
		er := emulate.Request{Method: req.Method, Path: req.Path, RawQuery: req.RawQuery, Body: req.Body}
		for _, hd := range req.Headers {
			er.Headers = append(er.Headers, emulate.Header{Name: hd.Name, Value: hd.Value})
		}
		vs, _ := cfg.Verify(er, req.ReceivedAt, !req.TooLarge, keys)
		n++
		return st.SetVerdicts(ctx, req.ID, emulate.Overall(vs), emulate.VerdictsJSON(vs))
	})
	return n, err
}
