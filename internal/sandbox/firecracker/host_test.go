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

// These tests run the hosts without sudo (no prefix), against temp
// directories standing in for the chroot base and cgroupfs.
func newTestHost(t *testing.T) *SudoHost {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX tools")
	}
	dir := t.TempDir()
	return &SudoHost{Jailer: JailerConfig{ChrootBase: filepath.Join(dir, "jails")}, CgroupRoot: filepath.Join(dir, "cgroup")}
}

func TestRemoveJail(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses GNU rm --one-file-system; Firecracker hosts are always Linux")
	}
	h := newTestHost(t)
	jail := JailDir(h.Jailer.ChrootBase, "run-1")
	cg := CgroupDir(h.CgroupRoot, "run-1")
	sibling := JailDir(h.Jailer.ChrootBase, "run-2")
	for _, d := range []string{filepath.Join(jail, "root", "dev"), cg, sibling} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.RemoveJail(context.Background(), "run-1"); err != nil {
		t.Fatalf("RemoveJail: %v", err)
	}
	for _, gone := range []string{jail, cg} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s still exists", gone)
		}
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("sibling jail removed: %v", err)
	}
	if err := h.RemoveJail(context.Background(), "run-1"); err != nil {
		t.Errorf("second RemoveJail: %v", err)
	}
}

func TestHostsRejectUnsafeIDs(t *testing.T) {
	h := newTestHost(t)
	helper := &HelperHost{HelperPath: "/nonexistent"}
	for _, id := range []string{"", "..", "../x", "a/b", "-rf"} {
		for name, err := range map[string]error{
			"sudo remove":   h.RemoveJail(context.Background(), id),
			"sudo kill":     h.KillJail(context.Background(), id),
			"helper remove": helper.RemoveJail(context.Background(), id),
			"helper kill":   helper.KillJail(context.Background(), id),
		} {
			if err == nil {
				t.Errorf("%s(%q) accepted", name, id)
			}
		}
		if _, err := helper.StartVM(StartRequest{ID: id}, os.Stdout); err == nil {
			t.Errorf("helper start(%q) accepted", id)
		}
	}
}

func TestKillJailWritesCgroupKill(t *testing.T) {
	h := newTestHost(t)
	cg := CgroupDir(h.CgroupRoot, "run-1")
	if err := os.MkdirAll(cg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := h.KillJail(context.Background(), "run-1"); err != nil {
		t.Fatalf("KillJail: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(cg, "cgroup.kill"))
	if err != nil || strings.TrimSpace(string(data)) != "1" {
		t.Errorf("cgroup.kill = %q, %v; want 1", data, err)
	}
	if err := h.KillJail(context.Background(), "gone"); err != nil {
		t.Errorf("KillJail on missing cgroup: %v", err)
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
	p, err := h.StartVM(StartRequest{ID: "run-1", Slot: 1, CPUs: 2, MemoryMiB: 256}, &out)
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
	if want := "helper start --id run-1 --slot 1 --cpus 2 --memory 256"; !strings.Contains(out.String(), want) {
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
