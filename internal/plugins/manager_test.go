package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/runner"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
)

type fakeRunner struct {
	ch    chan runner.Event
	mu    sync.Mutex
	sched []runner.Schedule
}

func (f *fakeRunner) Subscribe(int) (<-chan runner.Event, func()) { return f.ch, func() {} }
func (f *fakeRunner) Schedules() []runner.Schedule {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sched
}

type recProvider struct {
	mu    sync.Mutex
	specs []sandbox.Spec
	exit  string
}

func (p *recProvider) Name() string { return "rec" }
func (p *recProvider) Run(_ context.Context, s sandbox.Spec, console io.Writer) (sandbox.Result, error) {
	p.mu.Lock()
	p.specs = append(p.specs, s)
	exit := p.exit
	p.mu.Unlock()
	_, _ = io.WriteString(console, "HCC-RESULT-END exit="+exit+"\n")
	return sandbox.Result{Outcome: sandbox.OutcomeExited}, nil
}

func (p *recProvider) runs() []sandbox.Spec {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]sandbox.Spec(nil), p.specs...)
}

func installNotifier(t *testing.T, s *Store, m string) {
	t.Helper()
	b, err := Pack(pluginDir(t, map[string]string{ManifestName: m, "notify.sh": "#!/bin/sh\ncat\n"}))
	if err != nil {
		t.Fatal(err)
	}
	_, _, pv, err := Check(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Install(bytes.NewReader(b), pv.SHA256); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestNotifierGetsItsEvents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes")
	}
	store := NewStore(filepath.Join(t.TempDir(), "plugins"))
	installNotifier(t, store, strings.Replace(manifest, "events: [job.ended]", "events: [job.ended, schedule.paused]", 1))
	prov := &recProvider{exit: "0"}
	var egressFor []string
	m := NewManager(Config{Store: store, Provider: prov, Images: []string{"claude-code"}, Target: "server:r1", ScheduleCheck: 50 * time.Millisecond,
		Egress: func(ej runner.EgressJob) (sandbox.Proxy, error) {
			egressFor = append(egressFor, ej.Harness.Spec.Runtime+":"+ej.Harness.Spec.Permissions.Network.Egress[0].Host)
			return nil, nil
		}})
	fr := &fakeRunner{ch: make(chan runner.Event, 10)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { m.Run(ctx, fr); close(done) }()

	now := time.Now()
	fr.ch <- runner.Event{JobID: "job-1", Harness: "h", From: runner.StateQueued, To: runner.StateRunning, At: now}           // not an event
	fr.ch <- runner.Event{JobID: "job-1", Harness: "h", From: runner.StateRunning, To: runner.StateAwaitingApproval, At: now} // not subscribed
	fr.ch <- runner.Event{JobID: "job-1", Harness: "h", From: runner.StateRunning, To: runner.StateFailed, At: now, Message: "boom"}
	waitFor(t, "a notifier run", func() bool { return len(prov.runs()) == 1 })
	s := prov.runs()[0]
	var task pluginTask
	if err := json.Unmarshal(s.Task, &task); err != nil {
		t.Fatal(err)
	}
	if task.Runtime != "plugin" || task.Plugin.Name != "webhook" || task.Plugin.Command[0] != "./notify.sh" || len(task.Events) != 1 {
		t.Fatalf("task = %+v", task)
	}
	if e := task.Events[0]; e.Type != "job.ended" || e.Job != "job-1" || e.State != "failed" || e.Message != "boom" || e.Runner != "server:r1" {
		t.Errorf("event = %+v", e)
	}
	if len(s.Bundle) == 0 || s.Network == nil || s.MemoryMiB != 512 || !strings.HasPrefix(s.ID, "plg-") {
		t.Errorf("sandbox = %+v", s)
	}
	if len(egressFor) != 1 || egressFor[0] != "plugin:hooks.example.com" {
		t.Errorf("egress built for %v", egressFor)
	}
	waitFor(t, "status", func() bool { st := m.Statuses(); return len(st) == 1 && st[0].LastResult == "ok" })

	// A paused schedule is an event too; a failing run is reported.
	prov.mu.Lock()
	prov.exit = "3"
	prov.mu.Unlock()
	fr.mu.Lock()
	fr.sched = []runner.Schedule{{ID: "sch-1", Harness: "nightly", State: runner.SchedulePaused, Message: "paused: nobody contacted"}}
	fr.mu.Unlock()
	waitFor(t, "a second run", func() bool { return len(prov.runs()) == 2 })
	_ = json.Unmarshal(prov.runs()[1].Task, &task)
	if len(task.Events) != 1 || task.Events[0].Type != "schedule.paused" || task.Events[0].Schedule != "sch-1" {
		t.Errorf("events = %+v", task.Events)
	}
	waitFor(t, "failure status", func() bool {
		st := m.Statuses()
		return len(st) == 1 && strings.Contains(st[0].LastResult, "status 3")
	})
	cancel()
	<-done
}
