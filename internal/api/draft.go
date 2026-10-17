package api

import (
	"net/http"

	"github.com/petroladkin/hookwitness/internal/catch"
	"github.com/petroladkin/hookwitness/internal/emulate"
)

// draftJSON is a schema drafted from requests; nothing is stored.
type draftJSON struct {
	Name          string          `json:"name"`
	Text          string          `json:"text"`
	Requests      []int64         `json:"requests"`
	Exists        bool            `json:"exists"`  // a schema of that name is stored; saving it replaces that one
	Changes       []string        `json:"changes"` // against the stored one; null when none is stored
	Discriminator *discriminatorJ `json:"discriminator"`
}

// discriminatorJ is the schema check that picks by a value, and the value
// the requests share, so the draft can be mapped to it.
type discriminatorJ struct {
	Check  int    `json:"check"` // 1-based
	Label  string `json:"label"`
	Value  string `json:"value"`  // "" when the requests differ or lack it
	Mapped string `json:"mapped"` // the schema the value maps to now; "" when none
}

// schemaDraft drafts a JSON Schema from some of an endpoint's requests.
func (h *Handler) schemaDraft(w http.ResponseWriter, r *http.Request) {
	ep, ok := h.endpoint(w, r)
	if !ok {
		return
	}
	var in struct {
		Requests []int64 `json:"requests"`
	}
	if !decode(w, r, &in, false) {
		return
	}
	reqs, msg, err := catch.DraftRequests(r.Context(), h.store, ep.ID, in.Requests)
	if err != nil {
		h.fail(w, "schema draft", err)
		return
	}
	if msg != "" {
		WriteError(w, http.StatusUnprocessableEntity, "invalid", msg, "requests")
		return
	}
	cfg, err := emulate.Parse(ep.Config)
	if err != nil {
		h.fail(w, "schema draft", err)
		return
	}
	d, err := catch.DraftSchema(cfg, ep.Path, reqs)
	if err != nil {
		WriteError(w, http.StatusUnprocessableEntity, "invalid", err.Error(), "requests")
		return
	}
	out := draftJSON{Name: d.Name, Text: d.Text, Requests: d.Requests, Exists: d.Exists, Changes: d.Changes}
	if d.Hint.Check > 0 {
		out.Discriminator = &discriminatorJ{Check: d.Hint.Check, Label: d.Hint.Label, Value: d.Hint.Value, Mapped: d.Hint.Mapped}
	}
	writeJSON(w, http.StatusOK, out)
}
