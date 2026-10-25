//go:build unix

package credentials

import (
	"fmt"
	"os"
)

// checkPrivate requires a regular file that others can neither read nor
// write, and that its group (the runner's) cannot write.
func checkPrivate(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file (not a symlink)", path)
	}
	if fi.Mode().Perm()&0o027 != 0 {
		return fmt.Errorf("%s must not be writable by its group or accessible by others (mode %v)", path, fi.Mode().Perm())
	}
	return nil
}
