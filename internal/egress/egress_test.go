package egress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/policy"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

const harness = `
apiVersion: hcc/v1alpha1
kind: Harness
metadata: {name: t}
spec:
  runtime: claude-code
  prompt: p
  permissions:
    network:
      egress:
        - host: allowed.example
        - host: "*.cdn.example"
          ports: [8443]
`

// world is a fake DNS and internet: names resolve as listed, and every
// upstream connection is an echo server that records its address.
type world struct {
	mu     sync.Mutex
	dns    map[string][]string
	dialed []string
}

func (w *world) resolve(_ context.Context, host string) ([]netip.Addr, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	ips, ok := w.dns[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	var out []netip.Addr
	for _, ip := range ips {
		out = append(out, netip.MustParseAddr(ip))
	}
	return out, nil
}

func (w *world) dial(_ context.Context, addr netip.AddrPort) (net.Conn, error) {
	w.mu.Lock()
	w.dialed = append(w.dialed, addr.String())
	w.mu.Unlock()
	client, server := net.Pipe()
	go func() { _, _ = io.Copy(server, server); _ = server.Close() }() // echo
	return client, nil
}

func (w *world) dialedAddrs() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.dialed...)
}

func startProxy(t *testing.T, mutate func(*Proxy)) (string, *world, context.CancelFunc) {
	t.Helper()
	h, err := spec.Parse([]byte(harness))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := policy.New(h)
	if err != nil {
		t.Fatal(err)
	}
	w := &world{dns: map[string][]string{
		"allowed.example":     {"93.184.216.34"},
		"a.cdn.example":       {"93.184.216.35"},
		"rebind.cdn.example":  {"127.0.0.1"},
		"meta.cdn.example":    {"169.254.169.254"},
		"lan.cdn.example":     {"10.10.20.10"},
		"mixed.cdn.example":   {"10.0.0.1", "93.184.216.36"},
		"vmnet.cdn.example":   {"172.31.0.1"},
		"v6.cdn.example":      {"::1", "2606:2800:220:1:248:1893:25c8:1946"},
		"mapped.cdn.example":  {"::ffff:127.0.0.1"},
		"host.cdn.example":    {"10.10.20.151"}, // the host's LAN address
		"pubhost.cdn.example": {"203.1.113.7"},  // the host's public address
	}}
	p := &Proxy{
		Policy:  engine,
		JobID:   "job-1",
		Blocked: []netip.Prefix{netip.MustParsePrefix("172.31.0.0/16")},
		Resolve: w.resolve,
		Dial:    w.dial,
		LocalAddrs: func() ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("10.10.20.151"), netip.MustParseAddr("203.1.113.7"), netip.MustParseAddr("172.31.0.1")}, nil
		},
	}
	if mutate != nil {
		mutate(p)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Serve(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after cancel")
		}
	})
	return l.Addr().String(), w, cancel
}

// connect sends a CONNECT and returns the status code and the open conn.
func connect(t *testing.T, proxy, target string) (int, net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("CONNECT %s: %v", target, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, c, br
}

func TestAllowedTunnelRelaysData(t *testing.T) {
	proxy, w, _ := startProxy(t, nil)
	code, c, br := connect(t, proxy, "allowed.example:443")
	defer c.Close()
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if _, err := io.WriteString(c, "ping\n"); err != nil {
		t.Fatal(err)
	}
	line, err := br.ReadString('\n')
	if err != nil || line != "ping\n" {
		t.Errorf("echo = %q, %v", line, err)
	}
	if got := w.dialedAddrs(); len(got) != 1 || got[0] != "93.184.216.34:443" {
		t.Errorf("dialed %v; must dial exactly the checked address", got)
	}
}

func TestDataSentWithConnectIsForwarded(t *testing.T) {
	proxy, _, _ := startProxy(t, nil)
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	// Request and first payload bytes in one write, as TLS clients do.
	fmt.Fprint(c, "CONNECT allowed.example:443 HTTP/1.1\r\nHost: allowed.example:443\r\n\r\nhello-tls\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", resp, err)
	}
	_ = resp.Body.Close()
	if line, err := br.ReadString('\n'); err != nil || line != "hello-tls\n" {
		t.Errorf("early bytes lost: %q, %v", line, err)
	}
}

func TestRefusals(t *testing.T) {
	proxy, w, _ := startProxy(t, nil)
	tests := map[string]struct {
		target string
		want   int
	}{
		"host not allowed":       {"evil.example:443", http.StatusForbidden},
		"port not allowed":       {"allowed.example:22", http.StatusForbidden},
		"wildcard on wrong port": {"a.cdn.example:443", http.StatusForbidden},
		"ip literal":             {"93.184.216.34:443", http.StatusForbidden},
		"ipv6 literal":           {"[2606:2800:220:1::1]:443", http.StatusForbidden},
		// Rejected by the HTTP parser already; the policy would deny it too.
		"suffix trick":              {"evil.com#.cdn.example:8443", http.StatusBadRequest},
		"dns rebinding to loopback": {"rebind.cdn.example:8443", http.StatusForbidden},
		"cloud metadata":            {"meta.cdn.example:8443", http.StatusForbidden},
		"private lan":               {"lan.cdn.example:8443", http.StatusForbidden},
		"own vm subnet":             {"vmnet.cdn.example:8443", http.StatusForbidden},
		"ipv4-mapped loopback":      {"mapped.cdn.example:8443", http.StatusForbidden},
		"unresolvable":              {"none.cdn.example:8443", http.StatusBadGateway},
		"bad port":                  {"allowed.example:99999", http.StatusBadRequest},
		"no port":                   {"allowed.example", http.StatusBadRequest},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			code, c, _ := connect(t, proxy, tt.target)
			_ = c.Close()
			if code != tt.want {
				t.Errorf("status %d, want %d", code, tt.want)
			}
		})
	}
	if got := w.dialedAddrs(); len(got) != 0 {
		t.Errorf("refused requests dialed %v", got)
	}
}

func TestSkipsBlockedAddressesInAnswer(t *testing.T) {
	proxy, w, _ := startProxy(t, nil)
	for _, target := range []string{"mixed.cdn.example:8443", "v6.cdn.example:8443"} {
		code, c, _ := connect(t, proxy, target)
		_ = c.Close()
		if code != http.StatusOK {
			t.Errorf("%s: status %d", target, code)
		}
	}
	want := []string{"93.184.216.36:8443", "[2606:2800:220:1:248:1893:25c8:1946]:8443"}
	if got := w.dialedAddrs(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("dialed %v, want %v (the public addresses only)", got, want)
	}
}

func TestAllowPrivate(t *testing.T) {
	proxy, _, _ := startProxy(t, func(p *Proxy) { p.AllowPrivate = true })
	cases := map[string]int{
		"lan.cdn.example:8443":    http.StatusOK,        // granted
		"rebind.cdn.example:8443": http.StatusForbidden, // loopback never
		"meta.cdn.example:8443":   http.StatusForbidden, // metadata never
		"vmnet.cdn.example:8443":  http.StatusForbidden, // own subnet never
	}
	for target, want := range cases {
		code, c, _ := connect(t, proxy, target)
		_ = c.Close()
		if code != want {
			t.Errorf("%s: status %d, want %d", target, code, want)
		}
	}
}

func TestOnlyConnect(t *testing.T) {
	proxy, _, _ := startProxy(t, nil)
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprint(c, "GET http://allowed.example/ HTTP/1.1\r\nHost: allowed.example\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET through proxy: %d", resp.StatusCode)
	}
}

func TestOversizedHeaderRefused(t *testing.T) {
	proxy, _, _ := startProxy(t, nil)
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	go func() {
		fmt.Fprintf(c, "CONNECT allowed.example:443 HTTP/1.1\r\nX-Pad: %s\r\n\r\n", strings.Repeat("a", 64<<10))
	}()
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("oversized header: %d", resp.StatusCode)
	}
}

func TestConnectionLimit(t *testing.T) {
	proxy, _, _ := startProxy(t, func(p *Proxy) { p.MaxConns = 2 })
	var open []net.Conn
	for range 2 {
		code, c, _ := connect(t, proxy, "allowed.example:443")
		if code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
		open = append(open, c)
	}
	defer func() {
		for _, c := range open {
			_ = c.Close()
		}
	}()
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("third connection: %d", resp.StatusCode)
	}
}

func TestJobEndClosesTunnels(t *testing.T) {
	proxy, _, cancel := startProxy(t, nil)
	code, c, br := connect(t, proxy, "allowed.example:443")
	defer c.Close()
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	cancel()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := br.ReadByte(); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("tunnel still open after the job ended: %v", err)
	}
}

// The guest must never reach the host's own services through the proxy,
// whatever the harness grants.
func TestHostAddressesNeverReachable(t *testing.T) {
	for _, allowPrivate := range []bool{false, true} {
		proxy, w, _ := startProxy(t, func(p *Proxy) { p.AllowPrivate = allowPrivate })
		for _, target := range []string{"host.cdn.example:8443", "pubhost.cdn.example:8443"} {
			code, c, _ := connect(t, proxy, target)
			_ = c.Close()
			if code != http.StatusForbidden {
				t.Errorf("allowPrivate=%v %s: status %d, want 403", allowPrivate, target, code)
			}
		}
		if got := w.dialedAddrs(); len(got) != 0 {
			t.Errorf("dialed the host itself: %v", got)
		}
	}
}

func TestLocalAddrsErrorFailsClosed(t *testing.T) {
	proxy, w, _ := startProxy(t, func(p *Proxy) {
		p.LocalAddrs = func() ([]netip.Addr, error) { return nil, errors.New("netlink unavailable") }
	})
	code, c, _ := connect(t, proxy, "allowed.example:443")
	_ = c.Close()
	if code != http.StatusBadGateway || len(w.dialedAddrs()) != 0 {
		t.Errorf("status %d, dialed %v; want refusal without dialing", code, w.dialedAddrs())
	}
}

func TestHostAddrsIncludesLoopback(t *testing.T) {
	addrs, err := hostAddrs()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(addrs, netip.MustParseAddr("127.0.0.1")) {
		t.Errorf("host addresses %v lack 127.0.0.1", addrs)
	}
}

func TestServeClosesListenerOnConfigError(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Proxy{}).Serve(context.Background(), l); err == nil {
		t.Fatal("Serve without a policy succeeded")
	}
	if c, err := net.Dial("tcp", l.Addr().String()); err == nil {
		_ = c.Close()
		t.Error("listener still accepts connections after Serve failed")
	}
}

func TestNoCAMeansQuiet404(t *testing.T) {
	proxy, _, _ := startProxy(t, nil) // no credentials, no CA
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprint(c, "GET /hcc/ca.pem HTTP/1.1\r\nHost: proxy\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 404", resp.StatusCode)
	}
}

func rawRequest(t *testing.T, proxy, req string) (int, string, http.Header) {
	t.Helper()
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprint(c, req)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

func TestSubagentAPIIsServedToTheGuest(t *testing.T) {
	var got []string
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+" "+string(b))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "999") // must not survive: the body is close-delimited
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"job-2"}`)
	})
	proxy, _, _ := startProxy(t, func(p *Proxy) { p.Local = api })
	code, body, h := rawRequest(t, proxy, "POST /hcc/v1/agents HTTP/1.1\r\nHost: proxy\r\nContent-Length: 13\r\n\r\n{\"prompt\":1}\n")
	if code != http.StatusCreated || body != `{"id":"job-2"}` || h.Get("Content-Length") != "" || h.Get("Content-Type") != "application/json" {
		t.Errorf("response = %d %q %v", code, body, h)
	}
	if len(got) != 1 || got[0] != "POST /hcc/v1/agents {\"prompt\":1}\n" {
		t.Errorf("handler saw %q", got)
	}
	// In proxy form, only to the proxy's own address.
	code, _, _ = rawRequest(t, proxy, "POST http://"+proxy+"/hcc/v1/agents HTTP/1.1\r\nHost: x\r\nContent-Length: 2\r\n\r\n{}")
	if code != http.StatusCreated || len(got) != 2 || got[1] != "POST /hcc/v1/agents {}" {
		t.Errorf("proxy form: %d, %q", code, got)
	}
	// Only under /hcc/v1/, and never for another host.
	for _, req := range []string{
		"GET /hcc/agents HTTP/1.1\r\nHost: proxy\r\n\r\n",
		"GET http://proxy/hcc/v1/agents HTTP/1.1\r\nHost: proxy\r\n\r\n",
		"GET http://10.9.9.9:3128/hcc/v1/agents HTTP/1.1\r\nHost: proxy\r\n\r\n",
	} {
		if code, _, _ := rawRequest(t, proxy, req); code < 400 {
			t.Errorf("%q = %d", req, code)
		}
	}
	if len(got) != 2 {
		t.Errorf("handler saw %q", got)
	}
}

func TestNoSubagentAPIWithoutSubagents(t *testing.T) {
	proxy, _, _ := startProxy(t, nil)
	if code, _, _ := rawRequest(t, proxy, "GET /hcc/v1/agents HTTP/1.1\r\nHost: proxy\r\n\r\n"); code != http.StatusNotFound {
		t.Errorf("status %d, want 404", code)
	}
}

func TestSubagentAPIPanicIsContained(t *testing.T) {
	proxy, _, _ := startProxy(t, func(p *Proxy) {
		p.Local = http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("bug") })
	})
	if code, _, _ := rawRequest(t, proxy, "GET /hcc/v1/agents HTTP/1.1\r\nHost: proxy\r\n\r\n"); code != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", code)
	}
	// The proxy still serves.
	if code, _, _ := rawRequest(t, proxy, "GET /hcc/ca.pem HTTP/1.1\r\nHost: proxy\r\n\r\n"); code != http.StatusNotFound {
		t.Errorf("after a panic: %d", code)
	}
}
