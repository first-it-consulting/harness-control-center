//go:build !unix

package runnerd

import "errors"

func checkRootOwned(string) error {
	return errors.New("the runner daemon is only supported on Linux")
}

func checkNotWorldReadable(string) error {
	return errors.New("the runner daemon is only supported on Linux")
}
