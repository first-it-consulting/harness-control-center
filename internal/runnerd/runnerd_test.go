package runnerd

import (
	"archive/tar"
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

	"github.com/first-it-consulting/harness-control-center/internal/plugins"
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
	if s.Collect != nil {
		// Harnesses with 2 CPUs hand back a file; others leave the disk blank.
		disk := make([]byte, 4096)
		if s.CPUs == 2 {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			_ = tw.WriteHeader(&tar.Header{Name: "out.txt", Typeflag: tar.TypeReg, Size: 5, Mode: 0o600})
			_, _ = tw.Write([]byte("fib\n\n"))
			_ = tw.Close()
			disk = append(buf.Bytes(), disk...)
		}
		if err := s.Collect(bytes.NewReader(disk)); err != nil {
			return sandbox.Result{Outcome: sandbox.OutcomeFailed}, err
		}
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
	return startDaemonIn(t, shortTempDir(t))
}

// startDaemonIn starts a daemon whose state lives in dir, which may hold
// a previous daemon's state.
func startDaemonIn(t *testing.T, dir string) *testDaemon {
	t.Helper()
	state := filepath.Join(dir, "state")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := runner.New(runner.Config{
		Provider:      testProvider{},
		Images:        []string{"base"},
		MaxConcurrent: 2,
		Console:       ConsoleFunc(state, log),
		Results:       ResultsFunc(state),
		Store:         NewFileStore(state, log),
		ScheduleStore: NewFileStore(state, log),
		LastContact:   NewFileStore(state, log).LoadContact(),
		SaveContact:   NewFileStore(state, log).SaveContact,
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
	srv := NewServer(r, state, log)
	store := plugins.NewStore(filepath.Join(state, "plugins"))
	srv.EnablePlugins(store, plugins.NewManager(plugins.Config{Store: store}))
	go func() { d.done <- srv.Serve(ctx, l) }()
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
	_, err = d.client.Submit(ctx, []byte(harnessDoc+"  approvals: {requireFor: [shell.rm]}\n"))
	if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Message, "shell.rm") {
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

func TestResults(t *testing.T) {
	d := startDaemon(t)
	ctx := context.Background()
	run := func(doc string) string {
		t.Helper()
		id, err := d.client.Submit(ctx, []byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		if last, err := d.client.Follow(ctx, id, func(runner.Event) bool { return true }); err != nil || last.To != runner.StateSucceeded {
			t.Fatalf("job ended %s: %v", last.To, err)
		}
		return id
	}

	id := run(harnessDoc + "  sandbox: {cpus: 2, diskMiB: 256}\n")
	j, err := d.client.Get(ctx, id)
	if err != nil || j.Results == nil || j.Results.Files != 1 || j.Results.Bytes != 5 {
		t.Fatalf("job results = %+v, %v", j.Results, err)
	}
	var archive bytes.Buffer
	if err := d.client.Results(ctx, id, &archive); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&archive)
	h, err := tr.Next()
	if err != nil || h.Name != "out.txt" || h.Mode != 0o644 {
		t.Fatalf("archive entry = %+v, %v", h, err)
	}
	if fi, err := os.Stat(filepath.Join(d.state, "jobs", id, "results.tar")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("stored results: %v, %v", fi, err)
	}

	// A blank disk: nothing stored, and the API says so.
	empty := run(harnessDoc)
	err = d.client.Results(ctx, empty, io.Discard)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 || !strings.Contains(apiErr.Message, "no results") {
		t.Errorf("results of a blank disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d.state, "jobs", empty, "results.tar")); !os.IsNotExist(err) {
		t.Errorf("an empty archive was stored: %v", err)
	}
}

func TestResultsFuncKeepsNothingOnError(t *testing.T) {
	state := t.TempDir()
	store := ResultsFunc(state)
	if _, err := store("../escape", strings.NewReader(""), 1<<20); err == nil {
		t.Error("unsafe id accepted")
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "big", Typeflag: tar.TypeReg, Size: 100, Mode: 0o600})
	_, _ = tw.Write(make([]byte, 100))
	_ = tw.Close()
	if _, err := store("job-1", &buf, 50); err == nil {
		t.Error("oversized results accepted")
	}
	entries, _ := os.ReadDir(filepath.Join(state, "jobs", "job-1"))
	if len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
}

func TestFileStore(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.DiscardHandler)
	s := NewFileStore(dir, log)
	j := runner.Job{ID: "job-1", Harness: "h", State: runner.StateSucceeded, Created: time.Unix(1700000000, 0).UTC()}
	if err := s.Save(j); err != nil {
		t.Fatal(err)
	}
	j.Error = "updated"
	if err := s.Save(j); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "jobs", "job-1", "job.json")
	// Windows has no Unix modes; presence is still checked there.
	if fi, err := os.Stat(p); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Errorf("record = %v, %v", fi, err)
	}
	// Damaged, foreign and misnamed records are skipped.
	for name, content := range map[string]string{"job-2": "{not json", "job-3": `{"id":"job-1"}`} {
		_ = os.MkdirAll(filepath.Join(dir, "jobs", name), 0o700)
		_ = os.WriteFile(filepath.Join(dir, "jobs", name, "job.json"), []byte(content), 0o600)
	}
	_ = os.MkdirAll(filepath.Join(dir, "jobs", "Not_An_ID"), 0o700)
	jobs, err := s.Load()
	if err != nil || len(jobs) != 1 || jobs[0].Error != "updated" || !jobs[0].Created.Equal(j.Created) {
		t.Fatalf("Load = %+v, %v", jobs, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "jobs", "job-1")); len(entries) != 1 {
		t.Errorf("temporary files left: %v", entries)
	}
	if err := s.Save(runner.Job{ID: "../x"}); err == nil {
		t.Error("unsafe id saved")
	}
	if err := s.Delete("job-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(p)); !os.IsNotExist(err) {
		t.Errorf("job directory left: %v", err)
	}
}

func TestPruneOrphans(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(dir, slog.New(slog.DiscardHandler))
	old := filepath.Join(dir, "jobs", "job-old")
	_ = os.MkdirAll(old, 0o700)
	_ = os.WriteFile(filepath.Join(old, "console.log"), []byte("x"), 0o600)
	_ = os.MkdirAll(filepath.Join(dir, "jobs", "job-new"), 0o700)
	if err := s.Save(runner.Job{ID: "job-kept"}); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	for _, d := range []string{old, filepath.Join(dir, "jobs", "job-kept")} {
		_ = os.Chtimes(d, past, past)
	}
	pruned := s.PruneOrphans(24*time.Hour, time.Now())
	if !slices.Equal(pruned, []string{"job-old"}) {
		t.Errorf("pruned %v, want only the old directory without a record", pruned)
	}
}

func TestHistorySurvivesRestart(t *testing.T) {
	dir := shortTempDir(t)
	ctx := context.Background()
	d := startDaemonIn(t, dir)
	id, err := d.client.Submit(ctx, []byte(harnessDoc+"  sandbox: {cpus: 2, diskMiB: 256}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.client.Follow(ctx, id, func(runner.Event) bool { return true }); err != nil {
		t.Fatal(err)
	}
	// One job still running when the daemon stops.
	live, err := d.client.Submit(ctx, []byte(blockingHarnessDoc))
	if err != nil {
		t.Fatal(err)
	}
	d.cancel()
	if err := <-d.done; err != nil {
		t.Fatal(err)
	}
	d.done <- nil // for the first daemon's cleanup

	d2 := startDaemonIn(t, dir)
	j, err := d2.client.Get(ctx, id)
	if err != nil || j.State != runner.StateSucceeded || j.Results == nil || j.Results.Files != 1 {
		t.Fatalf("after restart: %+v, %v", j, err)
	}
	var console bytes.Buffer
	if err := d2.client.Console(ctx, id, &console); err != nil || console.String() != "guest says hello\n" {
		t.Errorf("console after restart = %q, %v", console.String(), err)
	}
	if err := d2.client.Results(ctx, id, io.Discard); err != nil {
		t.Errorf("results after restart: %v", err)
	}
	// Stopped on shutdown, so it ended cancelled, and is listed.
	l, err := d2.client.Get(ctx, live)
	if err != nil || !l.State.Terminal() {
		t.Errorf("job running at shutdown = %+v, %v", l, err)
	}
	if last, err := d2.client.Follow(ctx, id, func(runner.Event) bool { return true }); err != nil || last.To != runner.StateSucceeded {
		t.Errorf("Follow after restart = %v, %v", last, err)
	}
}

func TestInfo(t *testing.T) {
	d := startDaemon(t)
	st, err := d.client.Info(context.Background())
	if err != nil || st.Target != "local" || !slices.Equal(st.Images, []string{"base"}) || st.MaxConcurrent != 2 || st.Killed {
		t.Fatalf("Info = %+v, %v", st, err)
	}
	if err := d.client.KillAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.client.Info(context.Background()); !st.Killed {
		t.Error("kill switch not reported")
	}
}

func TestScheduleAPI(t *testing.T) {
	dir := shortTempDir(t)
	ctx := context.Background()
	d := startDaemonIn(t, dir)
	doc := []byte(harnessDoc + "  trigger: {schedule: '0 3 * * *'}\n")
	if _, err := d.client.Submit(ctx, doc); err == nil || !strings.Contains(err.Error(), "add it as a schedule") {
		t.Errorf("triggered harness as a job: %v", err)
	}
	sc, err := d.client.AddSchedule(ctx, doc)
	if err != nil || sc.Cron != "0 3 * * *" || sc.State != runner.ScheduleActive || sc.NextRun.Hour() != 3 {
		t.Fatalf("AddSchedule = %+v, %v", sc, err)
	}
	if _, err := d.client.AddSchedule(ctx, []byte(harnessDoc)); err == nil {
		t.Error("a harness without a trigger became a schedule")
	}
	if err := d.client.ScheduleAction(ctx, sc.ID, "pause"); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.client.GetSchedule(ctx, sc.ID); got.State != runner.SchedulePaused {
		t.Errorf("after pause: %+v", got)
	}
	if err := d.client.ScheduleAction(ctx, sc.ID, "resume"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(d.state, "schedules", sc.ID+".json")); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Errorf("stored schedule: %v %v", fi, err)
	}

	d.cancel()
	if err := <-d.done; err != nil {
		t.Fatal(err)
	}
	d.done <- nil
	d2 := startDaemonIn(t, dir)
	list, err := d2.client.Schedules(ctx)
	if err != nil || len(list) != 1 || list[0].ID != sc.ID || list[0].State != runner.ScheduleActive {
		t.Fatalf("after restart: %+v, %v (a shutdown must not pause schedules)", list, err)
	}
	if st, _ := d2.client.Info(ctx); st.Schedules != 1 {
		t.Errorf("info = %+v", st)
	}
	if err := d2.client.ScheduleAction(ctx, sc.ID, "delete"); err != nil {
		t.Fatal(err)
	}
	var apiErr *APIError
	if _, err := d2.client.GetSchedule(ctx, sc.ID); !errors.As(err, &apiErr) || apiErr.Status != 404 {
		t.Errorf("deleted schedule: %v", err)
	}
	if err := d2.client.ScheduleAction(ctx, sc.ID, "explode"); err == nil {
		t.Error("unknown action accepted")
	}
}

type nopProxy struct{}

func (nopProxy) Serve(ctx context.Context, l net.Listener) error { <-ctx.Done(); return l.Close() }

func TestApprovalAPI(t *testing.T) {
	dir := shortTempDir(t)
	log := slog.New(slog.DiscardHandler)
	approvers := make(chan runner.Approver, 1)
	r, err := runner.New(runner.Config{
		Provider: testProvider{}, Images: []string{"base"},
		Egress: func(ej runner.EgressJob) (sandbox.Proxy, error) {
			approvers <- ej.Approve
			return nopProxy{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "r.sock")
	l, err := Listen(sock, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- NewServer(r, filepath.Join(dir, "state"), log).Serve(ctx, l) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	c := NewUnixClient(sock)

	doc := blockingHarnessDoc + "  approvals: {requireFor: [http.delete]}\n  permissions: {network: {egress: [{host: example.com}]}}\n"
	id, err := c.Submit(ctx, []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	approve := <-approvers
	result := make(chan error, 1)
	go func() {
		result <- approve(ctx, runner.ApprovalRequest{Actions: []string{"http.delete"}, Method: "DELETE", Host: "example.com", Path: "/x"})
	}()
	var list []runner.Approval
	for range 500 {
		if list, err = c.Approvals(ctx); err == nil && len(list) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(list) != 1 || list[0].JobID != id || list[0].Method != "DELETE" {
		t.Fatalf("approvals = %+v, %v", list, err)
	}
	if j, _ := c.Get(ctx, id); j.State != runner.StateAwaitingApproval {
		t.Errorf("job state = %s", j.State)
	}
	if err := c.Decide(ctx, list[0].ID, false); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err == nil {
		t.Error("denied request went through")
	}
	var apiErr *APIError
	if err := c.Decide(ctx, list[0].ID, true); !errors.As(err, &apiErr) || apiErr.Status != 404 {
		t.Errorf("deciding again: %v", err)
	}
	// The body must say what to do.
	resp, err := c.http.Post(baseURL+"/v1/approvals/"+list[0].ID, "application/json", strings.NewReader(`{}`))
	if err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("decision without approve: %d", resp.StatusCode)
		}
	}
}

func TestEveryRequestIsContact(t *testing.T) {
	dir := shortTempDir(t)
	d := startDaemonIn(t, dir)
	ctx := context.Background()
	before, err := d.client.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := d.client.List(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := d.client.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !after.LastContact.After(before.LastContact) {
		t.Errorf("last contact %v, then %v", before.LastContact, after.LastContact)
	}
	// Persisted, for the next start.
	if got := NewFileStore(d.state, slog.New(slog.DiscardHandler)).LoadContact(); got.IsZero() {
		t.Error("contact not saved")
	}
}

func TestPluginInstallNeedsTheConfirmedChecksum(t *testing.T) {
	d := startDaemon(t)
	ctx := context.Background()
	dir := t.TempDir()
	manifest := "apiVersion: hcc/v1alpha1\nkind: Plugin\nmetadata: {name: echo, version: 1.0.0}\nspec:\n  type: notifier\n  command: [cat]\n  events: [job.ended]\n"
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, err := plugins.Pack(dir)
	if err != nil {
		t.Fatal(err)
	}
	pv, err := d.client.PreviewPlugin(ctx, bundle)
	if err != nil || pv.Name != "echo" || pv.Risk == "" {
		t.Fatalf("preview = %+v, %v", pv, err)
	}
	if list, _ := d.client.Plugins(ctx); len(list) != 0 {
		t.Fatal("a preview installed the plugin")
	}
	var apiErr *APIError
	if _, err := d.client.InstallPlugin(ctx, bundle, strings.Repeat("a", 64)); !errors.As(err, &apiErr) || apiErr.Status != 400 {
		t.Errorf("install with another checksum: %v", err)
	}
	if _, err := d.client.InstallPlugin(ctx, bundle, pv.SHA256); err != nil {
		t.Fatal(err)
	}
	list, err := d.client.Plugins(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "echo" || list[0].Status.Name != "echo" {
		t.Fatalf("plugins = %+v, %v", list, err)
	}
	if err := d.client.PluginLog(ctx, "echo", io.Discard); err == nil {
		t.Error("log of a plugin that never ran")
	}
	if err := d.client.RemovePlugin(ctx, "echo"); err != nil {
		t.Fatal(err)
	}
	if err := d.client.RemovePlugin(ctx, "echo"); !errors.As(err, &apiErr) || apiErr.Status != 404 {
		t.Errorf("second remove: %v", err)
	}
}
