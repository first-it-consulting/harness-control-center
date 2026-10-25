package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/credentials"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

// broker terminates TLS for one credentialed host and forwards each
// request with the credential's header set, to the address the proxy
// checked, over TLS verified against the system (or UpstreamRoots).
type broker struct {
	p      *Proxy
	host   string
	port   int
	target netip.Addr
	cred   credentials.Credential
}

func (b *broker) serve(ctx context.Context, conn net.Conn) {
	leaf, err := b.p.CA.Leaf(b.host)
	if err != nil {
		b.p.Log.Error("egress: broker certificate", "job", b.p.JobID, "host", b.host, "err", err)
		return
	}
	tconn := tls.Server(conn, &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			// The guest must ask for the host it CONNECTed to.
			if hello.ServerName != "" && hello.ServerName != b.host {
				return nil, fmt.Errorf("SNI %q does not match %s", hello.ServerName, b.host)
			}
			return leaf, nil
		},
	})
	_ = tconn.SetDeadline(time.Now().Add(headerTimeout))
	if err := tconn.HandshakeContext(ctx); err != nil {
		b.p.Log.Warn("egress: broker handshake failed", "job", b.p.JobID, "host", b.host, "err", err)
		return
	}
	_ = tconn.SetDeadline(time.Time{})

	transport := b.transport()
	defer transport.CloseIdleConnections()
	authority := b.host
	if b.port != 443 {
		authority = net.JoinHostPort(b.host, strconv.Itoa(b.port))
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Whatever the guest says, the request goes to this host.
			pr.Out.URL.Scheme = "https"
			pr.Out.URL.Host = authority
			pr.Out.Host = authority
			// The guest cannot set, override or probe the credential.
			pr.Out.Header.Del(b.cred.Header)
			pr.Out.Header.Del("Proxy-Authorization")
			pr.Out.Header.Set(b.cred.Header, b.cred.Value)
		},
		Transport:     transport,
		FlushInterval: -1, // stream (e.g. server-sent events) as it arrives
		ErrorLog:      log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			b.p.Log.Warn("egress: broker upstream error", "job", b.p.JobID, "host", b.host, "path", r.URL.Path, "err", err)
			w.WriteHeader(http.StatusBadGateway)
		},
		ModifyResponse: func(resp *http.Response) error {
			b.p.Log.Info("egress: brokered request", "job", b.p.JobID, "credential", b.cred.Name,
				"host", b.host, "method", resp.Request.Method, "path", resp.Request.URL.Path, "status", resp.StatusCode)
			return nil
		},
	}

	srv := &http.Server{
		Handler:           rp,
		ReadHeaderTimeout: headerTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	l := newOneConnListener(tconn)
	srv.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateClosed || s == http.StateHijacked {
			l.done()
		}
	}
	stop := context.AfterFunc(ctx, func() { _ = srv.Close() })
	defer stop()
	_ = srv.Serve(l)
	<-l.closed
	_ = srv.Close()
}

func (b *broker) transport() *http.Transport {
	return &http.Transport{
		DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			// Always the checked address, whatever the request said.
			raw, err := b.p.Dial(ctx, netip.AddrPortFrom(b.target, uint16(b.port))) //nolint:gosec // port is within 1..65535
			if err != nil {
				return nil, err
			}
			tc := tls.Client(raw, &tls.Config{
				MinVersion: tls.VersionTLS12,
				ServerName: b.host,
				RootCAs:    b.p.UpstreamRoots, // nil: the system roots
				NextProtos: []string{"http/1.1"},
			})
			if err := tc.HandshakeContext(ctx); err != nil {
				_ = raw.Close()
				return nil, err
			}
			return tc, nil
		},
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{}, // HTTP/1.1 only
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     90 * time.Second,
	}
}

// oneConnListener hands out a single connection, then reports closed
// once that connection is done.
type oneConnListener struct {
	conn   net.Conn
	once   sync.Once
	closed chan struct{}
	ch     chan net.Conn
}

func newOneConnListener(c net.Conn) *oneConnListener {
	l := &oneConnListener{conn: c, closed: make(chan struct{}), ch: make(chan net.Conn, 1)}
	l.ch <- c
	return l
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, errors.New("listener closed")
	}
}

func (l *oneConnListener) done()          { l.once.Do(func() { close(l.closed) }) }
func (l *oneConnListener) Close() error   { l.done(); return nil }
func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

// prefixConn replays bytes the proxy already read (e.g. the start of a
// TLS ClientHello sent together with CONNECT) before reading the conn.
type prefixConn struct {
	net.Conn
	prefix []byte
}

func (c *prefixConn) Read(p []byte) (int, error) {
	if len(c.prefix) > 0 {
		n := copy(p, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}

// maxRequestBody caps forwarded plain-HTTP request bodies.
const maxRequestBody = 64 << 20

// hopHeaders are connection-specific and never forwarded.
var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Upgrade"}

// forwardHTTP brokers plain-HTTP requests for a host whose credential the
// operator configured with scheme http (the secret crosses the network
// unencrypted; see ADR-0005). Only that host and port are served on this
// connection; everything else plain-HTTP is refused.
func (p *Proxy) forwardHTTP(ctx context.Context, conn net.Conn, br *bufio.Reader, limited *io.LimitedReader, req *http.Request) {
	host := spec.NormalizeHost(req.URL.Hostname())
	port := 80
	if ps := req.URL.Port(); ps != "" {
		n, err := strconv.Atoi(ps)
		if err != nil || n < 1 || n > 65535 {
			p.refuse(conn, http.StatusBadRequest, "invalid port", host, 0, nil)
			return
		}
		port = n
	}
	cred, ok := p.Credentials[host]
	if !ok || cred.Scheme != "http" || cred.Port != port {
		p.refuse(conn, http.StatusMethodNotAllowed, "only HTTPS through CONNECT is supported", host, port, nil)
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
	authority := net.JoinHostPort(host, strconv.Itoa(port))
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return p.Dial(ctx, netip.AddrPortFrom(target, uint16(port))) //nolint:gosec // port is within 1..65535
		},
		DisableCompression:  true, // pass encodings through untouched
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     90 * time.Second,
	}
	defer transport.CloseIdleConnections()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	for {
		_ = conn.SetReadDeadline(time.Time{})
		out := req.Clone(ctx)
		out.RequestURI = ""
		out.URL = &url.URL{Scheme: "http", Host: authority, Path: req.URL.Path, RawPath: req.URL.RawPath, RawQuery: req.URL.RawQuery}
		out.Host = authority
		out.Body = http.MaxBytesReader(nil, req.Body, maxRequestBody)
		for _, h := range hopHeaders {
			out.Header.Del(h)
		}
		out.Header.Del(cred.Header)
		out.Header.Set(cred.Header, cred.Value)

		resp, err := transport.RoundTrip(out)
		if err != nil {
			p.refuse(conn, http.StatusBadGateway, "upstream request failed", host, port, err)
			return
		}
		for _, h := range hopHeaders {
			resp.Header.Del(h)
		}
		p.Log.Info("egress: brokered request", "job", p.JobID, "credential", cred.Name, "host", host, "port", port,
			"scheme", "http", "method", req.Method, "path", req.URL.Path, "status", resp.StatusCode)
		werr := resp.Write(conn)
		_ = resp.Body.Close()
		if werr != nil || resp.Close || req.Close {
			return
		}

		// The next request on this connection: same limits, same host.
		limited.N = maxHeaderBytes
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
		next, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		limited.N = math.MaxInt64
		if !next.URL.IsAbs() || next.URL.Scheme != "http" || spec.NormalizeHost(next.URL.Hostname()) != host || next.URL.Port() != req.URL.Port() {
			p.refuse(conn, http.StatusForbidden, "one host per connection", next.URL.Host, 0, nil)
			return
		}
		req = next
	}
}
