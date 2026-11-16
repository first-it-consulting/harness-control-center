//go:build !unix

package runnerd

import (
	"errors"
	"os"
)

func checkRootOwned(string) error {
	return errors.New("the runner daemon is only supported on Linux")
}

func checkOwnedBySelf(string) error {
	return errors.New("the runner daemon is only supported on Linux and macOS")
}

func checkNotWorldReadable(string) error {
	return errors.New("the runner daemon is only supported on Linux")
}

func sameOwner(*os.File, os.FileInfo) error { return nil }
