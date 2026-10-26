//go:build !linux

package main

import (
	"errors"
	"net"
)

// dialVsock connects to a vsock port; only guests (Linux) have vsock.
func dialVsock(_, _ uint32) (net.Conn, error) {
	return nil, errors.New("vsock is only available in a Linux guest")
}
