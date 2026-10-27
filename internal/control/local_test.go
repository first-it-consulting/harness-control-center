package control

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/local"
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
func fakeLocal(ts *testServer, up func(ctx context.Context, progress func(local.Step)) error) (socket string, started, release chan struct{}) {
	socket = filepath.Join(ts.dir, "local", "runner.sock")
	started, release = make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	ready := false
	ts.s.local = &localRunner{
		up: func(ctx context.Context, progress func(local.Step)) error {
			progress(local.Step{ID: "images", Title: "Download the VM images", State: "running", Bytes: 1 << 20})
			close(started)
			<-release
			err := up(ctx, progress)
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
	socket, started, release := fakeLocal(ts, func(_ context.Context, progress func(local.Step)) error {
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
	_, _, release := fakeLocal(ts, func(context.Context, func(local.Step)) error {
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
