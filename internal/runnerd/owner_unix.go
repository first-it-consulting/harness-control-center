//go:build unix

package runnerd

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// checkRootOwned fails unless path and every directory above it are owned
// by root and not writable by group or others (a writable parent would
// allow replacing the file).
func checkRootOwned(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	for p := path; ; p = filepath.Dir(p) {
		fi, err := os.Stat(p)
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("cannot read the owner of %s", p)
		}
		if st.Uid != 0 {
			return fmt.Errorf("%s must be owned by root (it is run as root via sudo)", p)
		}
		if fi.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("%s must not be writable by group or others", p)
		}
		if p == filepath.Dir(p) {
			return nil
		}
	}
}

// checkOwnedBySelf fails unless path is a regular file and it and its
// directory are owned by this process's user (or root) and not writable by
// group or others.
func checkOwnedBySelf(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	me := uint32(os.Getuid()) //nolint:gosec // a uid fits
	for _, p := range []string{path, filepath.Dir(path)} {
		fi, err := os.Stat(p)
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("cannot read the owner of %s", p)
		}
		if st.Uid != me && st.Uid != 0 {
			return fmt.Errorf("%s must be owned by the runner's user", p)
		}
		if fi.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("%s must not be writable by group or others", p)
		}
	}
	return nil
}

func checkNotWorldReadable(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o004 != 0 {
		return fmt.Errorf("%s must not be readable by others", path)
	}
	return nil
}
