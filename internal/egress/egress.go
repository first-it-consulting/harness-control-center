// Package egress is the only way out of a sandbox: an HTTP CONNECT proxy,
// one per sandbox, that lets it reach exactly the host names and ports its
// harness allows (policy.Engine.CheckEgress) and never an internal address.
//
// The proxy resolves names itself (the guest has no DNS) and connects to
// the very address it checked, so a name cannot be re-resolved to
// something else afterwards (DNS rebinding). Every decision is logged.
//
// It is also the credential broker: for hosts with a granted credential
// (port 443) it terminates TLS with the job's CA and adds the secret to
// each request, so the guest never holds it. All other hosts are plain
// tunnels the proxy cannot read.
package egress

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/budget"
	"github.com/first-it-consulting/harness-control-center/internal/credentials"
	"github.com/first-it-consulting/harness-control-center/internal/policy"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
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
	// Credentials maps host names to the credential added to requests for
	// them; CA (required with Credentials) issues their certificates.
	Credentials map[string]credentials.Credential
	CA          *CA
	// UpstreamRoots verifies upstream servers when brokering (nil: the
	// system roots).
	UpstreamRoots *x509.CertPool
	// Meter, if set, limits and records the job's model usage: requests to
	// ModelHost:ModelPort (which must be brokered). Other hosts are never
	// metered: they report no token usage.
	Meter     *budget.Meter
	ModelHost string
	ModelPort int
	// Inspect brokers every CONNECT, not only credentialed hosts, so that
	// approval gates see every request (CA must cover all allowed hosts).
	// Anything but TLS-wrapped HTTP then fails: nothing goes unseen.
	Inspect bool
	// Gate decides on requests whose actions need approval
	// (Policy.RequiresApproval); without it they are refused.
	Gate Gate
	// Local, if set, serves the job's own API to the guest: plain HTTP
	// requests to the proxy itself under /hcc/v1/ (subagents, ADR-0011;
	// MCP servers run as plugins, ADR-0014).
	Local http.Handler
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
	// Also on early errors: a guest gets "connection refused", not a hang.
	defer l.Close()
	if p.Policy == nil {
		return errors.New("egress: no policy")
	}
	for _, c := range p.Credentials {
		if c.Scheme == "https" && p.CA == nil {
			return errors.New("egress: https credentials need a CA")
		}
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
	// The header limit must not cut off request bodies (forwarded plain
	// HTTP requests have their own body cap).
	limited.N = math.MaxInt64
	if req.Method != http.MethodConnect {
		if req.Method == http.MethodGet && req.URL.Path == caPath && !req.URL.IsAbs() {
			p.serveCA(conn) // 404 when the job has no CA: the guest's init asks anyway
			return
		}
		// To the proxy itself, also in proxy form (clients that send
		// every request to their proxy).
		if (!req.URL.IsAbs() || req.URL.Scheme == "http" && isSelf(req.URL.Host, conn.LocalAddr())) &&
			strings.HasPrefix(req.URL.Path, localPath) {
			p.serveLocal(conn, req)
			return
		}
		if req.URL.IsAbs() && req.URL.Scheme == "http" {
			p.forwardHTTP(ctx, conn, br, limited, req)
			return
		}
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

	target, ok := p.resolveChecked(ctx, conn, host, port)
	if !ok {
		return
	}
	buffered, _ := br.Peek(br.Buffered())
	cred, ok := p.Credentials[spec.NormalizeHost(host)]
	credentialed := ok && cred.Scheme == "https" && port == cred.Port
	if credentialed || (p.Inspect && p.CA != nil) {
		if !credentialed {
			cred = credentials.Credential{} // inspected only: nothing is added
		}
		_ = conn.SetReadDeadline(time.Time{})
		if _, err := io.WriteString(conn, connectedResponse); err != nil {
			return
		}
		b := &broker{p: p, host: spec.NormalizeHost(host), port: port, target: target, cred: cred}
		b.serve(ctx, &prefixConn{Conn: conn, prefix: bytes.Clone(buffered)})
		return
	}
	if p.Inspect { // fail closed: an uninspected tunnel would bypass the gates
		p.refuse(conn, http.StatusForbidden, "this job's connections must be inspected, but there is no CA", host, port, nil)
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

// resolveChecked resolves host and returns the first address the guest
// may reach; otherwise it refuses on conn.
func (p *Proxy) resolveChecked(ctx context.Context, conn net.Conn, host string, port int) (netip.Addr, bool) {
	addrs, err := p.Resolve(ctx, host)
	if err != nil {
		p.refuse(conn, http.StatusBadGateway, "cannot resolve host", host, port, err)
		return netip.Addr{}, false
	}
	local, err := p.LocalAddrs()
	if err != nil { // fail closed: without the host's addresses, nothing is safe
		p.refuse(conn, http.StatusBadGateway, "cannot determine the host's own addresses", host, port, err)
		return netip.Addr{}, false
	}
	for _, a := range addrs {
		if a = a.Unmap(); !p.blocked(a) && !slices.Contains(local, a) {
			return a, true
		}
	}
	p.refuse(conn, http.StatusForbidden, "host resolves only to internal or blocked addresses", host, port, nil)
	return netip.Addr{}, false
}

// caPath is where a guest fetches the job CA to trust (plain HTTP from
// the proxy itself; the certificate is public).
const caPath = "/hcc/ca.pem"

// localPath is the root of the job's own API (plain HTTP to the proxy
// itself).
const localPath = "/hcc/v1/"

// localAPITimeout bounds one exchange with the job's API, including
// downloading a subagent's results or a long MCP tool call.
const localAPITimeout = 10 * time.Minute

// isSelf reports whether hostport names the address the guest reached
// the proxy on.
func isSelf(hostport string, local net.Addr) bool {
	want, err := netip.ParseAddrPort(local.String())
	if err != nil {
		return false
	}
	got, err := netip.ParseAddrPort(hostport)
	return err == nil && got.Addr().Unmap() == want.Addr().Unmap() && got.Port() == want.Port()
}

// serveLocal answers one request to the job's API; the connection closes
// after it. The listener only accepts this sandbox's guest, so the
// request is the job's own.
func (p *Proxy) serveLocal(conn net.Conn, req *http.Request) {
	if p.Local == nil {
		p.refuse(conn, http.StatusNotFound, "this job has no API here", "", 0, nil)
		return
	}
	// A proxy-form request is answered as if sent directly.
	req.URL.Scheme, req.URL.Host = "", ""
	_ = conn.SetDeadline(time.Now().Add(localAPITimeout))
	w := &connResponse{conn: conn, header: http.Header{}}
	defer func() {
		// Nothing a guest sends may take the runner down.
		if v := recover(); v != nil {
			p.Log.Error("egress: job API failed", "job", p.JobID, "panic", fmt.Sprint(v))
			w.WriteHeader(http.StatusInternalServerError)
		}
	}()
	p.Local.ServeHTTP(w, req)
	w.finish()
	p.Log.Info("egress: job API", "job", p.JobID, "method", req.Method, "path", req.URL.Path, "status", w.code)
}

// connResponse writes one HTTP/1.1 response straight to a connection;
// its body ends when the connection closes.
type connResponse struct {
	conn   net.Conn
	header http.Header
	code   int
	err    error
}

func (w *connResponse) Header() http.Header { return w.header }

func (w *connResponse) WriteHeader(code int) {
	if w.code != 0 {
		return
	}
	w.code = code
	var b bytes.Buffer
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\n", code, http.StatusText(code))
	w.header.Del("Content-Length") // the body is close-delimited
	w.header.Set("Connection", "close")
	_ = w.header.Write(&b)
	b.WriteString("\r\n")
	_, w.err = w.conn.Write(b.Bytes())
}

func (w *connResponse) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.conn.Write(b)
	w.err = err
	return n, err
}

func (w *connResponse) finish() { w.WriteHeader(http.StatusOK) }

func (p *Proxy) serveCA(conn net.Conn) {
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if p.CA == nil {
		_, _ = io.WriteString(conn, "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	pemBytes := p.CA.PEM()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/x-pem-file\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(pemBytes), pemBytes)
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
