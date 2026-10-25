//go:build e2e

package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/credentials"
	"github.com/first-it-consulting/harness-control-center/internal/policy"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

// Against the real internet: real DNS, the system's root CAs, a real
// HTTPS server. httpbin.org echoes request headers, which shows the
// injected header arrived. (Hosts that echo headers would reveal a real
// secret to the agent; never use them as credential hosts.)
//
//	HCC_E2E_INTERNET=1 go test -tags e2e -run Internet ./internal/egress
func TestBrokerAgainstTheInternet(t *testing.T) {
	if os.Getenv("HCC_E2E_INTERNET") == "" {
		t.Skip("HCC_E2E_INTERNET not set")
	}
	const marker = "hcc-e2e-marker-7f3a"
	h, err := spec.Parse([]byte(`apiVersion: hcc/v1alpha1
kind: Harness
metadata: {name: internet}
spec:
  runtime: claude-code
  prompt: p
  permissions:
    credentials: [echo]
    network: {egress: [{host: httpbin.org}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	engine, _ := policy.New(h)
	ca, err := NewCA([]string{"httpbin.org"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var logs lockedLog
	p := &Proxy{
		Policy: engine, JobID: "internet", CA: ca,
		Log: slog.New(slog.NewTextHandler(&logs, nil)),
		Credentials: map[string]credentials.Credential{
			"httpbin.org": {Name: "echo", Hosts: []string{"httpbin.org"}, Header: "X-Hcc-Test", Value: marker},
		},
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Serve(ctx, l) }()

	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	fmt.Fprint(c, "CONNECT httpbin.org:443 HTTP/1.1\r\nHost: httpbin.org:443\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", resp, err)
	}
	_ = resp.Body.Close()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.PEM())
	tc := tls.Client(&prefixConn{Conn: c, prefix: drain(br)}, &tls.Config{RootCAs: pool, ServerName: "httpbin.org", MinVersion: tls.VersionTLS12})
	fmt.Fprint(tc, "GET /headers HTTP/1.1\r\nHost: httpbin.org\r\nX-Hcc-Test: guest-value\r\nConnection: close\r\n\r\n")
	resp, err = http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		t.Fatalf("request through the broker: %v\n%s", err, logs.String())
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), marker) || strings.Contains(string(body), "guest-value") {
		t.Fatalf("status %d, body:\n%s", resp.StatusCode, body)
	}
	if strings.Contains(logs.String(), marker) {
		t.Error("the secret appears in the log")
	}
	t.Logf("upstream saw: %s", strings.Join(strings.Fields(string(body)), " "))
}
