package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/local"
	"github.com/first-it-consulting/harness-control-center/internal/version"
)

func TestWithLocalRunner(t *testing.T) {
	c := &Config{Runners: []RunnerRef{{Name: "server", SSH: "hcc-dev"}}}
	next, added, err := c.WithLocalRunner(filepath.Join(t.TempDir(), "Application Support", "runner.sock"))
	if err != nil || !added || len(next.Runners) != 2 || next.Runners[1].Name != LocalRunnerName || len(c.Runners) != 1 {
		t.Fatalf("%+v %v %v (original %+v)", next, added, err, c)
	}
	// Again: already there.
	if again, added, err := next.WithLocalRunner(next.Runners[1].Socket); err != nil || added || again != next {
		t.Errorf("added twice: %v %v", added, err)
	}
	// The name is taken by another runner.
	taken := &Config{Runners: []RunnerRef{{Name: LocalRunnerName, SSH: "elsewhere"}}}
	if _, _, err := taken.WithLocalRunner(filepath.Join(t.TempDir(), "runner.sock")); err == nil {
		t.Error("replaced another runner")
	}
	// The socket must be a clean absolute path.
	if _, _, err := (&Config{}).WithLocalRunner("relative/runner.sock"); err == nil {
		t.Error("accepted a relative socket")
	}
}

// fakeLocal replaces the Mac's runner setup in a test server.
// The setup reports its first step, then waits for release.
func fakeLocal(ts *testServer, up func(ctx context.Context, restart bool, progress func(local.Step)) error) (socket string, started, release chan struct{}) {
	socket = filepath.Join(ts.dir, "local", "runner.sock")
	started, release = make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	ready := false
	ts.s.local = &localRunner{
		up: func(ctx context.Context, restart bool, progress func(local.Step)) error {
			progress(local.Step{ID: "images", Title: "Download the VM images", State: "running", Bytes: 1 << 20})
			close(started)
			<-release
			err := up(ctx, restart, progress)
			mu.Lock()
			ready = err == nil
			mu.Unlock()
			return err
		},
		status: func(context.Context) local.Status {
			mu.Lock()
			defer mu.Unlock()
			return local.Status{Images: ready, Config: ready, Service: ready, Answering: ready, Socket: socket}
		},
		stop: func(context.Context) error { return nil },
	}
	return socket, started, release
}

func waitLocal(t *testing.T, ts *testServer) localView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var v localView
		if code := ts.do(http.MethodGet, "/api/local", nil, &v); code != http.StatusOK {
			t.Fatalf("GET /api/local: %d", code)
		}
		if v.Setup.Finished {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("setup did not finish: %+v", v)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLocalSetupThroughTheUI(t *testing.T) {
	ts := newTestServer(t)
	socket, started, release := fakeLocal(ts, func(_ context.Context, _ bool, progress func(local.Step)) error {
		progress(local.Step{ID: "images", Title: "Download the VM images", State: "done"})
		progress(local.Step{ID: "service", Title: "Start the runner", State: "done"})
		return nil
	})
	var v localView
	ts.do(http.MethodGet, "/api/local", nil, &v)
	if v.Ready || v.Registered || v.Setup.Running {
		t.Fatalf("before: %+v", v)
	}
	if code := ts.do(http.MethodPost, "/api/local/setup", nil, nil); code != http.StatusAccepted {
		t.Fatalf("setup: %d", code)
	}
	// While it runs: its progress, and no second setup.
	<-started
	ts.do(http.MethodGet, "/api/local", nil, &v)
	if !v.Setup.Running || len(v.Setup.Steps) != 1 || v.Setup.Steps[0].Bytes != 1<<20 {
		t.Errorf("running: %+v", v.Setup)
	}
	if code := ts.do(http.MethodPost, "/api/local/setup", nil, nil); code != http.StatusConflict {
		t.Errorf("second setup: %d", code)
	}
	close(release)
	v = waitLocal(t, ts)
	if !v.Ready || !v.Registered || v.Setup.Error != "" || len(v.Setup.Steps) != 2 || v.Setup.Steps[0].State != "done" {
		t.Fatalf("after: %+v", v)
	}
	// Saved, so the runner stays listed.
	cfg, err := LoadConfig(ts.s.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := cfg.Runner(LocalRunnerName); !ok || r.Socket != socket {
		t.Errorf("config: %+v", cfg)
	}
}

func TestLocalSetupFailureIsReported(t *testing.T) {
	ts := newTestServer(t)
	_, _, release := fakeLocal(ts, func(context.Context, bool, func(local.Step)) error {
		return errors.New("Download the VM images: CHECKSUM MISMATCH")
	})
	ts.do(http.MethodPost, "/api/local/setup", nil, nil)
	close(release)
	v := waitLocal(t, ts)
	if !strings.Contains(v.Setup.Error, "CHECKSUM MISMATCH") || v.Registered {
		t.Fatalf("%+v", v)
	}
	if _, ok := ts.s.cfg.Runner(LocalRunnerName); ok {
		t.Error("a failed setup was added")
	}
}

func TestLocalNeedsASession(t *testing.T) {
	ts := newTestServer(t)
	ts.session = ""
	for _, p := range []string{"/api/local/setup", "/api/local/stop"} {
		if rec := ts.raw(http.MethodPost, p, "", map[string]string{"Origin": "http://" + host}); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session: %d", p, rec.Code)
		}
	}
}

// The Mac's runner runs the Control Center's own hcc: when it runs an
// older version, its card offers an update, which is the setup with a
// restart (refused while jobs run).
func TestLocalRunnerUpdate(t *testing.T) {
	orig := version.Version
	version.Version = "v1.0.0" // what the runner reports
	t.Cleanup(func() { version.Version = orig })
	ts := newTestServer(t)
	sock := startRunner(t)
	restarts := make(chan bool, 4)
	release := make(chan struct{})
	ts.s.local = &localRunner{
		up: func(_ context.Context, restart bool, progress func(local.Step)) error {
			progress(local.Step{ID: "service", Title: "Start the runner", State: "running"})
			restarts <- restart
			<-release
			return nil
		},
		status: func(context.Context) local.Status { return local.Status{Answering: true, Socket: sock} },
		stop:   func(context.Context) error { return nil },
		socket: sock, version: "v1.0.0",
	}
	if code := ts.do(http.MethodPost, "/api/runners", RunnerRef{Name: "mac", Socket: sock}, nil); code != http.StatusCreated {
		t.Fatalf("add: %d", code)
	}
	view := func() RunnerView {
		var v RunnerView
		ts.do(http.MethodGet, "/api/runners/mac", nil, &v)
		return v
	}
	// Same version: nothing to offer, and the setup does not restart.
	if v := view(); v.LocalUpdate != "" {
		t.Errorf("same version offers %q", v.LocalUpdate)
	}
	if code := ts.do(http.MethodPost, "/api/local/setup", nil, nil); code != http.StatusAccepted {
		t.Fatalf("setup: %d", code)
	}
	if <-restarts {
		t.Error("restarted a runner of the same version")
	}
	if v := view(); v.Installing == nil || v.Installing.Step != "Start the runner" {
		t.Errorf("card during the setup: %+v", v.Installing)
	}
	release <- struct{}{}
	waitLocal(t, ts)

	// This Control Center is newer: the card offers it, the setup restarts.
	ts.s.local.version = "v1.1.0"
	var all []RunnerView
	if ts.do(http.MethodGet, "/api/runners", nil, &all); len(all) != 1 || all[0].LocalUpdate != "v1.1.0" {
		t.Errorf("list: %+v", all)
	}
	if code := ts.do(http.MethodPost, "/api/local/setup", nil, nil); code != http.StatusAccepted {
		t.Fatalf("update: %d", code)
	}
	if !<-restarts {
		t.Error("an outdated runner was not restarted")
	}
	release <- struct{}{}
	waitLocal(t, ts)

	// With a job running, the restart is refused.
	long := strings.Replace(okHarness, "prompt: hi", "prompt: hi\n  sandbox: {memoryMiB: 256}", 1)
	if code := ts.do(http.MethodPost, "/api/runs", runRequest{Runner: "mac", YAML: long, Confirmed: true}, nil); code != http.StatusCreated {
		t.Fatalf("run: %d", code)
	}
	var out map[string]any
	if code := ts.do(http.MethodPost, "/api/local/setup", nil, &out); code != http.StatusConflict || !strings.Contains(fmt.Sprint(out), "jobs running") {
		t.Errorf("update with a job: %d %v", code, out)
	}
	select {
	case <-restarts:
		t.Error("the setup ran with a job running")
	default:
	}

	// Another runner (not on the local socket) never gets the offer.
	if code := ts.do(http.MethodPost, "/api/runners", RunnerRef{Name: "srv", SSH: "host"}, nil); code != http.StatusCreated {
		t.Fatalf("add srv: %d", code)
	}
	ts.do(http.MethodGet, "/api/runners", nil, &all)
	for _, v := range all {
		if v.Name == "srv" && v.LocalUpdate != "" {
			t.Errorf("srv: %+v", v)
		}
	}
}
