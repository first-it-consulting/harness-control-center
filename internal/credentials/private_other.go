//go:build !unix

package credentials

import "errors"

func checkPrivate(string) error {
	return errors.New("credentials are only supported on Linux")
}
