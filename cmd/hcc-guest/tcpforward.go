package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// tcp-forward: programs that cannot use an HTTP proxy (psql, ssh, git
// over ssh, database drivers) reach the harness's allowed hosts anyway.
// The guest's init maps each allowed host to a loopback address of its
// own in /etc/hosts; this listens there on the host's allowed ports and
// carries each connection through the job's egress proxy with CONNECT
// host:port. It only moves bytes: the proxy decides, by name and by
// resolved address, exactly as for every other connection.

// forwardConfig is tcp-forward's configuration file, written by init.
type forwardConfig struct {
	// Proxy is the egress proxy's address (HCC_PROXY).
	Proxy  string         `json:"proxy"`
	Routes []forwardRoute `json:"routes"`
}

type forwardRoute struct {
	// Listen is a loopback address and port; Target the host and port
	// asked of the proxy.
	Listen string `json:"listen"`
	Target string `json:"target"`
}

// Bounds.
const (
	maxForwardRoutes = 256
	connectTimeout   = 30 * time.Second
)

// forwardHostRe is a host name a route may name (the proxy checks it
// again against the harness).
var forwardHostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

func cmdTCPForward(args []string, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: hcc-guest tcp-forward CONFIG.json")
		return 2
	}
	cfg, err := loadForwardConfig(args[0])
	if err != nil {
		fmt.Fprintln(stderr, "hcc-guest tcp-forward:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serveForwards(ctx, cfg, stderr); err != nil {
		fmt.Fprintln(stderr, "hcc-guest tcp-forward:", err)
		return 1
	}
	return 0
}

func loadForwardConfig(path string) (*forwardConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // init's own file
	if err != nil {
		return nil, err
	}
	var cfg forwardConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, cfg.check()
}

// check: the proxy is an address, every route listens on loopback and
// names a plain host and a port.
func (c *forwardConfig) check() error {
	if _, err := netip.ParseAddrPort(c.Proxy); err != nil {
		return fmt.Errorf("proxy %q is not an address and port", c.Proxy)
	}
	if len(c.Routes) == 0 || len(c.Routes) > maxForwardRoutes {
		return fmt.Errorf("1 to %d routes, got %d", maxForwardRoutes, len(c.Routes))
	}
	for _, r := range c.Routes {
		ap, err := netip.ParseAddrPort(r.Listen)
		if err != nil || !ap.Addr().Is4() || !ap.Addr().IsLoopback() || ap.Port() == 0 {
			return fmt.Errorf("route listens on %q, not a loopback address with a port", r.Listen)
		}
		host, port, err := net.SplitHostPort(r.Target)
		if n, perr := strconv.Atoi(port); err != nil || perr != nil || n < 1 || n > 65535 || !forwardHostRe.MatchString(host) {
			return fmt.Errorf("route target %q is not host:port", r.Target)
		}
	}
	return nil
}

// serveForwards listens on every route until ctx ends.
func serveForwards(ctx context.Context, cfg *forwardConfig, stderr io.Writer) error {
	var ls []net.Listener
	for _, r := range cfg.Routes {
		l, err := net.Listen("tcp4", r.Listen)
		if err != nil {
			for _, x := range ls {
				_ = x.Close()
			}
			return err
		}
		ls = append(ls, l)
	}
	var wg sync.WaitGroup
	for i, l := range ls {
		target := cfg.Routes[i].Target
		wg.Go(func() {
			err := forward(ctx, l, func() (net.Conn, error) { return dialThroughProxy(ctx, cfg.Proxy, target) })
			if err != nil {
				fmt.Fprintln(stderr, "hcc-guest tcp-forward:", target+":", err)
			}
		})
	}
	wg.Wait()
	return nil
}

// dialThroughProxy opens a tunnel to target through the proxy.
func dialThroughProxy(ctx context.Context, proxy, target string) (net.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	c, err := (&net.Dialer{}).DialContext(dctx, "tcp", proxy)
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(connectTimeout))
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		_ = c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = c.Close()
		return nil, errors.New("the proxy refused " + target + ": " + resp.Status)
	}
	_ = c.SetDeadline(time.Time{})
	return &bufferedConn{Conn: c, r: br}, nil
}

// bufferedConn reads what the proxy's reader buffered first; it can still
// be half-closed.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b *bufferedConn) CloseWrite() error {
	if cw, ok := b.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}
