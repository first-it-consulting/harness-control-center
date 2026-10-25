//go:build unix

package helper

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/first-it-consulting/harness-control-center/internal/sandbox/firecracker"
)

type recorder struct {
	execArgv [][]string
	execEnv  [][]string
	runArgv  [][]string
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
	}
}

type env struct {
	cfg  Config
	base string
	cg   string
	rec  *recorder
	me   int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	// Resolve symlinks (macOS: /var -> /private/var) so the temp dir
	// itself does not trip the symlink checks.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	me := os.Getuid()
	e := &env{base: filepath.Join(dir, "jails"), cg: filepath.Join(dir, "cgroup"), rec: &recorder{}, me: me}
	e.cfg = Config{
		Jailer: firecracker.JailerConfig{
			JailerPath: "/usr/local/lib/hcc/jailer", FirecrackerPath: "/usr/local/lib/hcc/firecracker",
			ChrootBase: e.base, UID: 64000, GID: 64000, Identities: 2,
			ForbiddenIDs: []int{0, me},
		},
		CgroupRoot: e.cg,
		RunnerUID:  me,
	}
	return e
}

// prepare creates the jail for job-1 like the provider does.
func (e *env) prepare(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(firecracker.JailDir(e.base, "job-1"), "root"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func (e *env) call(args ...string) (int, string) {
	var stderr bytes.Buffer
	code := Main(e.cfg, args, e.me, e.rec.deps(), &stderr)
	return code, stderr.String()
}

func TestStartExecsJailerFromConfig(t *testing.T) {
	e := newEnv(t)
	e.prepare(t)
	code, stderr := e.call("start", "--id", "job-1", "--slot", "1", "--cpus", "2", "--memory", "256")
	if code != 0 {
		t.Fatalf("start failed: %s", stderr)
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
}

func TestRefusesOtherCallers(t *testing.T) {
	e := newEnv(t)
	e.prepare(t)
	var stderr bytes.Buffer
	code := Main(e.cfg, []string{"start", "--id", "job-1", "--slot", "0", "--cpus", "1", "--memory", "128"}, e.me+1, e.rec.deps(), &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "not the runner") || len(e.rec.execArgv) != 0 {
		t.Errorf("foreign caller: code %d, %q", code, stderr.String())
	}
}

func TestRejectsBadArguments(t *testing.T) {
	e := newEnv(t)
	e.prepare(t)
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
		"unprepared jail":    {"start", "--id", "job-2", "--slot", "0", "--cpus", "1", "--memory", "128"},
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

// A compromised runner owns the chroot base, so it could swap parts of a
// jail for symlinks to make the jailer chroot into (or rm delete) some
// other place on the host.
func TestRefusesSymlinkedJails(t *testing.T) {
	outside := t.TempDir()
	cases := map[string]func(e *env){
		"root is a symlink": func(e *env) {
			jail := firecracker.JailDir(e.base, "job-1")
			_ = os.MkdirAll(jail, 0o700)
			_ = os.Symlink("/", filepath.Join(jail, "root"))
		},
		"jail dir is a symlink": func(e *env) {
			parent := filepath.Join(e.base, "firecracker")
			_ = os.MkdirAll(filepath.Join(outside, "root"), 0o700)
			_ = os.MkdirAll(parent, 0o700)
			_ = os.Symlink(outside, filepath.Join(parent, "job-1"))
		},
		"chroot base is a symlink": func(e *env) {
			_ = os.MkdirAll(filepath.Join(outside, "firecracker", "job-1", "root"), 0o700)
			_ = os.Symlink(outside, e.base)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			setup(e)
			if code, stderr := e.call("start", "--id", "job-1", "--slot", "0", "--cpus", "1", "--memory", "128"); code == 0 {
				t.Error("start accepted a symlinked jail")
			} else if !strings.Contains(stderr, "symlink") {
				t.Errorf("unexpected refusal: %s", stderr)
			}
			if code, _ := e.call("remove", "--id", "job-1"); code == 0 && name != "root is a symlink" {
				t.Error("remove followed a symlinked jail")
			}
			if len(e.rec.execArgv) != 0 {
				t.Error("jailer was executed")
			}
		})
	}
}

func TestRefusesJailNotOwnedByRunner(t *testing.T) {
	e := newEnv(t)
	e.prepare(t)
	// Pretend the runner is someone else: the jail (owned by us) is then
	// not the runner's.
	e.cfg.RunnerUID = e.me + 1
	e.cfg.Jailer.ForbiddenIDs = []int{0, e.me + 1}
	var stderr bytes.Buffer
	code := Main(e.cfg, []string{"start", "--id", "job-1", "--slot", "0", "--cpus", "1", "--memory", "128"}, e.me+1, e.rec.deps(), &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "owned by the runner") {
		t.Errorf("code %d, %q", code, stderr.String())
	}
}

func TestKillAndRemove(t *testing.T) {
	e := newEnv(t)
	e.prepare(t)
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
}
