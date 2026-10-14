package web

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/petroladkin/hookwitness/internal/catch"
	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/report"
	"github.com/petroladkin/hookwitness/internal/store"
)

// draftDialogView picks the requests a schema is drafted from: by number,
// or all that share a discriminator value.
type draftDialogView struct {
	Action string // the behaviour page, which opens the drafted schema
	Close  string
	By     string // pick or value
	Pick   string
	Label  string       // the discriminator check's label; "" without one
	Values []valueCount // the values the kept requests carry, most frequent first
	Value  string       // the one chosen
}

type valueCount struct {
	Value string
	N     int
}

// draftDefaults opens the dialog: the exhibit's number when one is open,
// and the discriminator values of the kept requests.
func (h *Handler) draftDefaults(ctx context.Context, ep store.Endpoint, ex *exhibitView, closeURL string) (*draftDialogView, error) {
	v := &draftDialogView{Action: h.regPath(ep.ID) + "/behaviour", Close: closeURL, By: "pick"}
	if ex != nil {
		v.Pick = strconv.FormatInt(ex.No, 10)
	}
	cfg, err := emulate.Parse(ep.Config)
	if err != nil {
		return nil, err
	}
	hint := cfg.DraftHint(ep.Path, nil)
	if hint.Check == 0 {
		return v, nil
	}
	v.Label = hint.Label
	counts := map[string]int{}
	err = h.store.EachSelected(ctx, ep.ID, store.Selection{}, true, func(r store.Request) error {
		if val := catch.DiscriminatorValue(cfg, r); val != "" {
			counts[val]++
		}
		return nil
	})
	for val, n := range counts {
		v.Values = append(v.Values, valueCount{val, n})
	}
	slices.SortFunc(v.Values, func(a, b valueCount) int {
		if a.N != b.N {
			return b.N - a.N
		}
		return strings.Compare(a.Value, b.Value)
	})
	if len(v.Values) > 0 && ex == nil {
		v.By, v.Value = "value", v.Values[0].Value
	}
	return v, err
}

// draftView is what the schema editor shows of a draft.
type draftView struct {
	From     string   // which requests it came from
	Exists   bool     // saving replaces the stored schema of that name
	Changes  []string // against that schema
	MapCheck int      // a discriminator check to map the value in; 0 for none
	MapLabel string
	MapValue string
}

// openDraft drafts a schema from the requests a URL names (pick=3,5-9 or
// value=order.paid) and opens the schema editor on it. why says, for the
// person, why there is no draft.
func (h *Handler) openDraft(ctx context.Context, ep store.Endpoint, cfg emulate.Config, q url.Values) (ed *editor, why string, err error) {
	var reqs []store.Request
	var from string
	switch {
	case q.Get("by") == "value" || (q.Get("by") == "" && q.Has("value")):
		val := q.Get("value")
		total := 0
		err = h.store.EachSelected(ctx, ep.ID, store.Selection{}, true, func(r store.Request) error {
			if catch.DiscriminatorValue(cfg, r) == val {
				total++
				reqs = append(reqs, r)
				if len(reqs) > catch.MaxDraftRequests {
					reqs = reqs[1:] // the newest ones
				}
			}
			return nil
		})
		if err != nil {
			return nil, "", err
		}
		label := cfg.DraftHint(ep.Path, nil).Label
		label = strings.TrimPrefix(label, "JSON Schema by ")
		from = fmt.Sprintf("the %d requests with %s = %s", len(reqs), label, val)
		if total > len(reqs) {
			from = fmt.Sprintf("the %d newest of %d requests with %s = %s", len(reqs), total, label, val)
		}
		if len(reqs) == 0 {
			return nil, fmt.Sprintf("no kept request has %s = %s", label, val), nil
		}
	default:
		nos, perr := report.ParseNumbers(q.Get("pick"), catch.MaxDraftRequests, "")
		if perr != nil {
			return nil, perr.Error(), nil
		}
		if len(nos) == 0 {
			return nil, "give the request numbers, such as 3 or 5-9", nil
		}
		err = h.store.EachSelected(ctx, ep.ID, store.Selection{Nos: nos}, true, func(r store.Request) error {
			reqs = append(reqs, r)
			return nil
		})
		if err != nil {
			return nil, "", err
		}
		if len(reqs) < len(nos) {
			return nil, "some of those numbers are not kept requests of this endpoint", nil
		}
		from = "request " + q.Get("pick")
		if len(nos) > 1 {
			from = "requests " + q.Get("pick")
		}
	}
	d, derr := catch.DraftSchema(cfg, ep.Path, reqs)
	if derr != nil {
		return nil, derr.Error(), nil
	}
	ed = &editor{Kind: "schema", I: "new", Schema: emulate.Schema{Name: d.Name, Text: d.Text},
		Draft: &draftView{From: from, Exists: d.Exists, Changes: d.Changes}}
	for i, sc := range cfg.Schemas {
		if sc.Name == d.Name {
			ed.I = strconv.Itoa(i) // saving replaces it
		}
	}
	if d.Hint.Check > 0 && d.Hint.Value != "" && d.Hint.Mapped == "" {
		ed.Draft.MapCheck, ed.Draft.MapLabel, ed.Draft.MapValue = d.Hint.Check, d.Hint.Label, d.Hint.Value
	}
	return ed, "", nil
}
