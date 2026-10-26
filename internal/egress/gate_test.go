package egress

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
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

	"github.com/first-it-consulting/harness-control-center/internal/policy"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

const gateHarness = `
apiVersion: hcc/v1alpha1
kind: Harness
metadata: {name: g}
spec:
  runtime: claude-code
  prompt: p
  approvals: {requireFor: [git.push, http.delete]}
  permissions:
    network:
      egress:
        - host: example.com
`

type gateEnv struct {
	*brokerEnv
	mu     sync.Mutex
	asked  []GateRequest
	answer error
	body   chan []byte
}

// startGate runs an inspecting proxy for a harness with approvals; no
// credentials at all.
func startGate(t *testing.T, gate bool) *gateEnv {
	t.Helper()
	env := &gateEnv{brokerEnv: &brokerEnv{log: &lockedLog{}, seen: make(chan *http.Request, 16)}, body: make(chan []byte, 16)}
	env.upstream = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		env.body <- b
		fmt.Fprint(w, "upstream ok")
	}))
	env.upstream.StartTLS()
	t.Cleanup(env.upstream.Close)
	h, err := spec.Parse([]byte(gateHarness))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := policy.New(h)
	if err != nil {
		t.Fatal(err)
	}
	if env.ca, err = NewCA([]string{"example.com"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(env.upstream.Certificate())
	addr := env.upstream.Listener.Addr().String()
	p := &Proxy{
		Policy: engine, JobID: "job-1", Log: slog.New(slog.NewTextHandler(env.log, nil)),
		CA: env.ca, UpstreamRoots: roots, Inspect: true,
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		},
		Dial: func(ctx context.Context, _ netip.AddrPort) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		},
		LocalAddrs: func() ([]netip.Addr, error) { return nil, nil },
	}
	if gate {
		p.Gate = func(_ context.Context, r GateRequest) error {
			env.mu.Lock()
			defer env.mu.Unlock()
			env.asked = append(env.asked, r)
			return env.answer
		}
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

func (env *gateEnv) do(t *testing.T, req string) *http.Response {
	t.Helper()
	tc, err := env.guest(t, "example.com:443", "example.com")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tc.Close() })
	return get(t, tc, req)
}

func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

func TestGateInspectsUncredentialedHosts(t *testing.T) {
	env := startGate(t, true)
	// Not gated: passes, through the broker (a plain tunnel would not
	// have accepted the job CA's certificate).
	resp := env.do(t, "GET /x HTTP/1.1\r\nHost: example.com\r\n\r\n")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(env.asked) != 0 {
		t.Fatalf("GET: %d, asked %v", resp.StatusCode, env.asked)
	}
	<-env.body
	env.answer = errors.New("denied by you")
	resp = env.do(t, "DELETE /repo HTTP/1.1\r\nHost: example.com\r\n\r\n")
	msg, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(msg), "denied by you") {
		t.Errorf("denied DELETE: %d %q", resp.StatusCode, msg)
	}
	select {
	case <-env.body:
		t.Error("a denied request reached the upstream")
	default:
	}
	if len(env.asked) != 1 || env.asked[0].Method != "DELETE" || env.asked[0].Host != "example.com" || env.asked[0].Path != "/repo" {
		t.Errorf("asked %+v", env.asked)
	}
}

func TestGateGitPushShowsRefsAndForwardsTheWholeBody(t *testing.T) {
	env := startGate(t, true)
	old, nw := strings.Repeat("a", 40), strings.Repeat("b", 40)
	body := pkt(old+" "+nw+" refs/heads/main\x00report-status side-band-64k\n") +
		pkt(strings.Repeat("0", 40)+" "+nw+" refs/heads/feature\n") +
		pkt(old+" "+strings.Repeat("0", 40)+" refs/heads/old\n") + "0000" + "PACK" + strings.Repeat("x", 100<<10)
	resp := env.do(t, fmt.Sprintf("POST /g/r.git/git-receive-pack HTTP/1.1\r\nHost: example.com\r\nContent-Length: %d\r\n\r\n%s", len(body), body))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approved push: %d", resp.StatusCode)
	}
	if got := <-env.body; !bytes.Equal(got, []byte(body)) {
		t.Errorf("upstream got %d bytes, want %d (identical)", len(got), len(body))
	}
	want := []string{"refs/heads/main: aaaaaaaaaaaa → bbbbbbbbbbbb", "refs/heads/feature: create at bbbbbbbbbbbb", "refs/heads/old: delete (was aaaaaaaaaaaa)"}
	if len(env.asked) != 1 || strings.Join(env.asked[0].Details, "|") != strings.Join(want, "|") ||
		strings.Join(env.asked[0].Actions, ",") != "http.post,git.push" {
		t.Errorf("asked %+v", env.asked)
	}
}

func TestGateWithoutDeciderFailsClosed(t *testing.T) {
	env := startGate(t, false)
	resp := env.do(t, "DELETE /repo HTTP/1.1\r\nHost: example.com\r\n\r\n")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestPushCommandsOnGarbage(t *testing.T) {
	for _, body := range []string{"", "zzzz", "0003", pkt("not a ref update"), strings.Repeat("ffff", 10)} {
		cmds, rest, _ := pushCommands(io.NopCloser(strings.NewReader(body)))
		got, _ := io.ReadAll(rest)
		if string(got) != body || len(cmds) > 1 {
			t.Errorf("%q: cmds %v, body %q", body, cmds, got)
		}
	}
}

func TestCAWildcards(t *testing.T) {
	ca, err := NewCA([]string{"*.npmjs.org", "example.com"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for host, ok := range map[string]bool{"registry.npmjs.org": true, "a.b.npmjs.org": true, "npmjs.org": false, "evilnpmjs.org": false, "example.com": true, "x.example.com": false} {
		leaf, err := ca.Leaf(host)
		if (err == nil) != ok {
			t.Errorf("Leaf(%s): %v", host, err)
			continue
		}
		if ok {
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(ca.PEM())
			cert, _ := x509.ParseCertificate(leaf.Certificate[0])
			if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: host}); err != nil {
				t.Errorf("%s does not verify: %v", host, err)
			}
		}
	}
}
