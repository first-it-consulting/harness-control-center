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
	return startGateFor(t, gate, gateHarness, nil)
}

func startGateFor(t *testing.T, gate bool, doc string, mutate func(*Proxy)) *gateEnv {
	t.Helper()
	env := &gateEnv{brokerEnv: &brokerEnv{log: &lockedLog{}, seen: make(chan *http.Request, 16)}, body: make(chan []byte, 16)}
	env.upstream = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		env.body <- b
		fmt.Fprint(w, "upstream ok")
	}))
	env.upstream.StartTLS()
	t.Cleanup(env.upstream.Close)
	h, err := spec.Parse([]byte(doc))
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
	if mutate != nil {
		mutate(p)
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
	long := pkt(strings.Repeat("a", 40) + " " + strings.Repeat("b", 40) + " refs/heads/" + strings.Repeat("x", maxRefName) + "\n")
	for _, body := range []string{"", "zzzz", "0003", pkt("not a ref update"), strings.Repeat("ffff", 10), long + "0000"} {
		_, rest, err := pushCommands(io.NopCloser(strings.NewReader(body)))
		got, _ := io.ReadAll(rest)
		if err == nil || string(got) != body {
			t.Errorf("%q: err %v, body %q", body, err, got)
		}
	}
}

// A push whose ref updates cannot all be shown is refused, not asked.
func TestGateRefusesPushesItCannotShowInFull(t *testing.T) {
	env := startGate(t, true)
	var b strings.Builder
	for i := range maxPushCommands + 1 {
		b.WriteString(pkt(fmt.Sprintf("%040d %040d refs/heads/b%d\n", 1, 2, i)))
	}
	b.WriteString("0000PACK")
	body := b.String()
	resp := env.do(t, fmt.Sprintf("POST /r.git/git-receive-pack HTTP/1.1\r\nHost: example.com\r\nContent-Length: %d\r\n\r\n%s", len(body), body))
	msg, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(msg), "more than") || len(env.asked) != 0 {
		t.Errorf("push of %d refs: %d %q, asked %d", maxPushCommands+1, resp.StatusCode, msg, len(env.asked))
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

func TestGateRefusesLongPathsAndUpgrades(t *testing.T) {
	env := startGate(t, true)
	resp := env.do(t, "DELETE /"+strings.Repeat("a", maxGatePath)+" HTTP/1.1\r\nHost: example.com\r\n\r\n")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || len(env.asked) != 0 {
		t.Errorf("long path: %d, asked %d", resp.StatusCode, len(env.asked))
	}
	resp = env.do(t, "GET /ws HTTP/1.1\r\nHost: example.com\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("upgrade: %d", resp.StatusCode)
	}
	select {
	case <-env.body:
		t.Error("a refused request reached the upstream")
	default:
	}
}

const mcpHarness = `
apiVersion: hcc/v1alpha1
kind: Harness
metadata: {name: g}
spec:
  runtime: claude-code
  prompt: p
  approvals: {requireFor: [mcp.docs.delete_page]}
  mcp:
    servers: [{name: docs, url: "https://example.com/mcp"}]
  permissions:
    network:
      egress:
        - host: example.com
`

func TestGateHoldsMCPToolCalls(t *testing.T) {
	env := startGateFor(t, true, mcpHarness, func(p *Proxy) {
		p.MCP = []Endpoint{{Name: "docs", Host: "example.com", Port: 443, Path: "/mcp"}}
	})
	post := func(body string) *http.Response {
		return env.do(t, fmt.Sprintf("POST /mcp HTTP/1.1\r\nHost: example.com\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body))
	}
	// Another tool: not gated, forwarded unchanged.
	search := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{"q":"x"}}}`
	resp := post(search)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(env.asked) != 0 || string(<-env.body) != search {
		t.Fatalf("search: %d, asked %v", resp.StatusCode, env.asked)
	}
	// The gated tool: held, shown with its arguments, then forwarded.
	del := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"Delete_Page","arguments":{"page":"home"}}}`
	resp = post(del)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(<-env.body) != del {
		t.Fatalf("approved delete: %d", resp.StatusCode)
	}
	if len(env.asked) != 1 || !strings.Contains(strings.Join(env.asked[0].Actions, ","), "mcp.docs.delete_page") ||
		len(env.asked[0].Details) != 1 || env.asked[0].Details[0] != `docs: tool Delete_Page, arguments {"page":"home"}` {
		t.Errorf("asked %+v", env.asked)
	}
	// A message that cannot be read is refused, never forwarded.
	resp = post(`{"jsonrpc":"2.0","method":"tools/call"`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("unreadable message: %d", resp.StatusCode)
	}
	select {
	case <-env.body:
		t.Error("an unreadable message reached the server")
	default:
	}
}

const a2aHarness = `
apiVersion: hcc/v1alpha1
kind: Harness
metadata: {name: g}
spec:
  runtime: claude-code
  prompt: p
  approvals: {requireFor: [a2a.reviewer.send]}
  a2a:
    agents: [{name: reviewer, url: "https://example.com/a2a"}]
  permissions:
    network:
      egress:
        - host: example.com
`

func TestGateHoldsA2AMessages(t *testing.T) {
	env := startGateFor(t, true, a2aHarness, func(p *Proxy) {
		p.A2A = []Endpoint{{Name: "reviewer", Host: "example.com", Port: 443, Path: "/a2a"}}
	})
	post := func(body string) *http.Response {
		return env.do(t, fmt.Sprintf("POST /a2a HTTP/1.1\r\nHost: example.com\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body))
	}
	get := `{"jsonrpc":"2.0","id":1,"method":"tasks/get","params":{"id":"t1"}}`
	resp := post(get)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(env.asked) != 0 || string(<-env.body) != get {
		t.Fatalf("tasks/get: %d, asked %v", resp.StatusCode, env.asked)
	}
	env.answer = errors.New("denied by you")
	send := `{"jsonrpc":"2.0","id":2,"method":"message/send","params":{"message":{"role":"user","parts":[{"kind":"text","text":"review my diff"}],"messageId":"m1"}}}`
	resp = post(send)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("denied send: %d", resp.StatusCode)
	}
	if len(env.asked) != 1 || !strings.Contains(strings.Join(env.asked[0].Actions, ","), "a2a.reviewer.send") ||
		!strings.Contains(env.asked[0].Details[0], `"text":"review my diff"`) {
		t.Errorf("asked %+v", env.asked)
	}
	select {
	case <-env.body:
		t.Error("a denied message reached the agent")
	default:
	}
	long := `{"jsonrpc":"2.0","id":3,"method":"message/send","params":{"message":{"parts":[{"kind":"text","text":"` + strings.Repeat("x", 3000) + `"}]}}}`
	resp = post(long)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || len(env.asked) != 1 {
		t.Errorf("a message too long to show: %d, asked %d times", resp.StatusCode, len(env.asked))
	}
}

// The review's bypasses: another spelling of the path, an encoded body,
// a body that is not JSON-RPC, keys a server could read differently, and
// a bare pattern.
func TestGateMCPBypassesAreClosed(t *testing.T) {
	doc := strings.Replace(mcpHarness, "[mcp.docs.delete_page]", "[mcp]", 1)
	env := startGateFor(t, true, doc, func(p *Proxy) {
		p.MCP = []Endpoint{{Name: "docs", Host: "example.com", Port: 443, Path: "/mcp"}}
	})
	env.answer = errors.New("denied by you")
	send := func(path, hdr, body string) int {
		resp := env.do(t, fmt.Sprintf("POST %s HTTP/1.1\r\nHost: example.com\r\n%sContent-Length: %d\r\n\r\n%s", path, hdr, len(body), body))
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_page","arguments":{}}}`
	for _, path := range []string{"/mcp", "//mcp", "/MCP/", "/mcp/.", "/other"} {
		env.asked = nil
		if code := send(path, "", call); code != http.StatusForbidden || len(env.asked) != 1 {
			t.Errorf("POST %s: %d, asked %d times (want held and denied)", path, code, len(env.asked))
		}
	}
	env.asked = nil
	for name, tt := range map[string]struct{ hdr, body string }{
		"gzip":       {"Content-Encoding: gzip\r\n", call},
		"form":       {"Content-Type: application/x-www-form-urlencoded\r\n", "a=b"},
		"case keys":  {"", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","Name":"delete_page"}}`},
		"dup keys":   {"", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","name":"delete_page"}}`},
		"two values": {"", call + call},
	} {
		if code := send("/mcp", tt.hdr, tt.body); code != http.StatusForbidden {
			t.Errorf("%s: %d", name, code)
		}
	}
	if len(env.asked) != 0 {
		t.Errorf("unreadable requests were offered for approval: %+v", env.asked)
	}
	select {
	case b := <-env.body:
		t.Errorf("the server got %q", b)
	default:
	}
	// Without a body (e.g. an event stream), nothing to inspect.
	resp := env.do(t, "GET /mcp HTTP/1.1\r\nHost: example.com\r\n\r\n")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET without a body: %d", resp.StatusCode)
	}
	<-env.body
}
