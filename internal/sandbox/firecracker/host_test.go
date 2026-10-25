package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests run SudoHost without a prefix (no root), against
// directories in a temp dir standing in for the chroot base and cgroupfs.
func newTestHost(t *testing.T) *SudoHost {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("SudoHost uses POSIX tools")
	}
	dir := t.TempDir()
	return &SudoHost{ChrootBase: filepath.Join(dir, "jails"), CgroupRoot: filepath.Join(dir, "cgroup")}
}

func TestRemoveJail(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses GNU rm --one-file-system; Firecracker hosts are always Linux")
	}
	h := newTestHost(t)
	jail := jailDir(h.ChrootBase, "run-1")
	cg := h.cgroupDir("run-1")
	sibling := jailDir(h.ChrootBase, "run-2")
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
	// Idempotent: nothing left to remove is fine.
	if err := h.RemoveJail(context.Background(), "run-1"); err != nil {
		t.Errorf("second RemoveJail: %v", err)
	}
}

func TestHostRejectsUnsafeIDs(t *testing.T) {
	h := newTestHost(t)
	for _, id := range []string{"", "..", "../x", "a/b", "-rf"} {
		if err := h.RemoveJail(context.Background(), id); err == nil {
			t.Errorf("RemoveJail(%q) accepted", id)
		}
		if err := h.KillJail(context.Background(), id); err == nil {
			t.Errorf("KillJail(%q) accepted", id)
		}
	}
}

func TestKillJailWritesCgroupKill(t *testing.T) {
	h := newTestHost(t)
	cg := h.cgroupDir("run-1")
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

func TestStartJailerStreamsConsoleAndSignals(t *testing.T) {
	h := newTestHost(t)
	var out strings.Builder
	p, err := h.StartJailer([]string{"sh", "-c", "echo console-line; exec sleep 30"}, &out)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := p.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.Wait() }()
	select {
	case err := <-done:
		_ = err // SIGTERM makes Wait return "signal: terminated"
	case <-time.After(5 * time.Second):
		t.Fatal("process did not stop on SIGTERM")
	}
	if !strings.Contains(out.String(), "console-line") {
		t.Errorf("console = %q", out.String())
	}
	if _, err := h.StartJailer(nil, &out); err == nil {
		t.Error("empty argv accepted")
	}
}
