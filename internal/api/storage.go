package api

import (
	"net/http"
)

// storageJSON is the database's size against its limit, and its volume.
type storageJSON struct {
	UsedBytes   int64    `json:"used_bytes"`         // in used pages (the WAL not counted)
	LimitBytes  *int64   `json:"limit_bytes"`        // -max-db-size; null: none
	PinnedBytes int64    `json:"pinned_bytes"`       // about what pinned requests hold: never dropped
	VolumeFree  *int64   `json:"volume_free_bytes"`  // null where the system does not tell
	VolumeTotal *int64   `json:"volume_total_bytes"` // null where the system does not tell
	Warnings    []string `json:"warnings"`
}

func (h *Handler) storage(w http.ResponseWriter, r *http.Request) {
	st, err := h.store.Storage(r.Context(), h.o.MaxDBSize)
	if err != nil {
		h.fail(w, "storage", err)
		return
	}
	v := storageJSON{UsedBytes: st.Used, PinnedBytes: st.Pinned, Warnings: st.Warnings()}
	if v.Warnings == nil {
		v.Warnings = []string{}
	}
	if st.Limit > 0 {
		v.LimitBytes = &st.Limit
	}
	if st.VolumeKnown {
		v.VolumeFree, v.VolumeTotal = &st.VolumeFree, &st.VolumeTotal
	}
	writeJSON(w, http.StatusOK, v)
}
