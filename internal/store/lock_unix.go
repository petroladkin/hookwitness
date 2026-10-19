//go:build unix

package store

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// lockFile takes an advisory lock on path (created if missing): shared for
// every process that uses the database, exclusive for compacting it. It
// never waits: errLocked when the other kind is held. The lock lasts until
// the returned file is closed.
func lockFile(path string, exclusive bool) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, err
	}
	return f, nil
}

// sameOwner gives dst the mode and, when it differs, the owner of src: the
// compacted file replaces the database, which the server's user must still
// be able to write. An owner that cannot be given (compacting as another
// user without the right) is an error.
func sameOwner(src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.Chmod(dst, fi.Mode().Perm()); err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	dfi, err := os.Stat(dst)
	if err != nil {
		return err
	}
	if dst, ok := dfi.Sys().(*syscall.Stat_t); ok && dst.Uid == st.Uid && dst.Gid == st.Gid {
		return nil
	}
	if err := os.Chown(dst, int(st.Uid), int(st.Gid)); err != nil {
		return fmt.Errorf("the database belongs to user %d, and the new file cannot be given to it (run as that user): %w", st.Uid, err)
	}
	return nil
}
