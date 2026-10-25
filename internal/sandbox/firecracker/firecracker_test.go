package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
)

// behavior decides how a fake VM reacts.
type behavior int

const (
	exitsCleanly behavior = iota
	crashes
	hangsUntilSIGTERM
	ignoresSIGTERM // only cgroup.kill stops it
	unkillable
)

type fakeHost struct {
	t        *testing.T
	behavior behavior
	startErr error
	onStart  func(jailRoot string)

	mu       sync.Mutex
	argv     []string               // last jailer call
	argvs    [][]string             // all jailer calls
	jailSeen map[string]os.FileMode // files in the jail root at start
	config   map[string]any
	signals  []os.Signal
	killed   []string
	removed  []string
	proc     *fakeProc
}

type fakeProc struct {
	h    *fakeHost
	done chan error
	once sync.Once
}

func (p *fakeProc) finish(err error) { p.once.Do(func() { p.done <- err }) }
func (p *fakeProc) Wait() error      { return <-p.done }
func (p *fakeProc) Signal(s os.Signal) error {
	p.h.mu.Lock()
	p.h.signals = append(p.h.signals, s)
	b := p.h.behavior
	p.h.mu.Unlock()
	if s == syscall.SIGTERM && b == hangsUntilSIGTERM {
		p.finish(errors.New("signal: terminated"))
	}
	return nil
}

func (h *fakeHost) StartJailer(argv []string, console io.Writer) (Process, error) {
	if h.startErr != nil {
		return nil, h.startErr
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.argv = argv
	h.argvs = append(h.argvs, argv)
	root := filepath.Join(argValue(argv, "--chroot-base-dir"), "firecracker", argValue(argv, "--id"), "root")
	h.jailSeen = map[string]os.FileMode{}
	if h.onStart != nil {
		h.onStart(root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		h.t.Errorf("jail root missing at start: %v", err)
	}
	for _, e := range entries {
		fi, _ := e.Info()
		h.jailSeen[e.Name()] = fi.Mode().Perm()
	}
	if data, err := os.ReadFile(filepath.Join(root, "config.json")); err == nil {
		_ = json.Unmarshal(data, &h.config)
	}
	_, _ = io.WriteString(console, "HCC-GUEST-UP\n")

	h.proc = &fakeProc{h: h, done: make(chan error, 1)}
	switch h.behavior {
	case exitsCleanly:
		h.proc.finish(nil)
	case crashes:
		h.proc.finish(errors.New("exit status 1"))
	}
	return h.proc, nil
}

func (h *fakeHost) KillJail(_ context.Context, id string) error {
	h.mu.Lock()
	h.killed = append(h.killed, id)
	b := h.behavior
	h.mu.Unlock()
	if b == ignoresSIGTERM {
		h.proc.finish(errors.New("signal: killed"))
	}
	return nil
}

func (h *fakeHost) RemoveJail(_ context.Context, id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removed = append(h.removed, id)
	// Mimic the real host so leftovers would be visible to the test.
	return os.RemoveAll(filepath.Join(h.chrootBase(), "firecracker", id))
}

func (h *fakeHost) chrootBase() string {
	if h.argv != nil {
		return argValue(h.argv, "--chroot-base-dir")
	}
	return testBase
}

var testBase string

func argValue(argv []string, flag string) string {
	if i := slices.Index(argv, flag); i >= 0 && i+1 < len(argv) {
		return argv[i+1]
	}
	return ""
}

type fixture struct {
	p    *Provider
	host *fakeHost
	spec sandbox.Spec
	base string
}

func newFixture(t *testing.T, b behavior) *fixture {
	t.Helper()
	dir := t.TempDir()
	base := filepath.Join(dir, "jails")
	testBase = base
	k, r := filepath.Join(dir, "vmlinux"), filepath.Join(dir, "rootfs.ext4")
	if err := os.WriteFile(k, []byte("kernel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r, []byte("rootfs"), 0o600); err != nil {
		t.Fatal(err)
	}
	host := &fakeHost{t: t, behavior: b}
	p, err := New(Config{
		JailerPath: filepath.Join(dir, "jailer"), FirecrackerPath: filepath.Join(dir, "firecracker"),
		ChrootBase: base, ImageRoot: dir, UID: 64000, GID: 64000, StopGrace: 20 * time.Millisecond, Host: host,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{p: p, host: host, base: base, spec: sandbox.Spec{
		ID: "run-1", Kernel: k, RootFS: r, CPUs: 2, MemoryMiB: 256, Timeout: time.Minute,
		BootArgs: []string{"hcc.mode=test"},
	}}
}

func (f *fixture) run(ctx context.Context, t *testing.T) (res sandbox.Result, console string, err error) {
	t.Helper()
	var out strings.Builder
	res, err = f.p.Run(ctx, f.spec, &out)
	return res, out.String(), err
}

func (f *fixture) assertCleanedUp(t *testing.T) {
	t.Helper()
	if !slices.Equal(f.host.removed, []string{f.spec.ID}) {
		t.Errorf("RemoveJail calls = %v, want exactly [%s]", f.host.removed, f.spec.ID)
	}
	if _, err := os.Stat(jailDir(f.base, f.spec.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("jail still exists: %v", err)
	}
}

func TestRunExited(t *testing.T) {
	f := newFixture(t, exitsCleanly)
	res, console, err := f.run(context.Background(), t)
	if err != nil || res.Outcome != sandbox.OutcomeExited {
		t.Fatalf("Run = %v, %v; want exited", res.Outcome, err)
	}
	if !strings.Contains(console, "HCC-GUEST-UP") {
		t.Errorf("console not streamed: %q", console)
	}
	if res.Duration <= 0 {
		t.Error("duration not set")
	}
	f.assertCleanedUp(t)

	// The jail held exactly the VM's files, all read-only.
	want := map[string]os.FileMode{"vmlinux": 0o444, "rootfs.ext4": 0o444, "config.json": 0o444}
	for name, mode := range want {
		if got, ok := f.host.jailSeen[name]; !ok || got != mode {
			t.Errorf("jail file %s: mode %v present=%v, want %v", name, got, ok, mode)
		}
	}
	if len(f.host.jailSeen) != len(want) {
		t.Errorf("unexpected jail contents: %v", f.host.jailSeen)
	}
}

func TestJailerArgv(t *testing.T) {
	f := newFixture(t, exitsCleanly)
	if _, _, err := f.run(context.Background(), t); err != nil {
		t.Fatal(err)
	}
	argv := f.host.argv
	checks := map[string]string{
		"--id": "run-1", "--exec-file": f.p.cfg.FirecrackerPath, "--uid": "64000", "--gid": "64000",
		"--chroot-base-dir": f.base, "--cgroup-version": "2", "--config-file": "config.json",
	}
	for flag, want := range checks {
		if got := argValue(argv, flag); got != want {
			t.Errorf("%s = %q, want %q", flag, got, want)
		}
	}
	if argv[0] != f.p.cfg.JailerPath {
		t.Errorf("argv[0] = %q", argv[0])
	}
	for _, want := range []string{"memory.max=335544320", "pids.max=64", "cpu.max=200000 100000"} {
		if !slices.Contains(argv, want) {
			t.Errorf("argv missing cgroup limit %q: %v", want, argv)
		}
	}
	if !slices.Contains(argv, "--no-api") {
		t.Error("API socket not disabled")
	}
}

func TestVMConfig(t *testing.T) {
	f := newFixture(t, exitsCleanly)
	if _, _, err := f.run(context.Background(), t); err != nil {
		t.Fatal(err)
	}
	cfg := f.host.config
	boot := cfg["boot-source"].(map[string]any)["boot_args"].(string)
	for _, want := range []string{"init=/init", " ro ", "console=ttyS0", "hcc.mode=test"} {
		if !strings.Contains(" "+boot+" ", want) {
			t.Errorf("boot_args %q missing %q", boot, want)
		}
	}
	drive := cfg["drives"].([]any)[0].(map[string]any)
	if drive["is_read_only"] != true || drive["is_root_device"] != true {
		t.Errorf("root drive not read-only: %v", drive)
	}
	mc := cfg["machine-config"].(map[string]any)
	if mc["vcpu_count"] != 2.0 || mc["mem_size_mib"] != 256.0 {
		t.Errorf("machine-config = %v", mc)
	}
	if _, ok := cfg["network-interfaces"]; ok {
		t.Error("VM must have no network interfaces")
	}
}

func TestRunCrashed(t *testing.T) {
	f := newFixture(t, crashes)
	res, _, err := f.run(context.Background(), t)
	if err == nil || res.Outcome != sandbox.OutcomeFailed {
		t.Fatalf("Run = %v, %v; want failed with error", res.Outcome, err)
	}
	f.assertCleanedUp(t)
}

func TestRunStartError(t *testing.T) {
	f := newFixture(t, exitsCleanly)
	f.host.startErr = errors.New("sudo: a password is required")
	res, _, err := f.run(context.Background(), t)
	if err == nil || res.Outcome != sandbox.OutcomeFailed {
		t.Fatalf("Run = %v, %v; want failed", res.Outcome, err)
	}
	f.assertCleanedUp(t)
}

func TestRunTimeoutStopsWithSIGTERM(t *testing.T) {
	f := newFixture(t, hangsUntilSIGTERM)
	f.spec.Timeout = 30 * time.Millisecond
	res, _, err := f.run(context.Background(), t)
	if err != nil || res.Outcome != sandbox.OutcomeTimedOut {
		t.Fatalf("Run = %v, %v; want timed_out", res.Outcome, err)
	}
	if !slices.Equal(f.host.signals, []os.Signal{syscall.SIGTERM}) {
		t.Errorf("signals = %v, want [SIGTERM]", f.host.signals)
	}
	if len(f.host.killed) != 0 {
		t.Errorf("cgroup.kill used although SIGTERM worked: %v", f.host.killed)
	}
	f.assertCleanedUp(t)
}

func TestRunTimeoutEscalatesToCgroupKill(t *testing.T) {
	f := newFixture(t, ignoresSIGTERM)
	f.spec.Timeout = 30 * time.Millisecond
	res, _, err := f.run(context.Background(), t)
	if err != nil || res.Outcome != sandbox.OutcomeTimedOut {
		t.Fatalf("Run = %v, %v; want timed_out", res.Outcome, err)
	}
	if !slices.Equal(f.host.killed, []string{"run-1"}) {
		t.Errorf("KillJail calls = %v, want [run-1]", f.host.killed)
	}
	f.assertCleanedUp(t)
}

func TestRunUnkillableIsReported(t *testing.T) {
	f := newFixture(t, unkillable)
	f.spec.Timeout = 30 * time.Millisecond
	res, _, err := f.run(context.Background(), t)
	if err == nil || !strings.Contains(err.Error(), "did not stop") || res.Outcome != sandbox.OutcomeFailed {
		t.Fatalf("Run = %v, %v; want failed with 'did not stop'", res.Outcome, err)
	}
	f.host.proc.finish(nil) // release the waiting goroutine
}

func TestRunCancelIsKill(t *testing.T) {
	f := newFixture(t, hangsUntilSIGTERM)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	res, _, err := f.run(ctx, t)
	if err != nil || res.Outcome != sandbox.OutcomeKilled {
		t.Fatalf("Run = %v, %v; want killed", res.Outcome, err)
	}
	f.assertCleanedUp(t)
}

func TestRunRefusesExistingJail(t *testing.T) {
	f := newFixture(t, exitsCleanly)
	existing := jailDir(f.base, f.spec.ID)
	if err := os.MkdirAll(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.run(context.Background(), t); err == nil {
		t.Fatal("Run reused an existing jail")
	}
	if len(f.host.removed) != 0 {
		t.Errorf("removed a jail it did not create: %v", f.host.removed)
	}
	if _, err := os.Stat(existing); err != nil {
		t.Errorf("existing jail was touched: %v", err)
	}
}

func TestRunInvalidSpecTouchesNothing(t *testing.T) {
	f := newFixture(t, exitsCleanly)
	f.spec.ID = "../escape"
	if _, _, err := f.run(context.Background(), t); err == nil {
		t.Fatal("invalid spec accepted")
	}
	if _, err := os.Stat(f.base); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("chroot base created for an invalid spec: %v", err)
	}
	if len(f.host.removed) != 0 || f.host.argv != nil {
		t.Error("host was used for an invalid spec")
	}
}

func TestNewRejectsUnsafeConfig(t *testing.T) {
	d := t.TempDir()
	good := Config{
		JailerPath: filepath.Join(d, "jailer"), FirecrackerPath: filepath.Join(d, "firecracker"),
		ChrootBase: filepath.Join(d, "j"), ImageRoot: filepath.Join(d, "img"), UID: 64000, GID: 64000, Host: &fakeHost{},
	}
	tests := map[string]func(*Config){
		"root uid":         func(c *Config) { c.UID = 0 },
		"root gid":         func(c *Config) { c.GID = 0 },
		"relative jailer":  func(c *Config) { c.JailerPath = "jailer" },
		"unclean base":     func(c *Config) { c.ChrootBase = c.ImageRoot + string(filepath.Separator) + ".." },
		"no host":          func(c *Config) { c.Host = nil },
		"no image root":    func(c *Config) { c.ImageRoot = "" },
		"too many ids":     func(c *Config) { c.Identities = MaxIdentities + 1 },
		"range overflows":  func(c *Config) { c.UID = math.MaxInt32 },
		"runner's own uid": func(c *Config) { c.UID = os.Getuid() },
		"range covers own uid": func(c *Config) {
			c.UID, c.Identities = os.Getuid()-1, 4
		},
	}
	if _, err := New(good); err != nil {
		t.Fatalf("good config rejected: %v", err)
	}
	for name, mutate := range tests {
		c := good
		mutate(&c)
		if (name == "runner's own uid" || name == "range covers own uid") && os.Getuid() <= 0 {
			continue // running as root or on Windows: covered by "root uid"
		}
		if _, err := New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRunConfinesImagesToImageRoot(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.ext4")
	if err := os.WriteFile(outside, []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(t *testing.T, f *fixture){
		"path outside root": func(_ *testing.T, f *fixture) { f.spec.RootFS = outside },
		"symlink escaping root": func(t *testing.T, f *fixture) {
			skipWithoutSymlinks(t)
			link := filepath.Join(filepath.Dir(f.spec.Kernel), "escape.ext4")
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			f.spec.RootFS = link
		},
		"dot-dot path": func(t *testing.T, f *fixture) {
			root := filepath.Dir(f.spec.Kernel)
			rel, err := filepath.Rel(root, outside)
			if err != nil || !strings.HasPrefix(rel, "..") {
				t.Fatalf("test setup: rel = %q, %v", rel, err)
			}
			f.spec.Kernel = root + string(filepath.Separator) + rel // keep the ".." unresolved
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, exitsCleanly)
			mutate(t, f)
			_, _, err := f.run(context.Background(), t)
			if err == nil || !strings.Contains(err.Error(), "outside the image root") {
				t.Fatalf("Run = %v, want outside-the-image-root error", err)
			}
			if f.host.argv != nil {
				t.Error("VM started with an image outside the root")
			}
		})
	}
}

func TestRunFollowsSymlinksInsideImageRoot(t *testing.T) {
	skipWithoutSymlinks(t)
	f := newFixture(t, exitsCleanly)
	link := filepath.Join(filepath.Dir(f.spec.Kernel), "vmlinux-current")
	if err := os.Symlink(f.spec.Kernel, link); err != nil {
		t.Fatal(err)
	}
	f.spec.Kernel = link
	if _, _, err := f.run(context.Background(), t); err != nil {
		t.Fatalf("symlink inside the image root rejected: %v", err)
	}
}

// Creating symlinks on Windows needs elevated rights or developer mode.
func skipWithoutSymlinks(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need extra privileges on Windows")
	}
}

// Concurrent VMs must never share a UID/GID.
func TestConcurrentRunsGetDistinctIdentities(t *testing.T) {
	f := newFixture(t, hangsUntilSIGTERM)
	f.p.cfg.Identities = 3
	f.p.inUse = make([]bool, 3)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			spec := f.spec
			spec.ID = fmt.Sprintf("run-%d", i)
			if _, err := f.p.Run(ctx, spec, io.Discard); err != nil {
				t.Errorf("run-%d: %v", i, err)
			}
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.host.mu.Lock()
		n := len(f.host.argvs)
		f.host.mu.Unlock()
		if n == 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	wg.Wait()

	uids, gids := map[string]bool{}, map[string]bool{}
	for _, argv := range f.host.argvs {
		uids[argValue(argv, "--uid")] = true
		gids[argValue(argv, "--gid")] = true
	}
	want := map[string]bool{"64000": true, "64001": true, "64002": true}
	if len(uids) != 3 || len(gids) != 3 {
		t.Fatalf("identities not distinct: uids %v gids %v", uids, gids)
	}
	for u := range uids {
		if !want[u] {
			t.Errorf("uid %s outside the configured range", u)
		}
	}
}

func TestIdentityExhaustionAndRelease(t *testing.T) {
	f := newFixture(t, exitsCleanly)
	slot, err := f.p.acquire() // the only identity is now taken
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.run(context.Background(), t); err == nil || !strings.Contains(err.Error(), "identities are in use") {
		t.Fatalf("Run with no free identity = %v", err)
	}
	if f.host.argv != nil {
		t.Error("VM started without a free identity")
	}
	f.p.release(slot)
	if _, _, err := f.run(context.Background(), t); err != nil {
		t.Fatalf("Run after release: %v", err)
	}
	// Released again after the run.
	if _, err := f.p.acquire(); err != nil {
		t.Errorf("identity not released after a run: %v", err)
	}
}

func TestUnstoppableVMKeepsItsIdentity(t *testing.T) {
	f := newFixture(t, unkillable)
	f.spec.Timeout = 30 * time.Millisecond
	if _, _, err := f.run(context.Background(), t); err == nil {
		t.Fatal("want did-not-stop error")
	}
	f.host.proc.finish(nil)
	if _, err := f.p.acquire(); err == nil {
		t.Error("identity of a VM that could not be stopped was reused")
	}
}
