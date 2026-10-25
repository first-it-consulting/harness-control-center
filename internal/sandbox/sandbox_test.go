package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validSpec(t *testing.T) Spec {
	t.Helper()
	dir := t.TempDir()
	k, r := filepath.Join(dir, "vmlinux"), filepath.Join(dir, "rootfs.ext4")
	for _, p := range []string{k, r} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return Spec{ID: "run-1", Kernel: k, RootFS: r, CPUs: 1, MemoryMiB: 128, Timeout: time.Minute}
}

func TestValidateID(t *testing.T) {
	for _, id := range []string{"a", "run-1", "0abc", strings.Repeat("a", 63)} {
		if err := ValidateID(id); err != nil {
			t.Errorf("ValidateID(%q) = %v", id, err)
		}
	}
	for _, id := range []string{"", "-a", "A", "a_b", "a/b", "..", "../etc", "a b", "a\x00", strings.Repeat("a", 64)} {
		if ValidateID(id) == nil {
			t.Errorf("ValidateID(%q) accepted", id)
		}
	}
}

func TestSpecValidate(t *testing.T) {
	if s := validSpec(t); s.Validate() != nil {
		t.Fatalf("valid spec rejected: %v", s.Validate())
	}
	tests := []struct {
		name    string
		mutate  func(*Spec)
		wantErr string
	}{
		{"traversal id", func(s *Spec) { s.ID = "../../etc" }, "invalid id"},
		{"relative kernel", func(s *Spec) { s.Kernel = "vmlinux" }, "absolute"},
		{"missing rootfs", func(s *Spec) { s.RootFS = "/nonexistent/rootfs" }, "rootfs"},
		{"directory as kernel", func(s *Spec) { s.Kernel = filepath.Dir(s.Kernel) }, "regular file"},
		{"no cpus", func(s *Spec) { s.CPUs = 0 }, "cpus"},
		{"too much memory", func(s *Spec) { s.MemoryMiB = MaxMemoryMiB + 1 }, "memory"},
		{"no timeout", func(s *Spec) { s.Timeout = 0 }, "timeout"},
		{"timeout too long", func(s *Spec) { s.Timeout = 25 * time.Hour }, "timeout"},
		{"init override", func(s *Spec) { s.BootArgs = []string{"init=/bin/sh"} }, "controlled by the provider"},
		{"rw override", func(s *Spec) { s.BootArgs = []string{"rw"} }, "controlled by the provider"},
		{"console override", func(s *Spec) { s.BootArgs = []string{"console=tty0"} }, "controlled by the provider"},
		{"arg injection", func(s *Spec) { s.BootArgs = []string{"a=b init=/bin/sh"} }, "invalid boot argument"},
		{"quote", func(s *Spec) { s.BootArgs = []string{`a="b"`} }, "invalid boot argument"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := validSpec(t)
			tt.mutate(&s)
			err := s.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestSpecValidateAllowsHarmlessBootArgs(t *testing.T) {
	s := validSpec(t)
	s.BootArgs = []string{"hcc.mode=hang", "nokaslr", "hcc.url=a.example.com:443/x"}
	if err := s.Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}
}

func TestOutcomeString(t *testing.T) {
	want := map[Outcome]string{OutcomeExited: "exited", OutcomeTimedOut: "timed_out", OutcomeKilled: "killed", OutcomeFailed: "failed"}
	for o, s := range want {
		if o.String() != s {
			t.Errorf("%d.String() = %q, want %q", o, o.String(), s)
		}
	}
}
