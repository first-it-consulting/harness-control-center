package main

import (
	"archive/tar"
	"bytes"
	"context"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/runner"
	"github.com/first-it-consulting/harness-control-center/internal/runnerd"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
)

type cliProvider struct{}

func (cliProvider) Name() string { return "test" }

// Run exits at once, except for harnesses with memoryMiB 256, which run
// until stopped.
func (cliProvider) Run(ctx context.Context, s sandbox.Spec, console io.Writer) (sandbox.Result, error) {
	_, _ = io.WriteString(console, "HCC-GUEST-UP\n\x1b]52;c;cm0gLXJmIH4=\x07\n")
	if s.MemoryMiB == 256 {
		<-ctx.Done()
		return sandbox.Result{Outcome: sandbox.OutcomeKilled}, nil
	}
	if s.Collect != nil {
		// One file, and a symlink whose name tries to drive the terminal.
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		_ = tw.WriteHeader(&tar.Header{Name: "fib.txt", Typeflag: tar.TypeReg, Size: 3, Mode: 0o644})
		_, _ = tw.Write([]byte("1 1"))
		_ = tw.WriteHeader(&tar.Header{Name: "evil\x1b]0;pwned\x07", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
		_ = tw.Close()
		if err := s.Collect(&buf); err != nil {
			return sandbox.Result{Outcome: sandbox.OutcomeFailed}, err
		}
	}
	return sandbox.Result{Outcome: sandbox.OutcomeExited}, nil
}

// startTestRunner runs a daemon with a fake provider and returns its socket.
func startTestRunner(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets")
	}
	dir, err := os.MkdirTemp("/tmp", "hcc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := runner.New(runner.Config{
		Provider: cliProvider{},
		Images:   []string{"base"},
		Console:  runnerd.ConsoleFunc(filepath.Join(dir, "state"), log),
		Results:  runnerd.ResultsFunc(filepath.Join(dir, "state")),
	})
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "runner.sock")
	l, err := runnerd.Listen(sock, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = runnerd.NewServer(r, filepath.Join(dir, "state"), log).Serve(ctx, l); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("runner did not stop")
		}
	})
	return sock
}

func TestRunWithYes(t *testing.T) {
	sock := startTestRunner(t)
	code, out, errOut := runCLI("run", "../../examples/hello-offline.yaml", "--yes", "--console", "--socket", sock)
	if code != exitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"Risk: low", "Started job job-", "running", "succeeded after", "--- console ---", "HCC-GUEST-UP"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	code, out, _ = runCLI("jobs", "--socket", sock)
	if code != exitOK || !strings.Contains(out, "hello-offline") || !strings.Contains(out, "succeeded") {
		t.Errorf("jobs: %d\n%s", code, out)
	}
}

func TestRunAsksForConfirmation(t *testing.T) {
	sock := startTestRunner(t)
	code, out, _ := runCLIWithInput("n\n", "run", "--socket", sock, "../../examples/hello-offline.yaml")
	if code != exitOK || !strings.Contains(out, "Start this harness on this computer? [y/N]") || !strings.Contains(out, "Not started.") {
		t.Errorf("declined: %d\n%s", code, out)
	}
	if _, jobs, _ := runCLI("jobs", "--socket", sock); !strings.Contains(jobs, "No jobs.") {
		t.Errorf("declined run created a job:\n%s", jobs)
	}
	code, out, _ = runCLIWithInput("y\n", "run", "--socket", sock, "../../examples/hello-offline.yaml")
	if code != exitOK || !strings.Contains(out, "succeeded") {
		t.Errorf("accepted: %d\n%s", code, out)
	}
}

func TestRunWithoutTerminalNeedsYes(t *testing.T) {
	sock := startTestRunner(t)
	f, err := os.Open("../../examples/hello-offline.yaml") // a regular file, not a terminal
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out, errOut strings.Builder
	code := run([]string{"run", "--socket", sock, "../../examples/hello-offline.yaml"}, f, &out, &errOut)
	if code != exitUsage || !strings.Contains(errOut.String(), "pass --yes") {
		t.Errorf("exit %d, stderr %q", code, errOut.String())
	}
}

func TestRunWithDevNullNeedsYes(t *testing.T) {
	sock := startTestRunner(t)
	devnull, err := os.Open(os.DevNull) // a character device, but not a terminal
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	var out, errOut strings.Builder
	code := run([]string{"run", "--socket", sock, "../../examples/hello-offline.yaml"}, devnull, &out, &errOut)
	if code != exitUsage || !strings.Contains(errOut.String(), "pass --yes") || strings.Contains(out.String(), "[y/N]") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
}

func TestRunRejectedByRunner(t *testing.T) {
	sock := startTestRunner(t)
	// Needs network, which this runner cannot enforce yet.
	code, _, errOut := runCLI("run", "--yes", "--socket", sock, "../../examples/nightly-dependency-bump.yaml")
	if code != exitInvalid || !strings.Contains(errOut, "not started") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

func TestKillResumeLogs(t *testing.T) {
	sock := startTestRunner(t)
	if code, out, _ := runCLI("kill", "--socket", sock); code != exitOK || !strings.Contains(out, "Kill switch engaged") {
		t.Errorf("kill: %d %q", code, out)
	}
	if code, _, errOut := runCLI("run", "--yes", "--socket", sock, "../../examples/hello-offline.yaml"); code != exitInvalid || !strings.Contains(errOut, "kill switch") {
		t.Errorf("run while killed: %d %q", code, errOut)
	}
	if code, out, _ := runCLI("resume", "--socket", sock); code != exitOK || !strings.Contains(out, "accepts jobs again") {
		t.Errorf("resume: %d %q", code, out)
	}
	if code, _, errOut := runCLI("logs", "--socket", sock, "../escape"); code != exitInvalid || !strings.Contains(errOut, "invalid id") {
		t.Errorf("logs with bad id: %d %q", code, errOut)
	}
}

func TestUnreachableRunner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets")
	}
	code, _, errOut := runCLI("jobs", "--socket", "/nonexistent/runner.sock")
	if code != exitInvalid || !strings.Contains(errOut, "runner unreachable") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

func TestSSHHostInjectionRejected(t *testing.T) {
	code, _, errOut := runCLI("jobs", "--ssh", "-oProxyCommand=touch /tmp/x")
	if code != exitUsage || !strings.Contains(errOut, "invalid ssh host") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

func TestServeRefusesBadConfig(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	code, _, errOut := runCLI("runner", "serve", "--config", filepath.Join(t.TempDir(), "missing.yaml"))
	if code != exitInvalid || !strings.Contains(errOut, "runner stopped") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

func TestParseFlagsInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "")
	sock := fs.String("socket", "", "")
	pos, err := parseFlags(fs, []string{"a.yaml", "--yes", "--socket", "/s", "b"})
	if err != nil || !*yes || *sock != "/s" || !slices.Equal(pos, []string{"a.yaml", "b"}) {
		t.Errorf("pos=%v yes=%v sock=%q err=%v", pos, *yes, *sock, err)
	}
}

func TestConsoleEscapesAreNeutralized(t *testing.T) {
	sock := startTestRunner(t)
	code, out, _ := runCLI("run", "--yes", "--console", "--socket", sock, "../../examples/hello-offline.yaml")
	if code != exitOK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if strings.Contains(out, "\x1b") || !strings.Contains(out, "^[]52;c;cm0gLXJmIH4=^G") {
		t.Errorf("guest escape sequence reached the terminal raw:\n%q", out)
	}
	id := out[strings.Index(out, "job-") : strings.Index(out, "job-")+20]
	_, logs, _ := runCLI("logs", "--socket", sock, id)
	if strings.Contains(logs, "\x1b") {
		t.Errorf("hcc logs printed a raw escape: %q", logs)
	}
	_, rawLogs, _ := runCLI("logs", "--raw", "--socket", sock, id)
	if !strings.Contains(rawLogs, "\x1b]52;") {
		t.Errorf("hcc logs --raw altered the output: %q", rawLogs)
	}
}

func TestResultsCommand(t *testing.T) {
	sock := startTestRunner(t)
	code, out, errOut := runCLI("run", "../../examples/hello-offline.yaml", "--yes", "--socket", sock)
	if code != exitOK {
		t.Fatalf("run: exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "Results: 1 files, 3 bytes.") || !strings.Contains(out, "hcc results job-") {
		t.Errorf("run output lacks the results:\n%s", out)
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("an agent-chosen name reached the terminal raw:\n%q", out)
	}
	_, id, _ := strings.Cut(out, "Started job ")
	id, _, _ = strings.Cut(id, " ")

	dst := filepath.Join(t.TempDir(), "out.tar")
	if code, out, errOut := runCLI("results", id, "-o", dst, "--socket", sock); code != exitOK {
		t.Fatalf("results: exit %d\n%s\n%s", code, out, errOut)
	}
	f, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h, err := tar.NewReader(f).Next()
	if err != nil || h.Name != "fib.txt" {
		t.Errorf("archive = %+v, %v", h, err)
	}
	// Never overwrites.
	if code, _, errOut := runCLI("results", id, "-o", dst, "--socket", sock); code == exitOK || !strings.Contains(errOut, "exists") {
		t.Errorf("overwrote an existing file: %d %s", code, errOut)
	}
	if code, _, _ := runCLI("results", id, "--socket", sock); code != exitUsage {
		t.Errorf("results without -o: exit %d", code)
	}
	if code, _, _ := runCLI("results", "job-missing", "-o", filepath.Join(t.TempDir(), "x.tar"), "--socket", sock); code != exitInvalid {
		t.Errorf("results of an unknown job: exit %d", code)
	}
}
