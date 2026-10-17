package catch

import (
	"context"
	"errors"
	"fmt"

	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/schema"
	"github.com/petroladkin/hookwitness/internal/store"
)

// MaxDraftRequests bounds how many requests one schema draft reads.
const MaxDraftRequests = 100

// SchemaDraft is a JSON Schema drafted from an endpoint's requests, with
// what the person needs to save it: a name, the discriminator check that
// could map to it, and, when a schema of that name exists, how the draft
// differs from it. Nothing is stored.
type SchemaDraft struct {
	Name     string
	Text     string
	Hint     emulate.DraftHint
	Exists   bool     // a schema of that name is stored: saving replaces it
	Changes  []string // against the stored one; nil when none is stored
	Requests []int64  // the request ids drafted from, oldest first
}

// DraftSchema drafts a schema from requests of an endpoint (cfg and path
// are its own). An error says, for the person, why there is no draft.
func DraftSchema(cfg emulate.Config, path string, reqs []store.Request) (SchemaDraft, error) {
	if len(reqs) == 0 {
		return SchemaDraft{}, errors.New("choose at least one request")
	}
	if len(reqs) > MaxDraftRequests {
		return SchemaDraft{}, fmt.Errorf("at most %d requests at a time", MaxDraftRequests)
	}
	var bodies [][]byte
	var ers []emulate.Request
	d := SchemaDraft{}
	for _, r := range reqs {
		if r.TooLarge {
			return SchemaDraft{}, fmt.Errorf("request %d: its body was over the limit and was not kept", r.No)
		}
		if msg := schema.BodyError(r.Body); msg != "" {
			return SchemaDraft{}, fmt.Errorf("request %d: %s", r.No, msg)
		}
		bodies = append(bodies, r.Body)
		ers = append(ers, toEmulate(r))
		d.Requests = append(d.Requests, r.ID)
	}
	text, err := schema.Draft(bodies)
	if err != nil {
		return SchemaDraft{}, err
	}
	d.Text = text
	d.Hint = cfg.DraftHint(path, ers)
	d.Name = d.Hint.Name
	for _, sc := range cfg.Schemas {
		if sc.Name == d.Name {
			d.Exists = true
			if d.Changes, err = schema.Changes(sc.Text, text); err != nil {
				d.Changes = []string{err.Error()}
			}
			if d.Changes == nil {
				d.Changes = []string{}
			}
		}
	}
	return d, nil
}

// DraftRequests reads the requests of an endpoint by id, with bodies, for a
// draft. msg says why the ids cannot be drafted from (an id that is not the
// endpoint's, too many); err is a failure of the store.
func DraftRequests(ctx context.Context, st *store.Store, endpointID int64, ids []int64) (reqs []store.Request, msg string, err error) {
	if len(ids) > MaxDraftRequests {
		return nil, fmt.Sprintf("at most %d requests at a time", MaxDraftRequests), nil
	}
	for _, id := range ids {
		r, err := st.Request(ctx, id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && r.EndpointID != endpointID) {
			return nil, fmt.Sprintf("request %d is not one of this endpoint's", id), nil
		}
		if err != nil {
			return nil, "", err
		}
		reqs = append(reqs, r)
	}
	return reqs, "", nil
}

func toEmulate(r store.Request) emulate.Request {
	er := emulate.Request{Method: r.Method, Path: r.Path, RawQuery: r.RawQuery, Body: r.Body}
	for _, hd := range r.Headers {
		er.Headers = append(er.Headers, emulate.Header{Name: hd.Name, Value: hd.Value})
	}
	return er
}

// DiscriminatorValue is the value a request carries for the endpoint's
// first discriminator check; "" without one, or when it lacks the value.
func DiscriminatorValue(cfg emulate.Config, r store.Request) string {
	return cfg.DraftHint("", []emulate.Request{toEmulate(r)}).Value
}
