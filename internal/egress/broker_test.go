package egress

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/credentials"
	"github.com/first-it-consulting/harness-control-center/internal/policy"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

const brokerSecret = "sk-live-do-not-leak-4242" //nolint:gosec // fake secret; tests check it never leaks

const brokerHarness = `
apiVersion: hcc/v1alpha1
kind: Harness
metadata: {name: b}
spec:
  runtime: claude-code
  prompt: p
  permissions:
    credentials: [api]
    network:
      egress:
        - host: example.com
        - host: plain.example
`

type lockedLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type brokerEnv struct {
	proxy    string
	ca       *CA
	upstream *httptest.Server
	log      *lockedLog
	seen     chan *http.Request
}

// startBroker runs a proxy brokering the "api" credential for
// example.com, with an upstream TLS server standing in for the API
// (httptest's certificate is valid for example.com).
func startBroker(t *testing.T, handler http.HandlerFunc, trustUpstream bool) *brokerEnv {
	t.Helper()
	env := &brokerEnv{log: &lockedLog{}, seen: make(chan *http.Request, 16)}
	env.upstream = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.seen <- r.Clone(context.Background())
		if handler != nil {
			handler(w, r)
			return
		}
		fmt.Fprint(w, "upstream ok")
	}))
	env.upstream.StartTLS()
	t.Cleanup(env.upstream.Close)

	h, err := spec.Parse([]byte(brokerHarness))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := policy.New(h)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := NewCA([]string{"example.com"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	env.ca = ca
	roots := x509.NewCertPool()
	if trustUpstream {
		roots.AddCert(env.upstream.Certificate())
	}
	upstreamAddr := env.upstream.Listener.Addr().String()
	p := &Proxy{
		Policy: engine,
		JobID:  "job-1",
		Log:    slog.New(slog.NewTextHandler(env.log, nil)),
		Credentials: map[string]credentials.Credential{
			"example.com": {Name: "api", Hosts: []string{"example.com"}, Scheme: "https", Port: 443, Header: "x-api-key", Value: brokerSecret},
		},
		CA:            ca,
		UpstreamRoots: roots,
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		},
		Dial: func(ctx context.Context, _ netip.AddrPort) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", upstreamAddr) // the "internet"
		},
		LocalAddrs: func() ([]netip.Addr, error) { return nil, nil },
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Serve(ctx, l); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	env.proxy = l.Addr().String()
	return env
}

// guest opens a CONNECT tunnel and a TLS session trusting only the job CA.
func (env *brokerEnv) guest(t *testing.T, target, sni string) (*tls.Conn, error) {
	t.Helper()
	c, err := net.Dial("tcp", env.proxy)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT %s: %d", target, resp.StatusCode)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(env.ca.PEM())
	tc := tls.Client(&prefixConn{Conn: c, prefix: drain(br)}, &tls.Config{RootCAs: pool, ServerName: sni, MinVersion: tls.VersionTLS12})
	return tc, tc.Handshake()
}

func drain(br *bufio.Reader) []byte {
	b, _ := br.Peek(br.Buffered())
	return b
}

func get(t *testing.T, tc *tls.Conn, req string) *http.Response {
	t.Helper()
	if _, err := io.WriteString(tc, req); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestBrokerInjectsCredential(t *testing.T) {
	env := startBroker(t, nil, true)
	tc, err := env.guest(t, "example.com:443", "example.com")
	if err != nil {
		t.Fatalf("handshake with the job CA: %v", err)
	}
	defer tc.Close()
	// The guest tries to set its own key and to redirect the request.
	resp := get(t, tc, "GET /v1/messages?x=1 HTTP/1.1\r\nHost: evil.example\r\nx-api-key: guessed\r\nProxy-Authorization: x\r\nConnection: close\r\n\r\n")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "upstream ok" {
		t.Fatalf("response %d %q", resp.StatusCode, body)
	}
	r := <-env.seen
	if got := r.Header.Values("X-Api-Key"); len(got) != 1 || got[0] != brokerSecret {
		t.Errorf("upstream saw x-api-key %q, want only the brokered secret", got)
	}
	if r.Host != "example.com" {
		t.Errorf("upstream saw Host %q, want example.com", r.Host)
	}
	if r.Header.Get("Proxy-Authorization") != "" {
		t.Error("Proxy-Authorization forwarded")
	}
	if r.URL.Path != "/v1/messages" || r.URL.RawQuery != "x=1" {
		t.Errorf("upstream path %q?%q", r.URL.Path, r.URL.RawQuery)
	}
	if strings.Contains(env.log.String(), brokerSecret) {
		t.Error("the secret appears in the log")
	}
	if !strings.Contains(env.log.String(), "brokered request") {
		t.Errorf("request not logged:\n%s", env.log.String())
	}
}

func TestBrokerStreams(t *testing.T) {
	release := make(chan struct{})
	env := startBroker(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, "data: second\n\n")
	}, true)
	tc, err := env.guest(t, "example.com:443", "example.com")
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	resp := get(t, tc, "GET /stream HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n")
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("first event %q, %v (not streamed?)", line, err)
	}
	close(release)
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "data: second") {
		t.Errorf("second event missing: %q", rest)
	}
}

func TestBrokerRefusesWrongSNI(t *testing.T) {
	env := startBroker(t, nil, true)
	if _, err := env.guest(t, "example.com:443", "other.example"); err == nil {
		t.Error("TLS for another name succeeded through the broker")
	}
}

func TestBrokerRefusesUntrustedUpstream(t *testing.T) {
	env := startBroker(t, nil, false) // upstream certificate not trusted
	tc, err := env.guest(t, "example.com:443", "example.com")
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	resp := get(t, tc, "GET / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status %d, want 502 for an unverifiable upstream", resp.StatusCode)
	}
	select {
	case <-env.seen:
		t.Error("the secret was sent to an unverified server")
	default:
	}
}

func TestOtherHostsAreNotBrokered(t *testing.T) {
	env := startBroker(t, nil, true)
	// plain.example has no credential: a tunnel straight to "the internet"
	// (our upstream), so TLS must fail against the job CA.
	if _, err := env.guest(t, "plain.example:443", "plain.example"); err == nil {
		t.Error("a host without credential was intercepted")
	}
}

func TestServesCAOnly(t *testing.T) {
	env := startBroker(t, nil, true)
	c, err := net.Dial("tcp", env.proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprint(c, "GET /hcc/ca.pem HTTP/1.1\r\nHost: proxy\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != string(env.ca.PEM()) {
		t.Errorf("CA endpoint: %d %q", resp.StatusCode, body)
	}
}

// Even a certificate signed with the job CA's own key is useless for any
// other name: the name constraints forbid it.
func TestCANameConstraints(t *testing.T) {
	ca, err := NewCA([]string{"example.com"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.Leaf("evil.example"); err == nil {
		t.Error("Leaf issued a certificate for a host without credential")
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	sign := func(name string, ip net.IP) *x509.Certificate {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: name},
			NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		if name != "" {
			tmpl.DNSNames = []string{name}
		}
		if ip != nil {
			tmpl.IPAddresses = []net.IP{ip}
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := x509.ParseCertificate(der)
		return c
	}
	verify := func(c *x509.Certificate, name string) error {
		_, err := c.Verify(x509.VerifyOptions{Roots: pool, DNSName: name})
		return err
	}
	if err := verify(sign("example.com", nil), "example.com"); err != nil {
		t.Errorf("permitted name rejected: %v", err)
	}
	if verify(sign("bank.example", nil), "bank.example") == nil {
		t.Error("CA could vouch for bank.example")
	}
	if verify(sign("", net.ParseIP("10.0.0.1")), "10.0.0.1") == nil {
		t.Error("CA could vouch for an IP address")
	}
}
