package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

// fakeProvider runs "sandboxes" that end as configured. With block set,
// a run waits until release is closed or its context ends.
type fakeProvider struct {
	outcome sandbox.Outcome
	err     error
	block   bool
	release chan struct{}

	mu      sync.Mutex
	specs   []sandbox.Spec
	running atomic.Int32
	maxSeen atomic.Int32
}

func newFakeProvider() *fakeProvider { return &fakeProvider{release: make(chan struct{})} }

func (p *fakeProvider) Name() string { return "fake" }

func (p *fakeProvider) Run(ctx context.Context, s sandbox.Spec, _ io.Writer) (sandbox.Result, error) {
	p.mu.Lock()
	p.specs = append(p.specs, s)
	p.mu.Unlock()
	n := p.running.Add(1)
	defer p.running.Add(-1)
	for {
		m := p.maxSeen.Load()
		if n <= m || p.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	if p.block {
		select {
		case <-p.release:
		case <-ctx.Done():
			return sandbox.Result{Outcome: sandbox.OutcomeKilled}, nil
		}
	}
	return sandbox.Result{Outcome: p.outcome}, p.err
}

func (p *fakeProvider) startedSpecs() []sandbox.Spec {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.specs)
}

const baseHarness = `
apiVersion: hcc/v1alpha1
kind: Harness
metadata: {name: hello}
spec:
  runtime: claude-code
  prompt: hi
  sandbox: {image: base, cpus: 2, memoryMiB: 512, timeout: 10m}
`

func harness(t *testing.T, extra string) *spec.Harness {
	t.Helper()
	h, err := spec.Parse([]byte(baseHarness + extra))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func newRunner(t *testing.T, p *fakeProvider, maxConcurrent int) *Runner {
	t.Helper()
	var n atomic.Int32
	r, err := New(Config{
		Provider:      p,
		Images:        []string{"base"},
		MaxConcurrent: maxConcurrent,
		NewID:         func() string { return fmt.Sprintf("job-%d", n.Add(1)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := r.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return r
}

// waitState polls until the job reaches want (or fails the test).
func waitState(t *testing.T, r *Runner, id string, want State) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if j, ok := r.Get(id); ok && j.State == want {
			return j
		}
		time.Sleep(2 * time.Millisecond)
	}
	j, _ := r.Get(id)
	t.Fatalf("job %s: state %s, want %s (error %q)", id, j.State, want, j.Error)
	return j
}

func TestSubmitRunsToSuccessWithFullEventTrail(t *testing.T) {
	p := newFakeProvider()
	r := newRunner(t, p, 1)
	events, stop := r.Subscribe(16)
	defer stop()

	id, err := r.Submit(harness(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	j := waitState(t, r, id, StateSucceeded)
	if j.Started.IsZero() || j.Finished.IsZero() || j.Error != "" || j.Harness != "hello" {
		t.Errorf("job = %+v", j)
	}

	var trail []State
	for len(trail) < 4 {
		select {
		case e := <-events:
			if e.JobID == id {
				trail = append(trail, e.To)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("events so far: %v", trail)
		}
	}
	want := []State{StateQueued, StateProvisioning, StateRunning, StateSucceeded}
	if !slices.Equal(trail, want) {
		t.Errorf("events = %v, want %v", trail, want)
	}

	s := p.startedSpecs()[0]
	if s.ID != id || s.CPUs != 2 || s.MemoryMiB != 512 || s.Timeout != 10*time.Minute ||
		s.Image != "base" || len(s.BootArgs) != 0 {
		t.Errorf("sandbox spec = %+v", s)
	}
}

func TestOutcomesMapToStates(t *testing.T) {
	tests := []struct {
		name    string
		outcome sandbox.Outcome
		err     error
		want    State
		msg     string
	}{
		{"exited", sandbox.OutcomeExited, nil, StateSucceeded, ""},
		{"timed out", sandbox.OutcomeTimedOut, nil, StateTimedOut, "stopped after 10m0s"},
		{"killed", sandbox.OutcomeKilled, nil, StateCancelled, "stopped"},
		{"provider error", sandbox.OutcomeFailed, errors.New("kvm unavailable"), StateFailed, "kvm unavailable"},
		{"failed without error", sandbox.OutcomeFailed, nil, StateFailed, "sandbox reported failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newFakeProvider()
			p.outcome, p.err = tt.outcome, tt.err
			r := newRunner(t, p, 1)
			id, err := r.Submit(harness(t, ""))
			if err != nil {
				t.Fatal(err)
			}
			if j := waitState(t, r, id, tt.want); j.Error != tt.msg {
				t.Errorf("error = %q, want %q", j.Error, tt.msg)
			}
		})
	}
}

func TestFailClosedOnUnenforceablePermissions(t *testing.T) {
	tests := map[string]string{
		"tools":     "  permissions: {tools: {install: [git]}}\n",
		"subagents": "  permissions: {subagents: {max: 1}}\n",
		"approvals": "  approvals: {requireFor: [git.push]}\n",
		"schedule":  "  trigger: {schedule: '0 2 * * *'}\n",
		"loop":      "  trigger: {loop: {maxIterations: 3}}\n",
	}
	for name, extra := range tests {
		t.Run(name, func(t *testing.T) {
			p := newFakeProvider()
			r := newRunner(t, p, 1)
			_, err := r.Submit(harness(t, extra))
			if err == nil || !strings.Contains(err.Error(), "not supported by this runner yet") {
				t.Fatalf("Submit = %v, want not-supported error", err)
			}
			if len(r.List()) != 0 || len(p.startedSpecs()) != 0 {
				t.Error("rejected harness created a job")
			}
		})
	}
}

func TestRejectsUnknownImageAndInvalidHarness(t *testing.T) {
	r := newRunner(t, newFakeProvider(), 1)
	h := harness(t, "")
	h.Spec.Sandbox.Image = "ubuntu"
	if _, err := r.Submit(h); err == nil || !strings.Contains(err.Error(), "unknown image") {
		t.Errorf("unknown image: %v", err)
	}
	if _, err := r.Submit(&spec.Harness{}); err == nil {
		t.Error("unvalidated harness accepted")
	}
	if _, err := r.Submit(nil); err == nil {
		t.Error("nil harness accepted")
	}
}

func TestConcurrencyLimit(t *testing.T) {
	p := newFakeProvider()
	p.block = true
	r := newRunner(t, p, 2)
	var ids []string
	for range 4 {
		id, err := r.Submit(harness(t, ""))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	deadline := time.Now().Add(5 * time.Second)
	for p.running.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // give a third job the chance to (wrongly) start
	queued := 0
	for _, j := range r.List() {
		if j.State == StateQueued {
			queued++
		}
	}
	if queued != 2 {
		t.Errorf("queued = %d, want 2", queued)
	}
	close(p.release)
	for _, id := range ids {
		waitState(t, r, id, StateSucceeded)
	}
	if got := p.maxSeen.Load(); got != 2 {
		t.Errorf("max concurrent runs = %d, want 2", got)
	}
}

func TestCancelRunningAndQueued(t *testing.T) {
	p := newFakeProvider()
	p.block = true
	r := newRunner(t, p, 1)
	running, _ := r.Submit(harness(t, ""))
	waitState(t, r, running, StateRunning)
	queued, _ := r.Submit(harness(t, ""))

	if err := r.Cancel(queued); err != nil {
		t.Fatal(err)
	}
	if j := waitState(t, r, queued, StateCancelled); j.Error != "cancelled while queued" {
		t.Errorf("queued job error = %q", j.Error)
	}
	if err := r.Cancel(running); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, running, StateCancelled)
	if n := len(p.startedSpecs()); n != 1 {
		t.Errorf("provider ran %d sandboxes, want 1 (the queued job must never start)", n)
	}
	if err := r.Cancel("job-unknown"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Cancel(unknown) = %v", err)
	}
}

func TestKillSwitch(t *testing.T) {
	p := newFakeProvider()
	p.block = true
	r := newRunner(t, p, 1)
	a, _ := r.Submit(harness(t, ""))
	b, _ := r.Submit(harness(t, ""))
	waitState(t, r, a, StateRunning)

	r.KillAll()
	waitState(t, r, a, StateCancelled)
	waitState(t, r, b, StateCancelled)
	if _, err := r.Submit(harness(t, "")); !errors.Is(err, ErrKilled) {
		t.Fatalf("Submit after KillAll = %v, want ErrKilled", err)
	}

	r.Resume()
	close(p.release)
	c, err := r.Submit(harness(t, ""))
	if err != nil {
		t.Fatalf("Submit after Resume: %v", err)
	}
	waitState(t, r, c, StateSucceeded)
}

func TestShutdown(t *testing.T) {
	p := newFakeProvider()
	p.block = true
	r, err := New(Config{Provider: p, Images: []string{"base"}})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := r.Submit(harness(t, ""))
	waitState(t, r, id, StateRunning)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if j, _ := r.Get(id); j.State != StateCancelled {
		t.Errorf("state after shutdown = %s", j.State)
	}
	if _, err := r.Submit(harness(t, "")); !errors.Is(err, ErrClosed) {
		t.Errorf("Submit after Shutdown = %v, want ErrClosed", err)
	}
}

func TestTransitions(t *testing.T) {
	allowed := [][2]State{
		{StateQueued, StateProvisioning}, {StateQueued, StateCancelled},
		{StateProvisioning, StateRunning}, {StateProvisioning, StateFailed},
		{StateRunning, StateSucceeded}, {StateRunning, StateTimedOut}, {StateRunning, StateCancelled},
	}
	forbidden := [][2]State{
		{StateQueued, StateRunning}, {StateQueued, StateSucceeded},
		{StateSucceeded, StateRunning}, {StateCancelled, StateRunning},
		{StateFailed, StateQueued}, {StateRunning, StateQueued}, {StateTimedOut, StateSucceeded},
	}
	for _, tr := range allowed {
		if !canTransition(tr[0], tr[1]) {
			t.Errorf("%s → %s should be allowed", tr[0], tr[1])
		}
	}
	for _, tr := range forbidden {
		if canTransition(tr[0], tr[1]) {
			t.Errorf("%s → %s must be forbidden", tr[0], tr[1])
		}
	}
	for _, s := range []State{StateSucceeded, StateFailed, StateTimedOut, StateCancelled} {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
}

func TestSubscribeNeverBlocksRunner(t *testing.T) {
	p := newFakeProvider()
	r := newRunner(t, p, 1)
	_, stop := r.Subscribe(1) // never read
	defer stop()
	id, _ := r.Submit(harness(t, ""))
	waitState(t, r, id, StateSucceeded) // would hang if emit blocked
	stop()
	stop() // idempotent
}

func TestEventString(t *testing.T) {
	e := Event{JobID: "job-1", From: StateRunning, To: StateFailed, At: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC), Message: "boom"}
	if got := e.String(); got != "2026-10-25T12:00:00Z job-1 running → failed: boom" {
		t.Errorf("String() = %q", got)
	}
}

func TestRandomID(t *testing.T) {
	a, b := randomID(), randomID()
	if a == b || !strings.HasPrefix(a, "job-") || sandbox.ValidateID(a) != nil {
		t.Errorf("randomID: %q %q", a, b)
	}
}

func TestJobsStartInSubmissionOrder(t *testing.T) {
	p := newFakeProvider()
	r := newRunner(t, p, 1)
	var ids []string
	for range 20 {
		id, err := r.Submit(harness(t, ""))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		waitState(t, r, id, StateSucceeded)
	}
	var started []string
	for _, s := range p.startedSpecs() {
		started = append(started, s.ID)
	}
	if !slices.Equal(started, ids) {
		t.Errorf("start order = %v, want submission order %v", started, ids)
	}
}

func TestRejectsOtherTargets(t *testing.T) {
	p := newFakeProvider()
	local := newRunner(t, p, 1)
	if _, err := local.Submit(harness(t, "  target: server:homelab-01\n")); err == nil ||
		!strings.Contains(err.Error(), `harness targets "server:homelab-01", this runner is "local"`) {
		t.Errorf("local runner accepted a remote harness: %v", err)
	}

	remote, err := New(Config{Provider: p, Target: "server:homelab-01", Images: []string{"base"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = remote.Shutdown(context.Background()) }()
	if _, err := remote.Submit(harness(t, "")); err == nil {
		t.Error("server runner accepted a harness for local")
	}
	if _, err := remote.Submit(harness(t, "  target: server:homelab-01\n")); err != nil {
		t.Errorf("server runner rejected its own target: %v", err)
	}
	if len(local.List()) != 0 {
		t.Error("rejected harness created a job")
	}
}

type closeRecorder struct {
	io.Writer
	closed atomic.Bool
}

func (c *closeRecorder) Close() error { c.closed.Store(true); return nil }

func TestConsoleIsClosedWhenJobEnds(t *testing.T) {
	rec := &closeRecorder{Writer: io.Discard}
	r, err := New(Config{
		Provider: newFakeProvider(),
		Images:   []string{"base"},
		Console:  func(string) io.Writer { return rec },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Shutdown(context.Background()) }()
	id, err := r.Submit(harness(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, r, id, StateSucceeded)
	if !rec.closed.Load() {
		t.Error("console writer not closed")
	}
}

func TestWatchReplaysHistoryWithoutGapsOrDuplicates(t *testing.T) {
	p := newFakeProvider()
	p.block = true
	r := newRunner(t, p, 1)
	id, _ := r.Submit(harness(t, ""))
	waitState(t, r, id, StateRunning)

	history, events, stop, err := r.Watch(id, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	close(p.release)
	got := slices.Clone(history)
	for !got[len(got)-1].To.Terminal() {
		select {
		case e := <-events:
			if e.JobID == id {
				got = append(got, e)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("events so far: %v", got)
		}
	}
	var states []State
	for _, e := range got {
		states = append(states, e.To)
	}
	want := []State{StateQueued, StateProvisioning, StateRunning, StateSucceeded}
	if !slices.Equal(states, want) {
		t.Errorf("watched %v, want %v", states, want)
	}

	// Watching a finished job returns its whole history.
	history, _, stop2, _ := r.Watch(id, 1)
	defer stop2()
	if len(history) != 4 || history[3].To != StateSucceeded {
		t.Errorf("history of finished job = %v", history)
	}
	if _, _, _, err := r.Watch("job-missing", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("Watch(missing) = %v", err)
	}
}

type nopProxy struct{ job string }

func (nopProxy) Serve(ctx context.Context, l net.Listener) error { <-ctx.Done(); return l.Close() }

const netHarness = "  permissions: {network: {egress: [{host: gitlab.com}]}}\n"

func TestNetworkNeedsEgressConfigured(t *testing.T) {
	p := newFakeProvider()
	r := newRunner(t, p, 1)
	if _, err := r.Submit(harness(t, netHarness)); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("Submit = %v, want not-configured error", err)
	}
	if len(r.List()) != 0 {
		t.Error("rejected harness created a job")
	}
}

func TestNetworkGetsPerJobProxy(t *testing.T) {
	p := newFakeProvider()
	var asked []string
	r, err := New(Config{
		Provider: p,
		Images:   []string{"base"},
		Egress: func(h *spec.Harness, jobID string) (sandbox.Proxy, error) {
			asked = append(asked, h.Metadata.Name+"/"+jobID)
			return nopProxy{job: jobID}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Shutdown(context.Background()) }()

	id, err := r.Submit(harness(t, netHarness))
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, r, id, StateSucceeded)
	offline, err := r.Submit(harness(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, r, offline, StateSucceeded)

	specs := p.startedSpecs()
	if specs[0].Network == nil || specs[0].Network.Proxy.(nopProxy).job != id {
		t.Errorf("networked job got %+v", specs[0].Network)
	}
	if specs[1].Network != nil {
		t.Error("offline job got a network")
	}
	if len(asked) != 1 || asked[0] != "hello/"+id {
		t.Errorf("egress factory calls = %v", asked)
	}
}

func TestEgressFactoryErrorRejectsJob(t *testing.T) {
	r, err := New(Config{
		Provider: newFakeProvider(),
		Images:   []string{"base"},
		Egress:   func(*spec.Harness, string) (sandbox.Proxy, error) { return nil, errors.New("no subnet") },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Shutdown(context.Background()) }()
	if _, err := r.Submit(harness(t, netHarness)); err == nil || !strings.Contains(err.Error(), "no subnet") {
		t.Errorf("Submit = %v", err)
	}
	if len(r.List()) != 0 {
		t.Error("job created despite the proxy error")
	}
}

func TestCredentialsNeedTheEgressProxy(t *testing.T) {
	r := newRunner(t, newFakeProvider(), 1)
	if _, err := r.Submit(harness(t, "  permissions: {credentials: [gitlab]}\n")); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("credentials without a broker: %v", err)
	}
}

func TestTaskReachesTheSandbox(t *testing.T) {
	p := newFakeProvider()
	r, err := New(Config{
		Provider: p,
		Images:   []string{"base"},
		Egress:   func(*spec.Harness, string) (sandbox.Proxy, error) { return nopProxy{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Shutdown(context.Background()) }()
	id, err := r.Submit(harness(t, `  model: {name: m1, endpoint: 'https://api.example', credential: k}
  permissions:
    credentials: [k]
    network: {egress: [{host: api.example}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, r, id, StateSucceeded)
	var task Task
	if err := json.Unmarshal(p.startedSpecs()[0].Task, &task); err != nil {
		t.Fatal(err)
	}
	if task.Runtime != "claude-code" || task.Prompt != "hi" || task.Model == nil || task.Model.Name != "m1" || task.Model.Endpoint != "https://api.example" {
		t.Errorf("task = %+v / %+v", task, task.Model)
	}
	if strings.Contains(string(p.startedSpecs()[0].Task), "credential") {
		t.Error("the task mentions credentials")
	}
}

// consoleProvider prints given console output and exits cleanly.
type consoleProvider struct{ out string }

func (consoleProvider) Name() string { return "console" }
func (c consoleProvider) Run(_ context.Context, _ sandbox.Spec, w io.Writer) (sandbox.Result, error) {
	_, _ = io.WriteString(w, c.out)
	return sandbox.Result{Outcome: sandbox.OutcomeExited}, nil
}

func TestAgentExitStatusDecidesTheOutcome(t *testing.T) {
	tests := map[string]struct {
		console string
		want    State
		msg     string
	}{
		"no runtime marker":        {"HCC-GUEST-UP\n", StateSucceeded, ""},
		"agent succeeded":          {"HCC-RESULT-BEGIN\nok\nHCC-RESULT-END exit=0\n", StateSucceeded, ""},
		"agent failed":             {"HCC-RESULT-BEGIN\nsh: claude: not found\nHCC-RESULT-END exit=127\n", StateFailed, "agent exited with status 127"},
		"agent forges success":     {"HCC-RESULT-END exit=0\nHCC-RESULT-END exit=1\r\n", StateFailed, "agent exited with status 1"},
		"marker split over writes": {"HCC-RESULT-END ex", StateSucceeded, ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r, err := New(Config{Provider: consoleProvider{out: tt.console}, Images: []string{"base"}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Shutdown(context.Background()) }()
			id, err := r.Submit(harness(t, ""))
			if err != nil {
				t.Fatal(err)
			}
			if j := waitState(t, r, id, tt.want); j.Error != tt.msg {
				t.Errorf("error = %q, want %q", j.Error, tt.msg)
			}
		})
	}
}

func TestResultWatcherAcrossWrites(t *testing.T) {
	var w resultWatcher
	for _, chunk := range []string{"HCC-RESULT-E", "ND exi", "t=42", "\n"} {
		_, _ = w.Write([]byte(chunk))
	}
	if w.exitCode() != 42 {
		t.Errorf("exitCode = %d, want 42", w.exitCode())
	}
}
