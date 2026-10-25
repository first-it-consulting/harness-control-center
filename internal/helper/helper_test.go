//go:build unix

package helper

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/first-it-consulting/harness-control-center/internal/sandbox/firecracker"
)

type recorder struct {
	execArgv [][]string
	execEnv  [][]string
	runArgv  [][]string
	free     uint64 // reported free space; 0 means plenty
}

func (r *recorder) deps() Deps {
	return Deps{
		Exec: func(_ string, argv, env []string) error {
			r.execArgv = append(r.execArgv, argv)
			r.execEnv = append(r.execEnv, env)
			return nil
		},
		Run: func(_ context.Context, argv ...string) error {
			r.runArgv = append(r.runArgv, argv)
			return os.RemoveAll(argv[len(argv)-1]) // behave like rm -rf
		},
		FreeBytes: func(string) (uint64, error) {
			if r.free == 0 {
				return 1 << 40, nil
			}
			return r.free, nil
		},
	}
}

// env stands in for a host: in tests the current user plays both root
// (owner of the jail tree, up to the trust anchor) and the runner.
type env struct {
	cfg             Config
	anchor          string
	base, stage, cg string
	rec             *recorder
	me              int
	start           []string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	// Resolve symlinks (macOS: /var -> /private/var) so the temp dir
	// itself does not trip the no-symlink rules.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // stands in for a root-owned 0755 directory
		t.Fatal(err)
	}
	me := os.Getuid()
	e := &env{
		anchor: dir, base: filepath.Join(dir, "jails"), stage: filepath.Join(dir, "staging"),
		cg: filepath.Join(dir, "cgroup"), rec: &recorder{}, me: me,
		start: []string{"start", "--id", "job-1", "--slot", "1", "--cpus", "2", "--memory", "256"},
	}
	for _, d := range []string{e.base, e.stage} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	e.cfg = Config{
		Jailer: firecracker.JailerConfig{
			JailerPath: "/usr/local/lib/hcc/jailer", FirecrackerPath: "/usr/local/lib/hcc/firecracker",
			ChrootBase: e.base, UID: 64000, GID: 64000, Identities: 2,
			ForbiddenIDs: []int{0, me},
		},
		StagingBase: e.stage,
		CgroupRoot:  e.cg,
		RunnerUID:   me,
		RootUID:     me,
		TrustAnchor: dir,
	}
	return e
}

// stageJob1 writes job-1's files like the provider does.
func (e *env) stageJob1(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(e.stage, "job-1")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range stagedFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("content of "+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func (e *env) call(args ...string) (int, string) {
	var stderr bytes.Buffer
	code := Main(e.cfg, args, e.me, e.rec.deps(), &stderr)
	return code, stderr.String()
}

func TestStartBuildsJailAndExecsJailer(t *testing.T) {
	old := syscall.Umask(0o077) // the daemon's umask must not matter
	defer syscall.Umask(old)
	e := newEnv(t)
	e.stageJob1(t)
	if code, stderr := e.call(e.start...); code != 0 {
		t.Fatalf("start failed: %s", stderr)
	}
	root := filepath.Join(firecracker.JailDir(e.base, "job-1"), "root")
	if fi, err := os.Stat(root); err != nil || fi.Mode().Perm() != 0o711 {
		t.Errorf("jail root = %v, %v; want mode 0711", fi, err)
	}
	for _, name := range stagedFiles {
		p := filepath.Join(root, name)
		data, err := os.ReadFile(p)
		if err != nil || string(data) != "content of "+name {
			t.Errorf("%s = %q, %v", name, data, err)
		}
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o444 {
			t.Errorf("%s mode = %v, want 0444", name, fi.Mode().Perm())
		}
	}
	if len(e.rec.execArgv) != 1 {
		t.Fatalf("exec calls = %d", len(e.rec.execArgv))
	}
	argv := e.rec.execArgv[0]
	if argv[0] != "/usr/local/lib/hcc/jailer" || !slices.Contains(argv, "/usr/local/lib/hcc/firecracker") {
		t.Errorf("argv not from config: %v", argv)
	}
	if i := slices.Index(argv, "--uid"); argv[i+1] != "64001" {
		t.Errorf("uid = %s, want 64001 (slot 1)", argv[i+1])
	}
	if got := e.rec.execEnv[0]; len(got) != 1 || !strings.HasPrefix(got[0], "PATH=") {
		t.Errorf("jailer environment = %v, want only PATH", got)
	}
	// A jail is never reused.
	if code, _ := e.call(e.start...); code == 0 {
		t.Error("second start reused the jail")
	}
}

func TestRefusesOtherCallers(t *testing.T) {
	e := newEnv(t)
	e.stageJob1(t)
	var stderr bytes.Buffer
	code := Main(e.cfg, e.start, e.me+1, e.rec.deps(), &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "not the runner") || len(e.rec.execArgv) != 0 {
		t.Errorf("foreign caller: code %d, %q", code, stderr.String())
	}
}

func TestRejectsBadArguments(t *testing.T) {
	e := newEnv(t)
	e.stageJob1(t)
	tests := map[string][]string{
		"no operation":       {},
		"unknown operation":  {"chown", "--id", "job-1"},
		"traversal id":       {"remove", "--id", "../../etc"},
		"option as id":       {"kill", "--id", "-rf"},
		"extra argument":     {"kill", "--id", "job-1", "/etc"},
		"slot out of range":  {"start", "--id", "job-1", "--slot", "2", "--cpus", "1", "--memory", "128"},
		"negative slot":      {"start", "--id", "job-1", "--slot", "-1", "--cpus", "1", "--memory", "128"},
		"too many cpus":      {"start", "--id", "job-1", "--slot", "0", "--cpus", "999", "--memory", "128"},
		"caller chooses uid": {"start", "--id", "job-1", "--slot", "0", "--cpus", "1", "--memory", "128", "--uid", "0"},
		"caller chooses bin": {"start", "--id", "job-1", "--slot", "0", "--cpus", "1", "--memory", "128", "--exec-file", "/bin/sh"},
		"nothing staged":     {"start", "--id", "job-2", "--slot", "0", "--cpus", "1", "--memory", "128"},
	}
	for name, args := range tests {
		if code, _ := e.call(args...); code == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(e.rec.execArgv) != 0 || len(e.rec.runArgv) != 0 {
		t.Errorf("rejected calls had effects: exec %v run %v", e.rec.execArgv, e.rec.runArgv)
	}
}

// The runner owns its staging directory, so a compromised runner may try
// to make root copy (and so expose to it) files it cannot read itself.
func TestRefusesHostileStaging(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("root only"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(t *testing.T, e *env){
		"staging dir is a symlink": func(t *testing.T, e *env) {
			target := filepath.Join(e.anchor, "elsewhere")
			_ = os.Mkdir(target, 0o700)
			for _, n := range stagedFiles {
				_ = os.WriteFile(filepath.Join(target, n), nil, 0o600)
			}
			if err := os.Symlink(target, filepath.Join(e.stage, "job-1")); err != nil {
				t.Fatal(err)
			}
		},
		"staged file is a symlink": func(t *testing.T, e *env) {
			dir := e.stageJob1(t)
			_ = os.Remove(filepath.Join(dir, "rootfs.ext4"))
			if err := os.Symlink(secret, filepath.Join(dir, "rootfs.ext4")); err != nil {
				t.Fatal(err)
			}
		},
		"staged file is a hard link": func(t *testing.T, e *env) {
			dir := e.stageJob1(t)
			if err := os.Link(filepath.Join(dir, "vmlinux"), filepath.Join(e.anchor, "second-link")); err != nil {
				t.Fatal(err)
			}
		},
		"staged file is a fifo": func(t *testing.T, e *env) {
			dir := e.stageJob1(t)
			_ = os.Remove(filepath.Join(dir, "config.json"))
			if err := syscall.Mkfifo(filepath.Join(dir, "config.json"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"staging base is a symlink": func(t *testing.T, e *env) {
			target := filepath.Join(e.anchor, "real-staging")
			if err := os.Rename(e.stage, target); err != nil {
				t.Fatal(err)
			}
			_ = os.Symlink(target, e.stage)
			e.stageJob1(t)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			setup(t, e)
			code, stderr := e.call(e.start...)
			if code == 0 {
				t.Fatal("start accepted hostile staging")
			}
			t.Logf("refused: %s", strings.TrimSpace(stderr))
			if len(e.rec.execArgv) != 0 {
				t.Error("jailer was executed")
			}
			if data, err := os.ReadFile(filepath.Join(firecracker.JailDir(e.base, "job-1"), "root", "rootfs.ext4")); err == nil && string(data) == "root only" {
				t.Error("secret was copied into the jail")
			}
		})
	}
}

// The jail tree must be root's all the way up, or the runner could swap
// parts of it between the helper's checks and the jailer's use.
func TestRequiresRootOwnedJailTree(t *testing.T) {
	cases := map[string]func(e *env){
		"base owned by someone else": func(e *env) { e.cfg.RootUID = e.me + 1 },
		"base group-writable":        func(e *env) { _ = os.Chmod(e.base, 0o775) }, //nolint:gosec // the unsafe mode is the point
		"base is a symlink": func(e *env) {
			target := filepath.Join(e.anchor, "real-jails")
			_ = os.Rename(e.base, target)
			_ = os.Symlink(target, e.base)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.stageJob1(t)
			setup(e)
			code, stderr := e.call(e.start...)
			if code == 0 {
				t.Error("start accepted an unsafe jail tree")
			}
			t.Logf("refused: %s", strings.TrimSpace(stderr))
			if len(e.rec.execArgv) != 0 {
				t.Error("jailer was executed")
			}
		})
	}
}

func TestKillAndRemove(t *testing.T) {
	e := newEnv(t)
	e.stageJob1(t)
	if code, stderr := e.call(e.start...); code != 0 {
		t.Fatalf("start: %s", stderr)
	}
	cg := firecracker.CgroupDir(e.cg, "job-1")
	if err := os.MkdirAll(cg, 0o700); err != nil {
		t.Fatal(err)
	}
	// cgroupfs provides cgroup.kill in every cgroup.
	if err := os.WriteFile(filepath.Join(cg, "cgroup.kill"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, stderr := e.call("kill", "--id", "job-1"); code != 0 {
		t.Fatalf("kill: %s", stderr)
	}
	if data, _ := os.ReadFile(filepath.Join(cg, "cgroup.kill")); strings.TrimSpace(string(data)) != "1" {
		t.Errorf("cgroup.kill = %q", data)
	}
	_ = os.Remove(filepath.Join(cg, "cgroup.kill")) // real cgroupfs has no such file to delete
	if code, stderr := e.call("remove", "--id", "job-1"); code != 0 {
		t.Fatalf("remove: %s", stderr)
	}
	want := []string{"rm", "-rf", "--one-file-system", "--", firecracker.JailDir(e.base, "job-1")}
	if len(e.rec.runArgv) != 1 || !slices.Equal(e.rec.runArgv[0], want) {
		t.Errorf("rm call = %v, want %v", e.rec.runArgv, want)
	}
	for _, gone := range []string{firecracker.JailDir(e.base, "job-1"), cg} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s still exists", gone)
		}
	}
	// Not a cgroup (no cgroup.kill): refuse rather than create files.
	notCg := firecracker.CgroupDir(e.cg, "job-3")
	_ = os.MkdirAll(notCg, 0o700)
	if code, _ := e.call("kill", "--id", "job-3"); code == 0 {
		t.Error("kill wrote into a directory that is not a cgroup")
	}
	if _, err := os.Stat(filepath.Join(notCg, "cgroup.kill")); !os.IsNotExist(err) {
		t.Error("kill created cgroup.kill")
	}
	// Idempotent.
	if code, _ := e.call("kill", "--id", "job-1"); code != 0 {
		t.Error("kill of a missing cgroup failed")
	}
	if code, _ := e.call("remove", "--id", "job-1"); code != 0 {
		t.Error("remove of a missing jail failed")
	}
	// Refuses once the tree is no longer root's.
	_ = os.Chmod(filepath.Join(e.base, "firecracker"), 0o777) //nolint:gosec // the unsafe mode is the point
	if code, _ := e.call("remove", "--id", "job-1"); code == 0 {
		t.Error("remove accepted a world-writable jail tree")
	}
}

func TestRefusesOversizedStagedFiles(t *testing.T) {
	e := newEnv(t)
	dir := e.stageJob1(t)
	e.cfg.MaxRootFSBytes = 1024
	// A sparse file: tiny on disk, but root would write all of it.
	f, err := os.OpenFile(filepath.Join(dir, "rootfs.ext4"), os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 30); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	code, stderr := e.call(e.start...)
	if code == 0 || !strings.Contains(stderr, "limit") {
		t.Fatalf("oversized rootfs accepted: %d %q", code, stderr)
	}
	if len(e.rec.execArgv) != 0 {
		t.Error("jailer was executed")
	}
	if _, err := os.Stat(firecracker.JailDir(e.base, "job-1")); !os.IsNotExist(err) {
		t.Error("a jail was created for a refused start")
	}
}

func TestLimitsTotalJailSpace(t *testing.T) {
	t.Run("too many jails", func(t *testing.T) {
		e := newEnv(t) // Identities: 2
		for _, id := range []string{"old-1", "old-2"} {
			if err := os.MkdirAll(firecracker.JailDir(e.base, id), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		e.stageJob1(t)
		if code, stderr := e.call(e.start...); code == 0 || !strings.Contains(stderr, "jails exist already") {
			t.Errorf("start with the jail limit reached: %d %q", code, stderr)
		}
	})
	t.Run("not enough free space", func(t *testing.T) {
		e := newEnv(t)
		e.stageJob1(t)
		e.rec.free = MinFreeBytes // nothing left once MinFreeBytes is kept
		if code, stderr := e.call(e.start...); code == 0 || !strings.Contains(stderr, "not enough space") {
			t.Errorf("start without free space: %d %q", code, stderr)
		}
		if _, err := os.Stat(firecracker.JailDir(e.base, "job-1")); !os.IsNotExist(err) {
			t.Error("a jail was created without space for it")
		}
	})
}

func TestFailedCopyRemovesPartialJail(t *testing.T) {
	e := newEnv(t)
	dir := e.stageJob1(t)
	// An unreadable staged file fails after the jail directory exists.
	if err := os.Chmod(filepath.Join(dir, "config.json"), 0o000); err != nil {
		t.Fatal(err)
	}
	if os.Getuid() == 0 {
		t.Skip("root can read anything")
	}
	if code, _ := e.call(e.start...); code == 0 {
		t.Fatal("start succeeded with an unreadable staged file")
	}
	if _, err := os.Stat(firecracker.JailDir(e.base, "job-1")); !os.IsNotExist(err) {
		t.Errorf("partial jail left behind: %v", err)
	}
}

// remove must fail closed when the chroot base is not root's, even if
// nothing exists below it yet.
func TestRemoveFailsClosedOnUnsafeBase(t *testing.T) {
	for name, setup := range map[string]func(e *env){
		"base missing":         func(e *env) { _ = os.Remove(e.base) },
		"base group-writable":  func(e *env) { _ = os.Chmod(e.base, 0o775) }, //nolint:gosec // the unsafe mode is the point
		"base owned by runner": func(e *env) { e.cfg.RootUID = e.me + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			setup(e)
			if code, _ := e.call("remove", "--id", "job-1"); code == 0 {
				t.Error("remove accepted an unsafe chroot base")
			}
			if len(e.rec.runArgv) != 0 {
				t.Error("rm was run")
			}
		})
	}
}
