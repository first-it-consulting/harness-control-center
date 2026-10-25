//go:build !unix

package runnerd

import "errors"

func freeBytes(string) (uint64, error) {
	return 0, errors.New("the runner daemon is only supported on Linux")
}
