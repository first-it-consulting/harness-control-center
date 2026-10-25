package egress

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/credentials"
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
