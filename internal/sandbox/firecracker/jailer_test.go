package firecracker

import (
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func testJailer(t *testing.T) JailerConfig {
	t.Helper()
	d := t.TempDir()
	return JailerConfig{
		JailerPath: filepath.Join(d, "jailer"), FirecrackerPath: filepath.Join(d, "firecracker"),
		ChrootBase: filepath.Join(d, "jails"), UID: 64000, GID: 64000, Identities: 2,
		ForbiddenIDs: []int{0, 1000},
	}
}

func TestJailerConfigValidate(t *testing.T) {
	if c := testJailer(t); c.Validate() != nil {
		t.Fatalf("good config rejected: %v", c.Validate())
	}
	tests := map[string]func(*JailerConfig){
		"root uid":            func(c *JailerConfig) { c.UID = 0 },
		"root gid":            func(c *JailerConfig) { c.GID = 0 },
		"relative jailer":     func(c *JailerConfig) { c.JailerPath = "jailer" },
		"unclean base":        func(c *JailerConfig) { c.ChrootBase += "/../etc" },
		"no identities":       func(c *JailerConfig) { c.Identities = 0 },
		"too many identities": func(c *JailerConfig) { c.Identities = MaxIdentities + 1 },
		"overflow":            func(c *JailerConfig) { c.UID = math.MaxInt32 },
		"covers runner uid":   func(c *JailerConfig) { c.UID = 999 },
		"covers runner gid":   func(c *JailerConfig) { c.GID = 1000 },
	}
	for name, mutate := range tests {
		c := testJailer(t)
		mutate(&c)
		if c.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestJailerArgvUsesConfigNotRequest(t *testing.T) {
	c := testJailer(t)
	argv, err := c.Argv(StartRequest{ID: "run-1", Slot: 1, CPUs: 2, MemoryMiB: 256})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"--id": "run-1", "--uid": "64001", "--gid": "64001", "--exec-file": c.FirecrackerPath, "--chroot-base-dir": c.ChrootBase}
	for flag, v := range want {
		if got := argValue(argv, flag); got != v {
			t.Errorf("%s = %q, want %q", flag, got, v)
		}
	}
	if argv[0] != c.JailerPath || !slices.Contains(argv, "memory.max=335544320") || !slices.Contains(argv, "cpu.max=200000 100000") {
		t.Errorf("argv = %v", argv)
	}
}

func TestJailerArgvRejectsOutOfBoundsRequests(t *testing.T) {
	c := testJailer(t)
	bad := map[string]StartRequest{
		"traversal id":  {ID: "../x", Slot: 0, CPUs: 1, MemoryMiB: 128},
		"option id":     {ID: "-uid", Slot: 0, CPUs: 1, MemoryMiB: 128},
		"negative slot": {ID: "a", Slot: -1, CPUs: 1, MemoryMiB: 128},
		"slot too high": {ID: "a", Slot: 2, CPUs: 1, MemoryMiB: 128},
		"no cpus":       {ID: "a", Slot: 0, CPUs: 0, MemoryMiB: 128},
		"too much ram":  {ID: "a", Slot: 0, CPUs: 1, MemoryMiB: 1 << 20},
	}
	for name, req := range bad {
		if argv, err := c.Argv(req); err == nil {
			t.Errorf("%s: accepted: %s", name, strings.Join(argv, " "))
		}
	}
}
