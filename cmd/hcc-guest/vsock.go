package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
)

// vsock-proxy (ADR-0016): in a vz sandbox the guest has no network
// device; its only channel is one vsock port to the host, where the
// job's egress proxy listens. This forwards a loopback TCP address (the
// proxy address the agent's tools are given) to that port. It only
// moves bytes; the proxy on the host decides everything.

// hostCID is the vsock address of the host.
const hostCID = 2

func cmdVsockProxy(args []string, stderr io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintln(stderr, "usage: hcc-guest vsock-proxy 127.0.0.1:PORT VSOCK-PORT")
		return 2
	}
	listen, port, err := vsockArgs(args[0], args[1])
	if err != nil {
		fmt.Fprintln(stderr, "hcc-guest vsock-proxy:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	l, err := net.Listen("tcp", listen.String())
	if err != nil {
		fmt.Fprintln(stderr, "hcc-guest vsock-proxy:", err)
		return 1
	}
	if err := forward(ctx, l, func() (net.Conn, error) { return dialVsock(hostCID, port) }); err != nil {
		fmt.Fprintln(stderr, "hcc-guest vsock-proxy:", err)
		return 1
	}
	return 0
}

// vsockArgs checks the listen address (loopback only: nothing outside the
// guest may use it) and the vsock port.
func vsockArgs(listen, port string) (netip.AddrPort, uint32, error) {
	ap, err := netip.ParseAddrPort(listen)
	if err != nil || !ap.Addr().IsLoopback() || ap.Port() == 0 {
		return netip.AddrPort{}, 0, fmt.Errorf("listen address must be a loopback address with a port, got %q", listen)
	}
	p, err := strconv.ParseUint(port, 10, 32)
	if err != nil || p == 0 {
		return netip.AddrPort{}, 0, fmt.Errorf("invalid vsock port %q", port)
	}
	return ap, uint32(p), nil
}

// forward accepts on l until ctx ends and joins each connection to a new
// one from dial.
func forward(ctx context.Context, l net.Listener, dial func() (net.Conn, error)) error {
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		wg.Go(func() { pipe(c, dial) })
	}
}

func pipe(c net.Conn, dial func() (net.Conn, error)) {
	defer func() { _ = c.Close() }()
	up, err := dial()
	if err != nil {
		return
	}
	defer func() { _ = up.Close() }()
	done := make(chan struct{}, 2)
	half := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		// Half-close only: the other direction may still be answering.
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go half(up, c)
	go half(c, up)
	<-done
	<-done
}
