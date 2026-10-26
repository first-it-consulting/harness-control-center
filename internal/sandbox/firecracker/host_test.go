package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestHelperHostRejectsUnsafeIDs(t *testing.T) {
	helper := &HelperHost{HelperPath: "/nonexistent"}
	for _, id := range []string{"", "..", "../x", "a/b", "-rf"} {
		if err := helper.RemoveJail(context.Background(), id); err == nil {
			t.Errorf("remove(%q) accepted", id)
		}
		if err := helper.KillJail(context.Background(), id); err == nil {
			t.Errorf("kill(%q) accepted", id)
		}
		if _, err := helper.StartVM(StartRequest{ID: id}, os.Stdout); err == nil {
			t.Errorf("start(%q) accepted", id)
		}
	}
}

// HelperHost passes exactly the request fields to the helper and streams
// its output; SIGTERM reaches it.
func TestHelperHostCallFormat(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell")
	}
	helperPath := filepath.Join(t.TempDir(), "hcc-helper")
	script := "#!/bin/sh\necho \"helper $*\"\n[ \"$1\" = start ] && exec sleep 30\nexit 0\n"
	if err := os.WriteFile(helperPath, []byte(script), 0o700); err != nil { //nolint:gosec // test script must be executable
		t.Fatal(err)
	}
	h := &HelperHost{HelperPath: helperPath}
	var out lockedBuffer
	p, err := h.StartVM(StartRequest{ID: "run-1", Image: "base", Slot: 1, CPUs: 2, MemoryMiB: 256, Tools: true}, &out)
	if err != nil {
		t.Fatal(err)
	}
	// Wait for the helper to run (a fresh executable can start slowly,
	// e.g. under macOS Gatekeeper) instead of guessing a delay.
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(out.String(), "helper start"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("helper produced no output")
		}
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.Wait() }()
	select {
	case err := <-done:
		_ = err // SIGTERM makes Wait return "signal: terminated"
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not stop on SIGTERM")
	}
	if want := "helper start --id run-1 --image base --slot 1 --cpus 2 --memory 256 --disk 0 --tools"; !strings.Contains(out.String(), want) {
		t.Errorf("helper got %q, want %q", out.String(), want)
	}
	if err := h.KillJail(context.Background(), "run-1"); err != nil {
		t.Errorf("KillJail: %v", err)
	}
	if err := h.RemoveJail(context.Background(), "run-1"); err != nil {
		t.Errorf("RemoveJail: %v", err)
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
