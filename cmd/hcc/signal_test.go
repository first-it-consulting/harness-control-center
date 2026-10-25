//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Ctrl-C during "hcc run" must stop the job on the runner, not just the
// CLI, and the CLI must wait until the runner confirms.
func TestRunCtrlCStopsTheJob(t *testing.T) {
	sock := startTestRunner(t)
	h := filepath.Join(t.TempDir(), "forever.yaml")
	doc := "apiVersion: hcc/v1alpha1\nkind: Harness\nmetadata: {name: forever}\nspec:\n  runtime: claude-code\n  prompt: run\n  sandbox: {memoryMiB: 256}\n"
	if err := os.WriteFile(h, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"run", "--yes", "--socket", sock, h}, strings.NewReader(""), &out, &errOut)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "running") {
		if time.Now().After(deadline) {
			t.Fatalf("job never ran:\n%s\n%s", out.String(), errOut.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != exitJob {
			t.Errorf("exit %d, want %d", code, exitJob)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("hcc run did not return after Ctrl-C")
	}
	for _, want := range []string{"Stopping the job", "cancelled"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
	if _, jobs, _ := runCLI("jobs", "--socket", sock); !strings.Contains(jobs, "cancelled") {
		t.Errorf("runner does not show the job as cancelled:\n%s", jobs)
	}
}
