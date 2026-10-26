package main

import (
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// dialVsock connects to a vsock port. The net package has no vsock
// support (net.FileConn refuses the address family), so the connection
// is a socket file of our own.
func dialVsock(cid, port uint32) (net.Conn, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock: %w", err)
	}
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: cid, Port: port}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("vsock: connect %d:%d: %w", cid, port, err)
	}
	// Non-blocking, so the runtime's poller serves reads and writes.
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("vsock: %w", err)
	}
	return &vsockConn{f: os.NewFile(uintptr(fd), "vsock"), port: port}, nil
}

type vsockConn struct {
	f    *os.File
	port uint32
}

func (c *vsockConn) Read(b []byte) (int, error)  { return c.f.Read(b) }
func (c *vsockConn) Write(b []byte) (int, error) { return c.f.Write(b) }
func (c *vsockConn) Close() error                { return c.f.Close() }

// CloseWrite ends the sending half, so the host sees the end of the
// request and can still answer.
func (c *vsockConn) CloseWrite() error {
	rc, err := c.f.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) { serr = unix.Shutdown(int(fd), unix.SHUT_WR) }); err != nil {
		return err
	}
	return serr
}

func (c *vsockConn) LocalAddr() net.Addr                { return vsockAddr{cid: unix.VMADDR_CID_ANY} }
func (c *vsockConn) RemoteAddr() net.Addr               { return vsockAddr{cid: hostCID, port: c.port} }
func (c *vsockConn) SetDeadline(t time.Time) error      { return c.f.SetDeadline(t) }
func (c *vsockConn) SetReadDeadline(t time.Time) error  { return c.f.SetReadDeadline(t) }
func (c *vsockConn) SetWriteDeadline(t time.Time) error { return c.f.SetWriteDeadline(t) }

type vsockAddr struct{ cid, port uint32 }

func (vsockAddr) Network() string  { return "vsock" }
func (a vsockAddr) String() string { return fmt.Sprintf("%d:%d", a.cid, a.port) }
