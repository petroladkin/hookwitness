//go:build !unix

package store

import "os"

// lockFile is not implemented here: compacting cannot tell a running server
// on this platform, and says so in its documentation.
func lockFile(path string, exclusive bool) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
}

// sameOwner gives dst the mode of src.
func sameOwner(src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	return os.Chmod(dst, fi.Mode().Perm())
}
