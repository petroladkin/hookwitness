package store

import (
	"context"
	"fmt"
	"strconv"
)

// Storage is what the database holds against its size limit and what is
// left on its volume.
type Storage struct {
	Used   int64 // bytes in used pages (Usage)
	Limit  int64 // -max-db-size; 0: none
	Pinned int64 // about what pinned requests hold (PinnedBytes)
	// The volume of the database file: free for this user and total, when
	// the system tells (Linux, macOS).
	VolumeFree, VolumeTotal int64
	VolumeKnown             bool
	// Stuck: the last eviction could not get under the limit's target,
	// because what is left is pinned.
	Stuck bool
}

// Storage reads the database's size and its volume's space, against limit.
func (s *Store) Storage(ctx context.Context, limit int64) (Storage, error) {
	st := Storage{Limit: limit}
	var err error
	if st.Used, err = s.Usage(ctx); err != nil {
		return st, err
	}
	if st.Pinned, err = s.PinnedBytes(ctx); err != nil {
		return st, err
	}
	st.VolumeFree, st.VolumeTotal, st.VolumeKnown = volumeSpace(s.path)
	st.Stuck = limit > 0 && s.stuck.Load()
	return st, nil
}

// Warning thresholds.
const (
	WarnShare      = 90      // % of the limit
	WarnVolumeFree = 1 << 30 // bytes free on the volume
	WarnVolumeLow  = 5       // % of the volume free
)

// Warnings say what needs a look: the database near its limit, pinned
// requests over it (nothing more can be dropped), the volume running out.
func (st Storage) Warnings() []string {
	var out []string
	if st.Limit > 0 && st.Pinned > st.Limit {
		out = append(out, "pinned requests alone hold more than the database size limit ("+Size(st.Pinned)+" of "+Size(st.Limit)+
			"): nothing more can be dropped; unpin some, or raise -max-db-size")
	} else if st.Limit > 0 && st.Stuck {
		out = append(out, "every request left is pinned, and the database cannot get under its size limit ("+Size(st.Used)+" of "+
			Size(st.Limit)+", "+Size(st.Pinned)+" pinned): each new request is dropped at the next check; unpin some, or raise -max-db-size")
	} else if st.Limit > 0 && st.Used*100 >= st.Limit*WarnShare {
		out = append(out, fmt.Sprintf("the database is at %d %% of its size limit (%s of %s): the oldest bodies, then requests, are being dropped",
			st.Used*100/st.Limit, Size(st.Used), Size(st.Limit)))
	}
	if st.VolumeKnown && st.VolumeTotal > 0 && (st.VolumeFree < WarnVolumeFree || st.VolumeFree*100 < st.VolumeTotal*WarnVolumeLow) {
		out = append(out, "the volume of the database has only "+Size(st.VolumeFree)+" free of "+Size(st.VolumeTotal)+
			": set -max-db-size, lower retention or free space before it fills")
	}
	return out
}

// Size formats bytes for people: "412 B", "2.0 KiB", "1.5 MiB", "1.8 GiB".
func Size(n int64) string {
	switch {
	case n < 1<<10:
		return strconv.FormatInt(n, 10) + " B"
	case n < 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<10), 'f', 1, 64) + " KiB"
	case n < 1<<30:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MiB"
	}
	return strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64) + " GiB"
}
