//go:build !linux && !darwin

package store

// volumeSpace is unknown off Linux and macOS.
func volumeSpace(string) (free, total int64, ok bool) { return 0, 0, false }
