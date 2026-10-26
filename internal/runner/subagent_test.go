package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/results"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

// scriptProvider runs a job by its prompt: "block..." runs until
// stopped, anything else prints an answer and exits.
type scriptProvider struct {
	mu    sync.Mutex
	specs map[string]sandbox.Spec
}

func (p *scriptProvider) Name() string { return "script" }

func (p *scriptProvider) Run(ctx context.Context, s sandbox.Spec, console io.Writer) (sandbox.Result, error) {
	p.mu.Lock()
	p.specs[s.ID] = s
	p.mu.Unlock()
	var task Task
	_ = json.Unmarshal(s.Task, &task)
	if strings.HasPrefix(task.Prompt, "block") {
		<-ctx.Done()
		return sandbox.Result{Outcome: sandbox.OutcomeKilled}, nil
	}
	_, _ = io.WriteString(console, "boot noise\nHCC-RESULT-BEGIN\nanswer to "+task.Prompt+"\n  indented\n\x1b[?25hHCC-RESULTS saved\nHCC-RESULT-END exit=0\n")
	if s.Collect != nil {
		if err := s.Collect(strings.NewReader("disk")); err != nil {
			return sandbox.Result{Outcome: sandbox.OutcomeFailed}, err
		}
	}
	return sandbox.Result{Outcome: sandbox.OutcomeExited}, nil
}

func (p *scriptProvider) spec(id string) sandbox.Spec {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.specs[id]
}

type treeEnv struct {
	r  *Runner
	p  *scriptProvider
	mu sync.Mutex
	ej map[string]EgressJob // by job ID
}

func newTreeEnv(t *testing.T, maxConcurrent int) *treeEnv {
	t.Helper()
	e := &treeEnv{p: &scriptProvider{specs: map[string]sandbox.Spec{}}, ej: map[string]EgressJob{}}
	r, err := New(Config{
		Provider: e.p, Images: []string{"base"}, MaxConcurrent: maxConcurrent,
		Egress: func(ej EgressJob) (sandbox.Proxy, error) {
			e.mu.Lock()
			e.ej[ej.JobID] = ej
			e.mu.Unlock()
			return nopProxy{job: ej.JobID}, nil
		},
		Results: func(string, io.Reader, int64) (results.Summary, error) {
			return results.Summary{Files: 1, Bytes: 4}, nil
		},
		ReadResults: func(id string) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("tar of " + id)), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.Shutdown(ctx)
	})
	e.r = r
	return e
}

func (e *treeEnv) egress(t *testing.T, id string) EgressJob {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	ej, ok := e.ej[id]
	if !ok {
		t.Fatalf("no egress proxy for %s", id)
	}
	return ej
}

const parentDoc = `
apiVersion: hcc/v1alpha1
kind: Harness
metadata: {name: lead}
spec:
  runtime: claude-code
  prompt: block until stopped
  sandbox: {image: base, cpus: 2, memoryMiB: 1024, timeout: 10m}
  permissions:
    subagents: {max: 2}
    network:
      egress:
        - host: gitlab.com
        - {host: "*.example.com", ports: [443, 8443]}
  approvals: {requireFor: [git.push]}
`

// startParent runs a parent agent and returns its job ID.
func (e *treeEnv) startParent(t *testing.T, doc string) string {
	t.Helper()
	h, err := spec.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	id, err := e.r.Submit(h)
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, e.r, id, StateRunning)
	return id
}

// call makes a request to a job's subagent API.
func (e *treeEnv) call(t *testing.T, jobID, method, path, body string) (int, string) {
	t.Helper()
	api := e.egress(t, jobID).Agents
	if api == nil {
		t.Fatalf("%s has no subagent API", jobID)
	}
	w := httptest.NewRecorder()
	api.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w.Code, w.Body.String()
}

func (e *treeEnv) spawn(t *testing.T, parent, body string) Job {
	t.Helper()
	code, out := e.call(t, parent, http.MethodPost, "/hcc/v1/agents", body)
	if code != http.StatusCreated {
		t.Fatalf("spawn = %d %s", code, out)
	}
	var j Job
	if err := json.Unmarshal([]byte(out), &j); err != nil {
		t.Fatal(err)
	}
	return j
}

func TestSubagentInheritsAndShares(t *testing.T) {
	e := newTreeEnv(t, 4)
	parent := e.startParent(t, parentDoc)
	j := e.spawn(t, parent, `{"name": "helper", "prompt": "block, then report",
		"sandbox": {"cpus": 1, "timeout": "5m"},
		"permissions": {"network": {"egress": [{"host": "api.example.com"}]}}}`)
	if j.Parent != parent || j.Harness != "helper" {
		t.Errorf("child = %+v", j)
	}
	waitState(t, e.r, j.ID, StateRunning)

	pe, ce := e.egress(t, parent), e.egress(t, j.ID)
	if ce.Meter == nil || ce.Meter != pe.Meter {
		t.Error("the subagent does not share its parent's budget")
	}
	h := ce.Harness
	if !slices.Equal(h.Spec.Approvals.RequireFor, []string{"git.push"}) {
		t.Errorf("approvals = %v, want the parent's", h.Spec.Approvals.RequireFor)
	}
	if h.Spec.Sandbox.Image != "base" || h.Spec.Sandbox.CPUs != 1 || h.Spec.Sandbox.MemoryMiB != 1024 || h.Spec.Sandbox.Timeout.Duration != 5*time.Minute {
		t.Errorf("sandbox = %+v", h.Spec.Sandbox)
	}
	if ce.Agents != nil {
		t.Error("a subagent without subagents.max got the subagent API")
	}
	if s := e.p.spec(j.ID); s.Timeout != 5*time.Minute || s.CPUs != 1 {
		t.Errorf("sandbox spec = %+v", s)
	}

	code, out := e.call(t, parent, http.MethodGet, "/hcc/v1/agents", "")
	if code != http.StatusOK || !strings.Contains(out, j.ID) {
		t.Errorf("list = %d %s", code, out)
	}
}

func TestSubagentLimits(t *testing.T) {
	e := newTreeEnv(t, 4)
	parent := e.startParent(t, parentDoc)
	for name, body := range map[string]string{ //nolint:gosec // requests naming a credential, not credentials
		"wider egress":     `{"prompt": "x", "permissions": {"network": {"egress": [{"host": "evil.org"}]}}}`,
		"other port":       `{"prompt": "x", "permissions": {"network": {"egress": [{"host": "gitlab.com", "ports": [22]}]}}}`,
		"wider wildcard":   `{"prompt": "x", "permissions": {"network": {"egress": [{"host": "*.com"}]}}}`,
		"private networks": `{"prompt": "x", "permissions": {"network": {"allowPrivateNetworks": true}}}`,
		"credential":       `{"prompt": "x", "permissions": {"credentials": ["gitlab"]}}`,
		"more subagents":   `{"prompt": "x", "permissions": {"subagents": {"max": 3}}}`,
		"more cpus":        `{"prompt": "x", "sandbox": {"cpus": 4}}`,
		"more memory":      `{"prompt": "x", "sandbox": {"memoryMiB": 2048}}`,
		"more time":        `{"prompt": "x", "sandbox": {"timeout": "11m"}}`,
		"an image":         `{"prompt": "x", "sandbox": {"image": "other"}}`,
		"a trigger":        `{"prompt": "x", "trigger": {"schedule": "* * * * *"}}`,
		"approvals":        `{"prompt": "x", "approvals": {"requireFor": []}}`,
		"no prompt":        `{"prompt": " "}`,
		"bad name":         `{"name": "Bad Name", "prompt": "x"}`,
		"two documents":    "prompt: x\n---\nprompt: y\n",
		"not json":         `{"prompt": `,
	} {
		if code, out := e.call(t, parent, http.MethodPost, "/hcc/v1/agents", body); code < 400 {
			t.Errorf("%s: %d %s", name, code, out)
		}
	}
	if kids, _ := e.r.Subagents(parent); len(kids) != 0 {
		t.Fatalf("refused requests started %d subagents", len(kids))
	}

	// subagents.max covers the whole subtree: a helper that starts its
	// own helper uses up the parent's second one.
	mid := e.spawn(t, parent, `{"prompt": "block as a lead", "permissions": {"subagents": {"max": 1}}}`)
	waitState(t, e.r, mid.ID, StateRunning)
	e.spawn(t, mid.ID, `{"prompt": "block as a leaf"}`)
	if code, out := e.call(t, mid.ID, http.MethodPost, "/hcc/v1/agents", `{"prompt": "x"}`); code != http.StatusForbidden || !strings.Contains(out, "limit of 1") {
		t.Errorf("third level beyond its own limit = %d %s", code, out)
	}
	if code, out := e.call(t, parent, http.MethodPost, "/hcc/v1/agents", `{"prompt": "x"}`); code != http.StatusForbidden || !strings.Contains(out, "limit of 2") {
		t.Errorf("beyond the tree's limit = %d %s", code, out)
	}
}

func TestSubagentsOnlySeeTheirOwn(t *testing.T) {
	e := newTreeEnv(t, 4)
	a := e.startParent(t, parentDoc)
	b := e.startParent(t, parentDoc)
	child := e.spawn(t, a, `{"prompt": "block"}`)
	for _, path := range []string{"/hcc/v1/agents/" + child.ID, "/hcc/v1/agents/" + child.ID + "/results", "/hcc/v1/agents/" + a} {
		if code, _ := e.call(t, b, http.MethodGet, path, ""); code != http.StatusNotFound {
			t.Errorf("GET %s from another agent = %d", path, code)
		}
	}
	if code, _ := e.call(t, b, http.MethodPost, "/hcc/v1/agents/"+child.ID+"/stop", ""); code != http.StatusNotFound {
		t.Errorf("another agent stopped the subagent: %d", code)
	}
	if j, _ := e.r.Get(child.ID); j.State.Terminal() {
		t.Error("the subagent was stopped")
	}
	// Its own parent may stop it.
	if code, out := e.call(t, a, http.MethodPost, "/hcc/v1/agents/"+child.ID+"/stop", ""); code != http.StatusOK {
		t.Errorf("stop = %d %s", code, out)
	}
	waitState(t, e.r, child.ID, StateCancelled)
}

func TestSubagentOutputAndResults(t *testing.T) {
	e := newTreeEnv(t, 4)
	parent := e.startParent(t, parentDoc)
	child := e.spawn(t, parent, `{"prompt": "sum it up"}`)
	waitState(t, e.r, child.ID, StateSucceeded)
	code, out := e.call(t, parent, http.MethodGet, "/hcc/v1/agents/"+child.ID, "")
	var v SubagentView
	if err := json.Unmarshal([]byte(out), &v); err != nil || code != http.StatusOK {
		t.Fatalf("get = %d %s", code, out)
	}
	if v.Output != "answer to sum it up\n  indented\n" || v.State != StateSucceeded {
		t.Errorf("view = %+v", v)
	}
	code, out = e.call(t, parent, http.MethodGet, "/hcc/v1/agents/"+child.ID+"/results", "")
	if code != http.StatusOK || out != "tar of "+child.ID {
		t.Errorf("results = %d %q", code, out)
	}
}

func TestSubagentsEndWithTheirParent(t *testing.T) {
	e := newTreeEnv(t, 2) // parent and one child run; the second child queues
	parent := e.startParent(t, parentDoc)
	running := e.spawn(t, parent, `{"prompt": "block"}`)
	waitState(t, e.r, running.ID, StateRunning)
	queued := e.spawn(t, parent, `{"prompt": "block too"}`)
	if j, _ := e.r.Get(queued.ID); j.State != StateQueued {
		t.Fatalf("second child is %s, want queued", j.State)
	}
	if err := e.r.Cancel(parent); err != nil {
		t.Fatal(err)
	}
	waitState(t, e.r, parent, StateCancelled)
	if j := waitState(t, e.r, running.ID, StateCancelled); j.Error != "stopped: its parent agent ended" {
		t.Errorf("running child: %q", j.Error)
	}
	if j := waitState(t, e.r, queued.ID, StateCancelled); j.Error != "cancelled: its parent agent ended" {
		t.Errorf("queued child: %q", j.Error)
	}
	if _, err := e.r.Spawn(parent, &spec.Subagent{Prompt: "late"}); err == nil {
		t.Error("a finished agent started a subagent")
	}
}

func TestSubagentsQueueAheadOfOtherJobs(t *testing.T) {
	e := newTreeEnv(t, 1)
	parent := e.startParent(t, parentDoc)
	other, err := e.r.Submit(harness(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	child := e.spawn(t, parent, `{"prompt": "x"}`)
	e.r.mu.Lock()
	order := []string{}
	for _, q := range e.r.queue {
		order = append(order, q.ID)
	}
	e.r.mu.Unlock()
	if !slices.Equal(order, []string{child.ID, other}) {
		t.Errorf("queue = %v, want the subagent first", order)
	}
}

func TestSubagentsShareTheBudget(t *testing.T) {
	e := newTreeEnv(t, 4)
	parent := e.startParent(t, strings.Replace(parentDoc, "  approvals:", "  budget: {maxTokens: 100}\n  approvals:", 1))
	child := e.spawn(t, parent, `{"prompt": "block", "permissions": {"network": {"egress": [{"host": "gitlab.com"}]}}}`)
	waitState(t, e.r, child.ID, StateRunning)
	// The child's model traffic exhausts the shared meter: the whole tree
	// stops.
	e.egress(t, child.ID).Meter.Record(150, 0)
	waitState(t, e.r, parent, StateOverBudget)
	if j := waitState(t, e.r, child.ID, StateCancelled); j.Error != "stopped: its parent agent ended" {
		t.Errorf("child: %q", j.Error)
	}
}

func TestResultWatcherKeepsTheEndOfTheOutput(t *testing.T) {
	var w resultWatcher
	_, _ = io.WriteString(&w, "HCC-RESULT-BEGIN\n")
	line := strings.Repeat("x", 1000) + "\n"
	for range 100 {
		_, _ = io.WriteString(&w, line)
	}
	_, _ = io.WriteString(&w, "last\nHCC-RESULT-END exit=0\nafter\n")
	out := w.agentOutput()
	if len(out) > maxOutput || !strings.HasSuffix(out, "x\nlast\n") || strings.Contains(out, "after") {
		t.Errorf("output: %d bytes, ends %q", len(out), out[max(0, len(out)-20):])
	}
}
