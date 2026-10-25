package egress

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/credentials"
	"github.com/first-it-consulting/harness-control-center/internal/policy"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

const httpHarness = `
apiVersion: hcc/v1alpha1
kind: Harness
metadata: {name: h}
spec:
  runtime: claude-code
  prompt: p
  permissions:
    credentials: [llm]
    network:
      egress:
        - host: llm.example
          ports: [4000]
        - host: other.example
          ports: [80]
`

type seenRequest struct {
	host, path, key string
	body            int
}

func startHTTPBroker(t *testing.T) (proxy string, seen chan seenRequest, logs *lockedLog) {
	t.Helper()
	seen = make(chan seenRequest, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen <- seenRequest{host: r.Host, path: r.URL.Path, key: strings.Join(r.Header.Values("X-Api-Key"), ","), body: len(b)}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: hello\n\n")
	}))
	t.Cleanup(upstream.Close)
	h, err := spec.Parse([]byte(httpHarness))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := policy.New(h)
	if err != nil {
		t.Fatal(err)
	}
	logs = &lockedLog{}
	p := &Proxy{
		Policy: engine, JobID: "job-1", Log: slog.New(slog.NewTextHandler(logs, nil)),
		Credentials: map[string]credentials.Credential{
			"llm.example": {Name: "llm", Hosts: []string{"llm.example"}, Scheme: "http", Port: 4000, Header: "x-api-key", Value: brokerSecret},
		},
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		},
		Dial: func(ctx context.Context, _ netip.AddrPort) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", upstream.Listener.Addr().String())
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
	return l.Addr().String(), seen, logs
}

func send(t *testing.T, c net.Conn, br *bufio.Reader, raw string) *http.Response {
	t.Helper()
	if _, err := io.WriteString(c, raw); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHTTPBrokerForwardsWithCredential(t *testing.T) {
	proxy, seen, logs := startHTTPBroker(t)
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)

	// A body far beyond the 16 KiB header limit, as a long prompt would be.
	body := strings.Repeat("x", 200<<10)
	resp := send(t, c, br, fmt.Sprintf("POST http://llm.example:4000/v1/messages HTTP/1.1\r\nHost: evil.example\r\nX-Api-Key: guessed\r\nContent-Length: %d\r\n\r\n%s", len(body), body))
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(got) != "data: hello\n\n" {
		t.Fatalf("response %d %q", resp.StatusCode, got)
	}
	r := <-seen
	if r.key != brokerSecret || r.host != "llm.example:4000" || r.path != "/v1/messages" || r.body != len(body) {
		t.Errorf("upstream saw %+v", r)
	}

	// Keep-alive: a second request to the same host works ...
	resp = send(t, c, br, "GET http://llm.example:4000/v1/models HTTP/1.1\r\nHost: llm.example:4000\r\n\r\n")
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || (<-seen).key != brokerSecret {
		t.Errorf("second request: %d", resp.StatusCode)
	}
	// ... another host on the same connection does not.
	resp = send(t, c, br, "GET http://other.example/ HTTP/1.1\r\nHost: other.example\r\n\r\n")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("switching hosts on a connection: %d", resp.StatusCode)
	}
	if strings.Contains(logs.String(), brokerSecret) {
		t.Error("the secret appears in the log")
	}
}

func TestHTTPBrokerRefusesEverythingElse(t *testing.T) {
	proxy, seen, _ := startHTTPBroker(t)
	for name, raw := range map[string]string{
		"unbrokered host":             "GET http://other.example/ HTTP/1.1\r\nHost: other.example\r\n\r\n",
		"brokered host, wrong port":   "GET http://llm.example:8080/ HTTP/1.1\r\nHost: llm.example:8080\r\n\r\n",
		"brokered host, default port": "GET http://llm.example/ HTTP/1.1\r\nHost: llm.example\r\n\r\n",
		"origin-form request":         "GET /v1/messages HTTP/1.1\r\nHost: llm.example:4000\r\n\r\n",
		"ip literal":                  "GET http://93.184.216.34:4000/ HTTP/1.1\r\nHost: 93.184.216.34:4000\r\n\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := net.Dial("tcp", proxy)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			resp := send(t, c, bufio.NewReader(c), raw)
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				t.Error("forwarded")
			}
		})
	}
	select {
	case r := <-seen:
		t.Errorf("a refused request reached upstream: %+v", r)
	default:
	}
}

// An https credential never applies to plain HTTP, and vice versa.
func TestSchemesDoNotMix(t *testing.T) {
	proxy, seen, _ := startHTTPBroker(t)
	code, c, _ := connect(t, proxy, "llm.example:4000") // CONNECT to the http credential host
	_ = c.Close()
	if code != http.StatusOK {
		t.Fatalf("CONNECT: %d", code)
	}
	// It is a plain tunnel (no brokering), so upstream sees no credential.
	select {
	case r := <-seen:
		t.Errorf("brokered over CONNECT: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
}
