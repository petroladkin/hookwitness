package web

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/store"
)

// The strip's ranges: the paper shows the last span of time.
var spans = map[string]time.Duration{
	"2m":  2 * time.Minute,
	"10m": 10 * time.Minute,
	"1h":  time.Hour,
	"6h":  6 * time.Hour,
}

// marksAnswer is what the strip draws for a range: every kept request in it
// as a compact mark [id, at (unix ms), size, mark, verdict, group], and
// kept_from when the register no longer keeps the requests before its
// oldest one (pruned past the retention, or cleared).
type marksAnswer struct {
	From     int64   `json:"from"`
	KeptFrom *int64  `json:"kept_from"`
	Marks    [][]any `json:"marks"`
}

// marks answers the strip's data for ?span= (2m, 10m, 1h, 6h): it draws
// from here rather than from the rows loaded, which are one page. A range
// is read with a margin of a twentieth, so a bracket from just off the
// paper still starts somewhere.
func (h *Handler) marks(w http.ResponseWriter, r *http.Request) {
	reg, ok := regID(r)
	span, known := spans[r.URL.Query().Get("span")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !known {
		http.Error(w, "span: 2m, 10m, 1h or 6h", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	if reg != store.Unmatched {
		if _, err := h.store.Endpoint(ctx, reg); errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		} else if err != nil {
			h.fail(w, "marks", err)
			return
		}
	}
	from := time.Now().Add(-span - span/20)
	out := marksAnswer{From: from.UnixMilli(), Marks: [][]any{}}
	err := h.store.EachSelected(ctx, reg, store.Selection{From: from}, false, func(req store.Request) error {
		v := rowView{TooLarge: req.TooLarge, Outcome: emulate.ParseOutcome(req.Outcome, req.Status)}
		group := ""
		if len(req.BodySHA256) > 0 {
			group = hex.EncodeToString(req.BodySHA256[:6])
		}
		out.Marks = append(out.Marks, []any{req.ID, req.ReceivedAt.UnixMilli(), req.Size, v.mark(), req.Verdict, group})
		return nil
	})
	if err != nil {
		h.fail(w, "marks", err)
		return
	}
	if at, ok, err := h.store.KeptFrom(ctx, reg); err != nil {
		h.fail(w, "marks", err)
		return
	} else if ok {
		ms := at.UnixMilli()
		out.KeptFrom = &ms
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(out)
}
