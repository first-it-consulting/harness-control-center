// Package egress is the only way out of a sandbox: an HTTP CONNECT proxy,
// one per sandbox, that lets it reach exactly the host names and ports its
// harness allows (policy.Engine.CheckEgress) and never an internal address.
//
// The proxy resolves names itself (the guest has no DNS) and connects to
// the very address it checked, so a name cannot be re-resolved to
// something else afterwards (DNS rebinding). Every decision is logged.
package egress

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/policy"
)

// Defaults.
const (
	DefaultMaxConns   = 64
	headerTimeout     = 10 * time.Second
	dialTimeout       = 10 * time.Second
	maxHeaderBytes    = 16 << 10
	connectedResponse = "HTTP/1.1 200 Connection established\r\n\r\n"
)

// Proxy serves one sandbox.
type Proxy struct {
	Policy *policy.Engine
	// AllowPrivate permits private (RFC 1918, ULA, CGNAT) destinations,
	// as granted by the harness. Loopback, link-local (cloud metadata),
	// multicast, reserved and Blocked addresses are never reachable.
	AllowPrivate bool
	// Blocked are further networks that are never reachable, e.g. the
	// sandboxes' own subnet.
	Blocked []netip.Prefix
	JobID   string
	Log     *slog.Logger
	// MaxConns bounds concurrent tunnels (default DefaultMaxConns).
	MaxConns int
	// Resolve, Dial and LocalAddrs are replaceable for tests.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	Dial    func(ctx context.Context, addr netip.AddrPort) (net.Conn, error)
	// LocalAddrs lists the host's own addresses, which are never
	// reachable (the guest must not reach host services through the
	// proxy). It is called for every connection, so new interfaces count.
	LocalAddrs func() ([]netip.Addr, error)
}

// Serve accepts connections on l until ctx ends; then it closes l and
// every open tunnel and returns.
func (p *Proxy) Serve(ctx context.Context, l net.Listener) error {
	if p.Policy == nil {
		return errors.New("egress: no policy")
	}
	p.defaults()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { <-ctx.Done(); _ = l.Close() }()

	slots := make(chan struct{}, p.MaxConns)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			p.Log.Warn("egress: too many connections", "job", p.JobID)
			_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
			_, _ = io.WriteString(conn, "HTTP/1.1 429 Too Many Requests\r\nContent-Length: 0\r\n\r\n")
			_ = conn.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			p.handle(ctx, conn)
		}()
	}
}

func (p *Proxy) defaults() {
	if p.MaxConns <= 0 {
		p.MaxConns = DefaultMaxConns
	}
	if p.Log == nil {
		p.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if p.Resolve == nil {
		p.Resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	if p.LocalAddrs == nil {
		p.LocalAddrs = hostAddrs
	}
	if p.Dial == nil {
		p.Dial = func(ctx context.Context, addr netip.AddrPort) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr.String())
		}
	}
}

func (p *Proxy) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	start := time.Now()
	_ = conn.SetReadDeadline(start.Add(headerTimeout))
	limited := &io.LimitedReader{R: conn, N: maxHeaderBytes}
	br := bufio.NewReader(limited)
	req, err := http.ReadRequest(br)
	if err != nil {
		p.refuse(conn, http.StatusBadRequest, "malformed or oversized request", "", 0, err)
		return
	}
	if req.Method != http.MethodConnect {
		p.refuse(conn, http.StatusMethodNotAllowed, "only HTTPS through CONNECT is supported", req.Host, 0, nil)
		return
	}
	host, portStr, err := net.SplitHostPort(req.Host)
	port, perr := strconv.Atoi(portStr)
	if err != nil || perr != nil || port < 1 || port > 65535 {
		p.refuse(conn, http.StatusBadRequest, "CONNECT target must be host:port", req.Host, 0, nil)
		return
	}
	if d := p.Policy.CheckEgress(host, port); !d.Allowed {
		p.refuse(conn, http.StatusForbidden, d.Reason, host, port, nil)
		return
	}

	addrs, err := p.Resolve(ctx, host)
	if err != nil {
		p.refuse(conn, http.StatusBadGateway, "cannot resolve host", host, port, err)
		return
	}
	local, err := p.LocalAddrs()
	if err != nil { // fail closed: without the host's addresses, nothing is safe
		p.refuse(conn, http.StatusBadGateway, "cannot determine the host's own addresses", host, port, err)
		return
	}
	var target netip.Addr
	for _, a := range addrs {
		if a = a.Unmap(); !p.blocked(a) && !slices.Contains(local, a) {
			target = a
			break
		}
	}
	if !target.IsValid() {
		p.refuse(conn, http.StatusForbidden, "host resolves only to internal or blocked addresses", host, port, nil)
		return
	}
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	upstream, err := p.Dial(dctx, netip.AddrPortFrom(target, uint16(port)))
	cancel()
	if err != nil {
		p.refuse(conn, http.StatusBadGateway, "cannot connect", host, port, err)
		return
	}
	defer upstream.Close()

	_ = conn.SetReadDeadline(time.Time{})
	if _, err := io.WriteString(conn, connectedResponse); err != nil {
		return
	}
	// Bytes the client sent right after the request (e.g. a TLS
	// ClientHello) are already buffered; forward them first.
	buffered, _ := br.Peek(br.Buffered())
	clientSide := io.MultiReader(bytes.NewReader(bytes.Clone(buffered)), conn)

	// End the tunnel when the job ends.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close(); _ = upstream.Close() })
	defer stop()
	var up, down int64
	done := make(chan struct{})
	go func() {
		up, _ = io.Copy(upstream, clientSide)
		if tc, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = tc.CloseWrite()
		}
		close(done)
	}()
	down, _ = io.Copy(conn, upstream)
	_ = conn.Close()
	<-done
	p.Log.Info("egress: tunnel closed", "job", p.JobID, "host", host, "port", port, "ip", target.String(),
		"bytesUp", up, "bytesDown", down, "duration", time.Since(start).Round(time.Millisecond).String())
}

func (p *Proxy) refuse(conn net.Conn, code int, reason, host string, port int, err error) {
	attrs := []any{"job", p.JobID, "host", host, "port", port, "status", code, "reason", reason}
	if err != nil {
		attrs = append(attrs, "err", err.Error())
	}
	p.Log.Warn("egress: refused", attrs...)
	body := reason + "\n"
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		code, http.StatusText(code), len(body), body)
}

// alwaysBlocked are never reachable, whatever the harness grants.
var alwaysBlocked = mustPrefixes(
	"0.0.0.0/8",       // "this network"
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // documentation
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // documentation
	"203.0.113.0/24",  // documentation
	"240.0.0.0/4",     // reserved, incl. broadcast
	"64:ff9b::/96",    // NAT64: could map to internal IPv4
	"64:ff9b:1::/48",  // local-use NAT64
	"2001:db8::/32",   // documentation
	"fec0::/10",       // deprecated site-local
)

// privateRanges are reachable only with AllowPrivate (besides the
// netip.Addr.IsPrivate ranges).
var privateRanges = mustPrefixes("100.64.0.0/10") // carrier-grade NAT

func (p *Proxy) blocked(a netip.Addr) bool {
	if !a.IsValid() || a.IsUnspecified() || a.IsLoopback() || a.IsLinkLocalUnicast() ||
		a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() || a.IsMulticast() {
		return true
	}
	for _, set := range [][]netip.Prefix{alwaysBlocked, p.Blocked} {
		for _, n := range set {
			if n.Contains(a) {
				return true
			}
		}
	}
	if !p.AllowPrivate {
		if a.IsPrivate() {
			return true
		}
		for _, n := range privateRanges {
			if n.Contains(a) {
				return true
			}
		}
	}
	return false
}

// hostAddrs returns every address configured on the host.
func hostAddrs() ([]netip.Addr, error) {
	ifAddrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, len(ifAddrs))
	for _, a := range ifAddrs {
		if pfx, err := netip.ParsePrefix(a.String()); err == nil {
			out = append(out, pfx.Addr().Unmap())
		}
	}
	return out, nil
}

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}
