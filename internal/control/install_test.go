package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedCommand struct {
	name  string
	args  []string
	env   []string
	dir   string
	stdin string
}

const fakeRemoteDir = "/home/admin/hcc-install.Ab12Cd34Ef"

type fakeInstallCommands struct {
	mu       sync.Mutex
	commands []recordedCommand
	failOn   string
}

func (f *fakeInstallCommands) run(_ context.Context, dir, name string, args, env []string, stdin []byte, out io.Writer) error {
	f.mu.Lock()
	f.commands = append(f.commands, recordedCommand{name: name, args: append([]string(nil), args...), env: append([]string(nil), env...), dir: dir, stdin: string(stdin)})
	f.mu.Unlock()
	switch {
	case name == "ssh" && strings.Contains(strings.Join(args, " "), "mktemp"):
		_, _ = fmt.Fprintln(out, fakeRemoteDir)
	case name == "git" && args[0] == "describe":
		_, _ = fmt.Fprintln(out, "v0.1.1-3-gabcdef0")
	case name == "git":
		_, _ = fmt.Fprintln(out, "abcdef0")
	default:
		_, _ = fmt.Fprintf(out, "%s finished\n", name)
	}
	if name == f.failOn {
		return errors.New("fake failure")
	}
	return nil
}

func (f *fakeInstallCommands) all() []recordedCommand {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedCommand(nil), f.commands...)
}

func waitRunnerInstall(t *testing.T, ts *testServer, name string) runnerInstallState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, ok := ts.s.installs.status(name)
		if ok && st.Finished {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("installation did not finish: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunnerInstallThroughTheUI(t *testing.T) {
	ts := newTestServer(t)
	fake := &fakeInstallCommands{}
	ts.s.installs.findSource = func() (string, error) { return "/src/hcc", nil }
	ts.s.installs.runCommand = fake.run
	var out map[string]any
	if code := ts.do(http.MethodPost, "/api/runners", RunnerRef{Name: "agent", SSH: "openclaw-agent-01"}, &out); code != http.StatusCreated {
		t.Fatalf("add runner: %d %v", code, out)
	}
	body := map[string]any{"target": "server:openclaw-agent-01", "confirmed": true, "sudoPassword": "s3cret"}
	rec := ts.rawJSON(http.MethodPost, "/api/runners/agent/install", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start install: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "s3cret") {
		t.Fatalf("the password came back: %s", rec.Body.String())
	}
	st := waitRunnerInstall(t, ts, "agent")
	if st.Error != "" || st.Target != "server:openclaw-agent-01" || len(st.Log) == 0 {
		t.Fatalf("state = %+v", st)
	}
	for _, line := range st.Log {
		if strings.Contains(line, "s3cret") {
			t.Fatalf("the password is in the log: %q", line)
		}
	}
	var builds, scp, installSSH int
	for _, c := range fake.all() {
		switch {
		case c.name == "go" && len(c.args) > 0 && c.args[0] == "build":
			builds++
			if c.dir != "/src/hcc" {
				t.Errorf("go ran in %q", c.dir)
			}
			// A Linux build, whatever this computer is.
			if env := strings.Join(c.env, " "); env != "GOOS=linux GOARCH=amd64 CGO_ENABLED=0" {
				t.Errorf("go build env %q", env)
			}
			if c.args[len(c.args)-1] == "./cmd/hcc" && !strings.Contains(strings.Join(c.args, " "), "Version=v0.1.1-3-gabcdef0") {
				t.Errorf("no version flags: %v", c.args)
			}
		case c.name == "scp":
			scp++
			if !strings.Contains(strings.Join(c.args, " "), "sudo-askpass") {
				t.Errorf("the askpass helper is not uploaded: %v", c.args)
			}
			if c.args[len(c.args)-1] != "openclaw-agent-01:"+fakeRemoteDir+"/" {
				t.Errorf("scp destination %q", c.args[len(c.args)-1])
			}
		case c.name == "ssh" && strings.Contains(strings.Join(c.args, " "), "install-runner.sh"):
			installSSH++
			if c.stdin != "s3cret\n" {
				t.Errorf("install stdin %q", c.stdin)
			}
			script := strings.Join(c.args, " ")
			if !strings.Contains(script, "server:openclaw-agent-01") {
				t.Errorf("target missing from remote script: %v", c.args)
			}
			// With a password, install-runner.sh's own sudo calls ask the
			// helper: sudo's cached password would not reach them.
			for _, want := range []string{"SUDO_ASKPASS=" + fakeRemoteDir + "/sudo-askpass", "export HCC_SUDO_PASSWORD", `rm -rf ` + fakeRemoteDir} {
				if !strings.Contains(script, want) {
					t.Errorf("remote script lacks %q: %s", want, script)
				}
			}
		}
		if strings.Contains(strings.Join(c.args, "\x00"), "s3cret") {
			t.Errorf("the password became a command argument: %#v", c.args)
		}
	}
	if builds != 3 || scp != 1 || installSSH != 1 {
		t.Errorf("commands: builds=%d scp=%d installSSH=%d (%v)", builds, scp, installSSH, fake.all())
	}

	var got runnerInstallState
	if code := ts.do(http.MethodGet, "/api/runners/agent/install", nil, &got); code != http.StatusOK || !got.Finished {
		t.Errorf("status: %d %+v", code, got)
	}
}

// unreachable is activeJobs for a runner that is not installed yet.
func unreachable(context.Context, string) (int, error) { return 0, errors.New("connection refused") }

func TestRunnerInstallValidationAndFailure(t *testing.T) {
	ts := newTestServer(t)
	var out map[string]any
	ts.do(http.MethodPost, "/api/runners", RunnerRef{Name: "ssh-runner", SSH: "host"}, &out)
	for _, tc := range []struct {
		name string
		body map[string]any
		code int
	}{
		{"not confirmed", map[string]any{}, http.StatusBadRequest},
		{"bad target", map[string]any{"target": "local", "confirmed": true}, http.StatusBadRequest},
		{"bad password", map[string]any{"confirmed": true, "sudoPassword": "two\nlines"}, http.StatusBadRequest},
	} {
		if code := ts.do(http.MethodPost, "/api/runners/ssh-runner/install", tc.body, &out); code != tc.code {
			t.Errorf("%s: %d, want %d (%v)", tc.name, code, tc.code, out)
		}
	}

	fake := &fakeInstallCommands{failOn: "go"}
	src := t.TempDir() // not in the callback: it runs in the background
	ts.s.installs.findSource = func() (string, error) { return src, nil }
	ts.s.installs.runCommand = fake.run
	if code := ts.do(http.MethodPost, "/api/runners/ssh-runner/install", map[string]any{"confirmed": true}, &out); code != http.StatusAccepted {
		t.Fatalf("start: %d %v", code, out)
	}
	st := waitRunnerInstall(t, ts, "ssh-runner")
	if st.Error == "" || !strings.Contains(st.Error, "build ./cmd/hcc") || st.Target != "server:ssh-runner" {
		t.Errorf("failure state = %+v", st)
	}

	if code := ts.do(http.MethodPost, "/api/runners/ssh-runner/install", map[string]any{"confirmed": true}, &out); code != http.StatusAccepted {
		t.Fatalf("after failure, a retry must be possible: %d %v", code, out)
	}
	waitRunnerInstall(t, ts, "ssh-runner") // nothing may run on after the test
}

func TestRunnerInstallOnlyOverSSH(t *testing.T) {
	inst := newRunnerInstaller()
	if err := inst.start(RunnerRef{Name: "mac", Socket: "/tmp/hcc.sock"}, "", nil); err == nil || !strings.Contains(err.Error(), "over ssh") {
		t.Fatalf("socket runner: %v", err)
	}
	if err := inst.start(RunnerRef{Name: "bad", SSH: "-oProxyCommand=x"}, "", nil); err == nil {
		t.Fatal("an invalid ssh host was accepted")
	}
}

func TestRunnerInstallPasswordIsCleared(t *testing.T) {
	release := make(chan struct{})
	inst := newRunnerInstaller()
	src := t.TempDir()
	inst.findSource = func() (string, error) { return src, nil }
	inst.runCommand = func(_ context.Context, _, name string, args, _ []string, _ []byte, out io.Writer) error {
		if name == "ssh" && strings.Contains(strings.Join(args, " "), "mktemp") {
			_, _ = fmt.Fprintln(out, fakeRemoteDir)
		}
		<-release
		return nil
	}
	password := []byte("secret")
	if err := inst.start(RunnerRef{Name: "agent", SSH: "host"}, "", password); err != nil {
		t.Fatal(err)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for !inst.mustStatus(t, "agent").Finished && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !inst.mustStatus(t, "agent").Finished {
		t.Fatal("installation did not finish")
	}
	if !bytes.Equal(password, make([]byte, len(password))) {
		t.Errorf("password was not cleared: %q", password)
	}
}

func (m *runnerInstaller) mustStatus(t *testing.T, name string) runnerInstallState {
	t.Helper()
	st, ok := m.status(name)
	if !ok {
		t.Fatalf("no state for %s", name)
	}
	return st
}

func TestFindRunnerSource(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{filepath.Join("deploy", "images")} {
		if err := os.MkdirAll(filepath.Join(root, p), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"go.mod", "Makefile", filepath.Join("deploy", "install-runner.sh"), filepath.Join("deploy", "hcc-runner.service"), filepath.Join("deploy", "sudo-askpass")} {
		if err := os.WriteFile(filepath.Join(root, p), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HCC_SOURCE_DIR", root)
	got, err := findRunnerSource()
	if err != nil || got != filepath.Clean(root) {
		t.Fatalf("%q, %v", got, err)
	}
	t.Setenv("HCC_SOURCE_DIR", filepath.Join(root, "missing"))
	if _, err := findRunnerSource(); err == nil || !strings.Contains(err.Error(), "HCC_SOURCE_DIR") {
		t.Errorf("invalid HCC_SOURCE_DIR: %v", err)
	}
	// A checkout in the working directory is not used: its installer
	// would run with sudo on the server.
	t.Setenv("HCC_SOURCE_DIR", "")
	t.Chdir(root)
	if got, err := findRunnerSource(); err == nil {
		t.Errorf("used the working directory's checkout %q", got)
	}
}

func TestRunnerInstallRefusesAnOddRemoteDir(t *testing.T) {
	ts := newTestServer(t)
	var out map[string]any
	ts.do(http.MethodPost, "/api/runners", RunnerRef{Name: "agent", SSH: "host"}, &out)
	ts.s.installs.findSource = func() (string, error) { return "/src/hcc", nil }
	ts.s.installs.runCommand = func(_ context.Context, _, name string, args, _ []string, _ []byte, w io.Writer) error {
		if name == "ssh" && strings.Contains(strings.Join(args, " "), "mktemp") {
			_, _ = fmt.Fprintln(w, "/tmp/x; reboot")
		}
		return nil
	}
	ts.do(http.MethodPost, "/api/runners/agent/install", map[string]any{"confirmed": true}, &out)
	if st := waitRunnerInstall(t, ts, "agent"); !strings.Contains(st.Error, "unexpected install directory") {
		t.Errorf("state = %+v", st)
	}
}

// An update restarts the runner, so it waits until no job runs there.
func TestRunnerUpdateWaitsForJobs(t *testing.T) {
	ts := newTestServer(t)
	fake := &fakeInstallCommands{}
	ts.s.installs.findSource = func() (string, error) { return "/src/hcc", nil }
	ts.s.installs.runCommand = fake.run
	jobs := 2
	ts.s.installs.activeJobs = func(_ context.Context, name string) (int, error) {
		if name != "agent" {
			t.Errorf("asked %q", name)
		}
		return jobs, nil
	}
	var out map[string]any
	ts.do(http.MethodPost, "/api/runners", RunnerRef{Name: "agent", SSH: "host"}, &out)
	body := map[string]any{"target": "server:agent", "confirmed": true}
	if code := ts.do(http.MethodPost, "/api/runners/agent/install", body, &out); code != http.StatusConflict || !strings.Contains(fmt.Sprint(out), "2 jobs") {
		t.Fatalf("with jobs: %d %v", code, out)
	}
	if got := fake.all(); len(got) != 0 {
		t.Fatalf("ran with jobs on the runner: %v", got)
	}
	if _, ok := ts.s.installs.status("agent"); ok {
		t.Error("a refused update has a state")
	}
	jobs = 0
	if code := ts.do(http.MethodPost, "/api/runners/agent/install", body, &out); code != http.StatusAccepted {
		t.Fatalf("idle: %d %v", code, out)
	}
	if st := waitRunnerInstall(t, ts, "agent"); st.Error != "" || !strings.Contains(strings.Join(st.Log, "\n"), "v0.1.1-3-gabcdef0") {
		t.Errorf("state = %+v", st)
	}
}

func TestRunnerVersion(t *testing.T) {
	ts := newTestServer(t)
	fake := &fakeInstallCommands{}
	ts.s.installs.findSource = func() (string, error) { return "/src/hcc", nil }
	ts.s.installs.runCommand = fake.run
	for range 2 {
		var v installVersion
		if code := ts.do(http.MethodGet, "/api/runner-version", nil, &v); code != http.StatusOK || v.Version != "v0.1.1-3-gabcdef0" || v.Error != "" {
			t.Fatalf("version: %d %+v", code, v)
		}
	}
	if n := len(fake.all()); n != 1 {
		t.Errorf("git ran %d times; the version is remembered", n)
	}

	ts = newTestServer(t)
	ts.s.installs.findSource = func() (string, error) { return "", errors.New("needs the hcc source checkout") }
	var v installVersion
	if ts.do(http.MethodGet, "/api/runner-version", nil, &v); v.Version != "" || !strings.Contains(v.Error, "source checkout") {
		t.Errorf("without a checkout: %+v", v)
	}
}

func TestActiveJobs(t *testing.T) {
	ts := newTestServer(t)
	sock := startRunner(t)
	var out map[string]any
	ts.do(http.MethodPost, "/api/runners", RunnerRef{Name: "r", Socket: sock}, &out)
	if n, err := ts.s.activeJobs(context.Background(), "r"); n != 0 || err != nil {
		t.Errorf("idle runner: %d %v", n, err)
	}
	if _, err := ts.s.activeJobs(context.Background(), "missing"); err == nil {
		t.Error("an unknown runner has jobs")
	}
}
