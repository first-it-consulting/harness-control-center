package runner

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

type fakeMCP struct {
	mu      sync.Mutex
	started []string
	ctxs    []context.Context
	sent    []string
}

func (f *fakeMCP) CheckMCP(plugin string) error {
	if plugin != "gh-mcp" {
		return errors.New("no such mcp-server plugin")
	}
	return nil
}

func (f *fakeMCP) StartMCP(ctx context.Context, jobID, plugin string) (MCPConn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, jobID+"/"+plugin)
	f.ctxs = append(f.ctxs, ctx)
	return f, nil
}

func (f *fakeMCP) Call(_ context.Context, id string, msg []byte) ([]byte, error) {
	f.mu.Lock()
	f.sent = append(f.sent, string(msg))
	f.mu.Unlock()
	return []byte(`{"jsonrpc":"2.0","id":` + id + `,"result":{"ok":true}}`), nil
}

func (f *fakeMCP) Send(msg []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, string(msg))
	return nil
}

const mcpDoc = `
apiVersion: hcc/v1alpha1
kind: Harness
metadata: {name: m}
spec:
  runtime: claude-code
  prompt: block
  sandbox: {image: base}
  mcp:
    servers: [{name: gh, plugin: gh-mcp}]
  approvals: {requireFor: [mcp.gh.delete_repo]}
`

func TestMCPPluginRelay(t *testing.T) {
	fm := &fakeMCP{}
	var mu sync.Mutex
	ejs := map[string]EgressJob{}
	sp := &scriptProvider{specs: map[string]sandbox.Spec{}}
	r, err := New(Config{Provider: sp, Images: []string{"base"}, MCP: fm, ApprovalTimeout: time.Minute,
		Egress: func(ej EgressJob) (sandbox.Proxy, error) {
			mu.Lock()
			ejs[ej.JobID] = ej
			mu.Unlock()
			return nopProxy{}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })
	h, err := spec.Parse([]byte(mcpDoc))
	if err != nil {
		t.Fatal(err)
	}
	id, err := r.Submit(h)
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, r, id, StateRunning)
	mu.Lock()
	local := ejs[id].Local
	mu.Unlock()
	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		local.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/hcc/v1/mcp/gh", strings.NewReader(body)))
		return w
	}

	if w := post(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"id":1,"result"`) {
		t.Errorf("tools/list = %d %s", w.Code, w.Body)
	}
	if w := post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); w.Code != http.StatusAccepted {
		t.Errorf("notification = %d", w.Code)
	}
	if w := post(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_repos"}}`); !strings.Contains(w.Body.String(), "result") {
		t.Errorf("ungated call = %s", w.Body)
	}

	// The gated call waits for a decision; denied, the server never sees it.
	done := make(chan string, 1)
	go func() {
		done <- post(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"Delete_Repo","arguments":{"repo":"x"}}}`).Body.String()
	}()
	var a []Approval
	for deadline := time.Now().Add(5 * time.Second); len(a) == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		a = r.Approvals()
	}
	if len(a) != 1 || a[0].Method != "MCP" || a[0].Host != "gh" || a[0].Path != "/Delete_Repo" || a[0].Details[0] != `gh: tool Delete_Repo, arguments {"repo":"x"}` {
		t.Fatalf("approvals = %+v", a)
	}
	if err := r.Decide(a[0].ID, false); err != nil {
		t.Fatal(err)
	}
	if got := <-done; !strings.Contains(got, `"id":3,"error"`) || !strings.Contains(got, "not approved") {
		t.Errorf("denied call = %s", got)
	}
	fm.mu.Lock()
	if len(fm.started) != 1 || len(fm.sent) != 3 || strings.Contains(strings.Join(fm.sent, ""), "Delete_Repo") {
		t.Errorf("started %v, sent %v", fm.started, fm.sent)
	}
	ctx := fm.ctxs[0]
	fm.mu.Unlock()

	if w := post(`not json`); !strings.Contains(w.Body.String(), "-32700") {
		t.Errorf("garbage = %s", w.Body)
	}
	w := httptest.NewRecorder()
	local.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/hcc/v1/mcp/other", strings.NewReader("{}")))
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown server = %d", w.Code)
	}

	// The plugin ends with its job.
	if err := r.Cancel(id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Error("the plugin's context outlived its job")
	}
}

func TestMCPPluginMustBeInstalled(t *testing.T) {
	r, err := New(Config{Provider: newFakeProvider(), Images: []string{"base"}, MCP: &fakeMCP{},
		Egress: func(EgressJob) (sandbox.Proxy, error) { return nopProxy{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })
	h, err := spec.Parse([]byte(strings.Replace(mcpDoc, "gh-mcp", "other", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Submit(h); err == nil || !strings.Contains(err.Error(), "MCP server gh") {
		t.Errorf("Submit = %v", err)
	}
	r2, err := New(Config{Provider: newFakeProvider(), Images: []string{"base"}, // no MCP plugins at all
		Egress: func(EgressJob) (sandbox.Proxy, error) { return nopProxy{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r2.Shutdown(context.Background()) })
	if _, err := r2.Submit(h); err == nil || !strings.Contains(err.Error(), "cannot run MCP plugins") {
		t.Errorf("Submit without MCP plugins = %v", err)
	}
}
