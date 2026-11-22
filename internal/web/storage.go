package web

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/petroladkin/hookwitness/internal/store"
)

// storageCache keeps the database's numbers for a few seconds: every page
// shows its warnings.
type storageCache struct {
	mu  sync.Mutex
	at  time.Time
	val store.Storage
	err error
}

const storageFresh = 5 * time.Second

// storage is the database's size against the limit, at most storageFresh old.
func (h *Handler) storage(ctx context.Context) (store.Storage, error) {
	c := &h.stor
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) < storageFresh {
		return c.val, c.err
	}
	c.val, c.err = h.store.Storage(ctx, h.maxDB)
	c.at = time.Now()
	return c.val, c.err
}

// storageLine is the endpoints page's line about the database: "Database
// 1.8 GiB of 4.0 GiB · pinned 120.0 MiB · the volume has 50.0 GiB free".
func storageLine(st store.Storage) string {
	parts := []string{"Database " + store.Size(st.Used)}
	if st.Limit > 0 {
		parts[0] += " of " + store.Size(st.Limit)
	} else {
		parts = append(parts, "no size limit (-max-db-size)")
	}
	if st.Pinned > 0 {
		parts = append(parts, "pinned "+store.Size(st.Pinned))
	}
	if st.VolumeKnown {
		parts = append(parts, "the volume has "+store.Size(st.VolumeFree)+" free of "+store.Size(st.VolumeTotal))
	}
	return strings.Join(parts, " · ")
}
