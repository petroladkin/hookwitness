//go:build linux || darwin

package store

import (
	"path/filepath"
	"syscall"
)

// volumeSpace is the free (for this user) and total bytes of the volume
// that holds path.
func volumeSpace(path string) (free, total int64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(path), &st); err != nil {
		return 0, 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize), true
}
