package runnerd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const goodConfig = `
target: server:hcc-dev
maxConcurrent: 2
runnerUser: hcc-runner
socket: /run/hcc/runner.sock
socketGroup: hcc
stateDir: /var/lib/hcc/state
firecracker:
  helper: /usr/local/lib/hcc/hcc-helper
  jailer: /usr/local/lib/hcc/jailer
  firecracker: /usr/local/lib/hcc/firecracker
  chrootBase: /var/lib/hcc/jails
  imageRoot: /var/lib/hcc/images
  uid: 64000
  gid: 64000
images:
  base:
    kernel: /var/lib/hcc/images/base/vmlinux
    rootfs: /var/lib/hcc/images/base/rootfs.ext4
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "runner.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths")
	}
	c, err := LoadConfig(writeConfig(t, goodConfig))
	if err != nil {
		t.Fatal(err)
	}
	if c.Target != "server:hcc-dev" || c.MaxConcurrent != 2 || c.Images["base"].Kernel != "/var/lib/hcc/images/base/vmlinux" {
		t.Errorf("config = %+v", c)
	}

	minimal := strings.Replace(goodConfig, "target: server:hcc-dev\nmaxConcurrent: 2\n", "", 1)
	minimal = strings.Replace(minimal, "socket: /run/hcc/runner.sock\nsocketGroup: hcc\n", "", 1)
	c, err = LoadConfig(writeConfig(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.Target != "local" || c.MaxConcurrent != 1 || c.Socket != DefaultSocket {
		t.Errorf("defaults = %+v", c)
	}
}

func TestLoadConfigRejects(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths")
	}
	tests := map[string]struct{ old, new, want string }{
		"unknown field":       {"maxConcurrent: 2", "maxConcurent: 2", "not found"},
		"bad target":          {"server:hcc-dev", "cloud", "target"},
		"relative path":       {"/var/lib/hcc/jails", "jails", "chrootBase"},
		"unclean path":        {"/var/lib/hcc/state", "/var/lib/hcc/../state", "stateDir"},
		"image outside root":  {"/var/lib/hcc/images/base/rootfs.ext4", "/etc/shadow", "inside firecracker.imageRoot"},
		"root uid":            {"uid: 64000", "uid: 0", "non-root"},
		"too many concurrent": {"maxConcurrent: 2", "maxConcurrent: 1000", "maxConcurrent"},
		"root runner":         {"runnerUser: hcc-runner", "runnerUser: root", "runnerUser"},
		"no runner user":      {"runnerUser: hcc-runner\n", "", "runnerUser"},
		"root socket group":   {"socketGroup: hcc", "socketGroup: root", "socketGroup"},
		"relative helper":     {"/usr/local/lib/hcc/hcc-helper", "hcc-helper", "firecracker.helper"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := LoadConfig(writeConfig(t, strings.Replace(goodConfig, tt.old, tt.new, 1)))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
	if _, err := LoadConfig(writeConfig(t, "target: local\nrunnerUser: hcc-runner\n")); err == nil || !strings.Contains(err.Error(), "at least one image") {
		t.Errorf("empty config: %v", err)
	}
}

func TestCheckFilesRequiresRootOwnedBinaries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX ownership")
	}
	// A binary in the user's temp dir is not root-owned (unless the test
	// runs as root), so the daemon must refuse it.
	if os.Getuid() == 0 {
		t.Skip("running as root")
	}
	bin := filepath.Join(t.TempDir(), "jailer")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Config{Firecracker: FirecrackerConfig{Jailer: bin, Firecracker: bin}}
	if err := c.CheckFiles(); err == nil || !strings.Contains(err.Error(), "owned by root") {
		t.Errorf("CheckFiles = %v, want owned-by-root error", err)
	}
	// /bin/sh's path is root-owned and not group/other-writable everywhere
	// we run.
	c = &Config{Firecracker: FirecrackerConfig{Jailer: "/bin/sh", Firecracker: "/bin/sh"}}
	if err := c.CheckFiles(); err != nil {
		t.Errorf("CheckFiles(/bin/sh) = %v", err)
	}
}
