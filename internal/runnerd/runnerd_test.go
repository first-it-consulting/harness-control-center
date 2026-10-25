package runnerd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/runner"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
)

// testProvider prints to the console and exits, except for harnesses
// with memoryMiB 256, which run until cancelled.
type testProvider struct{}

func (testProvider) Name() string { return "test" }

func (testProvider) Run(ctx context.Context, s sandbox.Spec, console io.Writer) (sandbox.Result, error) {
	_, _ = io.WriteString(console, "guest says hello\n")
	if s.MemoryMiB == 256 {
		<-ctx.Done()
		return sandbox.Result{Outcome: sandbox.OutcomeKilled}, nil
	}
	return sandbox.Result{Outcome: sandbox.OutcomeExited}, nil
}

const harnessDoc = `apiVersion: hcc/v1alpha1
kind: Harness
metadata: {name: hello}
spec:
  runtime: claude-code
  prompt: hi
`

const blockingHarnessDoc = harnessDoc + "  sandbox: {memoryMiB: 256}\n"

// shortTempDir returns a directory with a path short enough for a Unix
// socket (macOS limits socket paths to 104 bytes).
func shortTempDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets")
	}
	d, err := os.MkdirTemp("/tmp", "hcc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

type testDaemon struct {
	socket string
	client *Client
	cancel context.CancelFunc
	done   chan error
	state  string
}

func startDaemon(t *testing.T) *testDaemon {
	t.Helper()
	dir := shortTempDir(t)
	state := filepath.Join(dir, "state")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := runner.New(runner.Config{
		Provider:      testProvider{},
		Images:        map[string]runner.Image{"base": {}},
		MaxConcurrent: 2,
		Console:       ConsoleFunc(state, log),
	})
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "run", "runner.sock")
	l, err := Listen(sock, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &testDaemon{socket: sock, client: NewUnixClient(sock), cancel: cancel, done: make(chan error, 1), state: state}
	go func() { d.done <- NewServer(r, state, log).Serve(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-d.done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("daemon did not shut down")
		}
	})
	return d
}

func TestSocketIsOwnerOnly(t *testing.T) {
	d := startDaemon(t)
	fi, err := os.Stat(d.socket)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %v, want socket 0600", fi.Mode())
	}
	di, _ := os.Stat(filepath.Dir(d.socket))
	if di.Mode().Perm() != 0o700 {
		t.Errorf("socket dir mode = %v, want 0700", di.Mode().Perm())
	}
}

func TestListenRefusesNonSocket(t *testing.T) {
	dir := shortTempDir(t)
	p := filepath.Join(dir, "runner.sock")
	if err := os.WriteFile(p, []byte("important"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(p, ""); err == nil {
		t.Fatal("Listen replaced a regular file")
	}
	if data, _ := os.ReadFile(p); string(data) != "important" {
		t.Error("file was modified")
	}
}

func TestSubmitFollowConsole(t *testing.T) {
	d := startDaemon(t)
	ctx := context.Background()
	id, err := d.client.Submit(ctx, []byte(harnessDoc))
	if err != nil {
		t.Fatal(err)
	}
	var seen []runner.State
	last, err := d.client.Follow(ctx, id, func(e runner.Event) bool { seen = append(seen, e.To); return true })
	if err != nil {
		t.Fatal(err)
	}
	if last.To != runner.StateSucceeded {
		t.Errorf("final state %s (seen %v)", last.To, seen)
	}
	var console bytes.Buffer
	if err := d.client.Console(ctx, id, &console); err != nil {
		t.Fatal(err)
	}
	if console.String() != "guest says hello\n" {
		t.Errorf("console = %q", console.String())
	}
	jobs, err := d.client.List(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].ID != id {
		t.Errorf("List = %v, %v", jobs, err)
	}
	j, err := d.client.Get(ctx, id)
	if err != nil || j.State != runner.StateSucceeded {
		t.Errorf("Get = %+v, %v", j, err)
	}
}

func TestFollowLateSubscriberSeesFinalState(t *testing.T) {
	d := startDaemon(t)
	ctx := context.Background()
	id, _ := d.client.Submit(ctx, []byte(harnessDoc))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if j, _ := d.client.Get(ctx, id); j.State.Terminal() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	var seen []runner.State
	last, err := d.client.Follow(ctx, id, func(e runner.Event) bool { seen = append(seen, e.To); return true })
	if err != nil || last.To != runner.StateSucceeded {
		t.Errorf("Follow after completion = %v, %v", last.To, err)
	}
	want := []runner.State{runner.StateQueued, runner.StateProvisioning, runner.StateRunning, runner.StateSucceeded}
	if !slices.Equal(seen, want) {
		t.Errorf("late follower saw %v, want the full history %v", seen, want)
	}
}

func TestCancelKillResume(t *testing.T) {
	d := startDaemon(t)
	ctx := context.Background()
	a, err := d.client.Submit(ctx, []byte(blockingHarnessDoc))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = d.client.Cancel(ctx, a)
	}()
	last, err := d.client.Follow(ctx, a, func(runner.Event) bool { return true })
	if err != nil || last.To != runner.StateCancelled {
		t.Fatalf("cancelled job ended %v, %v", last.To, err)
	}

	b, _ := d.client.Submit(ctx, []byte(blockingHarnessDoc))
	if err := d.client.KillAll(ctx); err != nil {
		t.Fatal(err)
	}
	if last, _ := d.client.Follow(ctx, b, func(runner.Event) bool { return true }); last.To != runner.StateCancelled {
		t.Errorf("job after kill switch: %v", last.To)
	}
	_, err = d.client.Submit(ctx, []byte(harnessDoc))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 503 {
		t.Fatalf("Submit after kill = %v, want 503", err)
	}
	if err := d.client.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.client.Submit(ctx, []byte(harnessDoc)); err != nil {
		t.Errorf("Submit after resume: %v", err)
	}
}

func TestAPIRejectsBadInput(t *testing.T) {
	d := startDaemon(t)
	ctx := context.Background()
	var apiErr *APIError
	_, err := d.client.Submit(ctx, []byte("kind: Pod\n"))
	if !errors.As(err, &apiErr) || apiErr.Status != 400 {
		t.Errorf("invalid harness: %v", err)
	}
	_, err = d.client.Submit(ctx, []byte(harnessDoc+"  permissions: {subagents: {max: 1}}\n"))
	if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Message, "not supported") {
		t.Errorf("unsupported harness: %v", err)
	}
	if _, err := d.client.Get(ctx, "job-missing"); !errors.As(err, &apiErr) || apiErr.Status != 404 {
		t.Errorf("missing job: %v", err)
	}
	// IDs are validated client-side and server-side (path traversal).
	if _, err := d.client.Get(ctx, "../etc"); err == nil {
		t.Error("client accepted a traversal id")
	}
	raw := newClient(func(ctx context.Context) (net.Conn, error) {
		var dd net.Dialer
		return dd.DialContext(ctx, "unix", d.socket)
	})
	if err := raw.do(ctx, "GET", "/v1/jobs/..%2F..%2Fetc/console", nil, nil); !errors.As(err, &apiErr) || apiErr.Status != 400 {
		t.Errorf("server accepted a traversal id: %v", err)
	}
	big := bytes.Repeat([]byte("a"), 2<<20)
	if _, err := d.client.Submit(ctx, big); !errors.As(err, &apiErr) || apiErr.Status != 413 {
		t.Errorf("oversized body: %v", err)
	}
}

func TestRelay(t *testing.T) {
	d := startDaemon(t)
	// A client whose connections go through Relay, like the ssh transport.
	c := newClient(func(ctx context.Context) (net.Conn, error) {
		local, remote := net.Pipe()
		go func() { _ = Relay(ctx, d.socket, remote, remote); _ = remote.Close() }()
		return local, nil
	})
	ctx := context.Background()
	id, err := c.Submit(ctx, []byte(harnessDoc))
	if err != nil {
		t.Fatal(err)
	}
	if last, err := c.Follow(ctx, id, func(runner.Event) bool { return true }); err != nil || last.To != runner.StateSucceeded {
		t.Errorf("Follow via relay = %v, %v", last.To, err)
	}
}

func TestNewSSHClientRejectsInjection(t *testing.T) {
	for _, host := range []string{"-oProxyCommand=touch /tmp/pwned", "", "a b", "host;id", "$(id)"} {
		if _, err := NewSSHClient(host, DefaultSocket); err == nil {
			t.Errorf("host %q accepted", host)
		}
	}
	for _, sock := range []string{"relative", "/run/hcc/x;id", "/run/$(id)", "/run/a b"} {
		if _, err := NewSSHClient("hcc-dev", sock); err == nil {
			t.Errorf("socket %q accepted", sock)
		}
	}
	if _, err := NewSSHClient("user@hcc-dev.example.com", DefaultSocket); err != nil {
		t.Errorf("valid host rejected: %v", err)
	}
}

func TestConsoleLogIsCapped(t *testing.T) {
	dir := t.TempDir()
	c, err := openConsoleLog(dir, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 4 {
				if n, err := c.Write(chunk); err != nil || n != len(chunk) {
					t.Errorf("Write = %d, %v", n, err)
				}
			}
		}()
	}
	wg.Wait()
	_ = c.Close()
	p, _ := consolePath(dir, "job-1")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(MaxConsoleBytes + len(truncatedMarker)); fi.Size() != want {
		t.Errorf("log size = %d, want %d", fi.Size(), want)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 { // no Unix modes on Windows
		t.Errorf("log mode = %v", fi.Mode().Perm())
	}
	if _, err := openConsoleLog(dir, "../escape"); err == nil {
		t.Error("traversal id accepted")
	}
	if _, err := openConsoleLog(dir, "job-1"); err == nil {
		t.Error("existing log reopened")
	}
}
