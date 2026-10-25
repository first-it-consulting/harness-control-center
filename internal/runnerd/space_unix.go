//go:build unix

package runnerd

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// freeBytes is the space on path's filesystem available to unprivileged
// users.
func freeBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	bsize := toInt64(st.Bsize)
	if bsize <= 0 {
		return 0, fmt.Errorf("%s: invalid block size %d", path, bsize)
	}
	return st.Bavail * uint64(bsize), nil
}

// toInt64 converts Statfs_t.Bsize, which is int64 on Linux and uint32 on
// macOS.
func toInt64[T int64 | uint32](v T) int64 { return int64(v) }
