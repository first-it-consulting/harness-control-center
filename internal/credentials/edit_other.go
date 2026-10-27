//go:build !unix

package credentials

import "errors"

var errNoEdit = errors.New("credentials: the store is edited where the runner runs (Linux or macOS)")

// Set is not available here (see edit.go).
func Set(string, string, Entry, []byte) error { return errNoEdit }

// Remove is not available here (see edit.go).
func Remove(string, string, string) error { return errNoEdit }
