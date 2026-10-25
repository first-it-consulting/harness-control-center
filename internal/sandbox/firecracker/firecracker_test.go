package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
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
	// jailer turns requests into argv exactly like the helper does.
	jailer  JailerConfig
	staging string

	mu         sync.Mutex
	argv       []string               // last jailer call
	argvs      [][]string             // all jailer calls
	jailSeen   map[string]os.FileMode // staged files at start
	config     map[string]any
	signals    []os.Signal
	killed     []string
	removed    []string
	proc       *fakeProc
	events     []string // order of jail/network operations
	netErr     error
	netSlots   []int
	reqs       []StartRequest
	disk       string // what CollectDisk hands out
	collectErr error
	collected  []string
}

func (h *fakeHost) CollectDisk(_ context.Context, id string, fn func(io.Reader) error) error {
	h.mu.Lock()
	h.collected = append(h.collected, id)
	h.events = append(h.events, "collect "+id)
	disk, cerr := h.disk, h.collectErr
	h.mu.Unlock()
	if cerr != nil {
		return cerr
	}
	return fn(strings.NewReader(disk))
}

func (h *fakeHost) SetupNetwork(_ context.Context, slot int) (NetInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.netErr != nil {
		return NetInfo{}, h.netErr
	}
	h.netSlots = append(h.netSlots, slot)
	h.events = append(h.events, fmt.Sprintf("net-up %d", slot))
	nc := NetworkConfig{Subnet: netip.MustParsePrefix("172.31.0.0/16"), ProxyPort: 3128}
	return nc.Link(slot), nil
}

func (h *fakeHost) TeardownNetwork(_ context.Context, slot int) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, fmt.Sprintf("net-down %d", slot))
	return nil
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

func (h *fakeHost) StartVM(req StartRequest, console io.Writer) (Process, error) {
	if h.startErr != nil {
		return nil, h.startErr
	}
	argv, err := h.jailer.Argv(req)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.reqs = append(h.reqs, req)
	defer h.mu.Unlock()
	h.argv = argv
	h.argvs = append(h.argvs, argv)
	root := filepath.Join(h.staging, req.ID) // what the helper copies into the jail
	h.jailSeen = map[string]os.FileMode{}
	if h.onStart != nil {
		h.onStart(root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		h.t.Errorf("staging missing at start: %v", err)
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
	h.events = append(h.events, "remove "+id)
	return nil
}

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
	base := filepath.Join(dir, "staging")
	host := &fakeHost{t: t, behavior: b, staging: base, jailer: JailerConfig{
		JailerPath: filepath.Join(dir, "jailer"), FirecrackerPath: filepath.Join(dir, "firecracker"),
		ChrootBase: filepath.Join(dir, "jails"), UID: 64000, GID: 64000, Identities: 8,
	}}
	p, err := New(Config{StagingBase: base, StopGrace: 20 * time.Millisecond, Host: host})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{p: p, host: host, base: base, spec: sandbox.Spec{
		ID: "run-1", Image: "base", Task: []byte(`{"prompt":"hello"}`), CPUs: 2, MemoryMiB: 256, Timeout: time.Minute,
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
	if _, err := os.Stat(filepath.Join(f.base, f.spec.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staging still exists: %v", err)
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

	// Staging held exactly the VM's files, owner-only (the helper makes the
	// jail copies readable for the VM).
	want := map[string]os.FileMode{"config.json": 0o600, "task.img": 0o600}
	for name, mode := range want {
		got, ok := f.host.jailSeen[name]
		// Windows has no Unix modes; presence is still checked there.
		if !ok || (runtime.GOOS != "windows" && got != mode) {
			t.Errorf("staged file %s: mode %v present=%v, want %v", name, got, ok, mode)
		}
	}
	if len(f.host.jailSeen) != len(want) {
		t.Errorf("unexpected staged files: %v", f.host.jailSeen)
	}
}

func TestJailerArgv(t *testing.T) {
	f := newFixture(t, exitsCleanly)
	if _, _, err := f.run(context.Background(), t); err != nil {
		t.Fatal(err)
	}
	argv := f.host.argv
	checks := map[string]string{
		"--id": "run-1", "--exec-file": f.host.jailer.FirecrackerPath, "--uid": "64000", "--gid": "64000",
		"--chroot-base-dir": f.host.jailer.ChrootBase, "--cgroup-version": "2", "--config-file": "config.json",
	}
	for flag, want := range checks {
		if got := argValue(argv, flag); got != want {
			t.Errorf("%s = %q, want %q", flag, got, want)
		}
	}
	if argv[0] != f.host.jailer.JailerPath {
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
	existing := filepath.Join(f.base, f.spec.ID)
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
	good := Config{StagingBase: filepath.Join(d, "s"), Host: &fakeHost{}}
	tests := map[string]func(*Config){
		"unclean base": func(c *Config) { c.StagingBase += string(filepath.Separator) + ".." },
		"no host":      func(c *Config) { c.Host = nil },
		"no staging":   func(c *Config) { c.StagingBase = "" },
		"too many ids": func(c *Config) { c.Identities = MaxIdentities + 1 },
	}
	if _, err := New(good); err != nil {
		t.Fatalf("good config rejected: %v", err)
	}
	for name, mutate := range tests {
		c := good
		mutate(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
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

// testProxy records how the provider uses it.
type testProxy struct {
	mu      sync.Mutex
	served  bool
	stopped bool
}

func (p *testProxy) Serve(ctx context.Context, l net.Listener) error {
	p.mu.Lock()
	p.served = true
	p.mu.Unlock()
	<-ctx.Done()
	_ = l.Close()
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	return nil
}

// withNetwork gives the fixture a network and records the listener address
// the provider asks for (it really listens on loopback).
func withNetwork(t *testing.T, f *fixture) (*testProxy, *[]string) {
	t.Helper()
	proxy := &testProxy{}
	f.spec.Network = &sandbox.Network{Proxy: proxy}
	var asked []string
	f.p.cfg.Listen = func(network, addr string) (net.Listener, error) {
		asked = append(asked, addr)
		return net.Listen(network, "127.0.0.1:0")
	}
	return proxy, &asked
}

func TestRunWithNetwork(t *testing.T) {
	f := newFixture(t, exitsCleanly)
	proxy, asked := withNetwork(t, f)
	if _, _, err := f.run(context.Background(), t); err != nil {
		t.Fatal(err)
	}
	if len(*asked) != 1 || (*asked)[0] != "172.31.0.1:3128" {
		t.Errorf("proxy listener on %v, want the link's host address", *asked)
	}
	if !proxy.served || !proxy.stopped {
		t.Errorf("proxy served=%v stopped=%v", proxy.served, proxy.stopped)
	}
	boot := f.host.config["boot-source"].(map[string]any)["boot_args"].(string)
	for _, want := range []string{"hcc.ip=172.31.0.2/30", "hcc.proxy=172.31.0.1:3128"} {
		if !strings.Contains(boot, want) {
			t.Errorf("boot_args %q missing %q", boot, want)
		}
	}
	nics, _ := f.host.config["network-interfaces"].([]any)
	if len(nics) != 1 || nics[0].(map[string]any)["host_dev_name"] != "hcc0" {
		t.Errorf("network-interfaces = %v", f.host.config["network-interfaces"])
	}
	// The link outlives the jail: removed only after the VM's jail is gone.
	if want := []string{"net-up 0", "remove run-1", "net-down 0"}; !slices.Equal(f.host.events, want) {
		t.Errorf("order = %v, want %v", f.host.events, want)
	}
}

func TestRunWithoutNetworkHasNoInterface(t *testing.T) {
	f := newFixture(t, exitsCleanly)
	if _, _, err := f.run(context.Background(), t); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.host.config["network-interfaces"]; ok || len(f.host.netSlots) != 0 {
		t.Error("a sandbox without Network got a network")
	}
	if strings.Contains(f.host.config["boot-source"].(map[string]any)["boot_args"].(string), "hcc.proxy") {
		t.Error("proxy boot argument without a network")
	}
}

func TestRunNetworkSetupFailure(t *testing.T) {
	f := newFixture(t, exitsCleanly)
	withNetwork(t, f)
	f.host.netErr = errors.New("nft: not found")
	if _, _, err := f.run(context.Background(), t); err == nil || !strings.Contains(err.Error(), "nft") {
		t.Fatalf("Run = %v, want the network error", err)
	}
	if f.host.argv != nil {
		t.Error("VM started without its network")
	}
	if _, err := f.p.acquire(); err != nil {
		t.Errorf("identity not released after a failed setup: %v", err)
	}
}

func TestUnstoppableVMKeepsItsLink(t *testing.T) {
	f := newFixture(t, unkillable)
	withNetwork(t, f)
	f.spec.Timeout = 30 * time.Millisecond
	if _, _, err := f.run(context.Background(), t); err == nil {
		t.Fatal("want did-not-stop error")
	}
	f.host.proc.finish(nil)
	for _, e := range f.host.events {
		if strings.HasPrefix(e, "net-down") {
			t.Error("link of a VM that could not be stopped was deleted")
		}
	}
}

func TestPeerListenerAcceptsOnlyItsGuest(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	for _, tc := range []struct {
		peer string
		want bool
	}{{"127.0.0.1", true}, {"127.0.0.2", false}} {
		l := &peerListener{Listener: inner, peer: netip.MustParseAddr(tc.peer)}
		accepted := make(chan bool, 1)
		go func() {
			c, err := l.Accept()
			if err == nil {
				_ = c.Close()
			}
			accepted <- err == nil
		}()
		c, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-accepted:
			if got != tc.want {
				t.Errorf("peer %s: accepted=%v, want %v", tc.peer, got, tc.want)
			}
		case <-time.After(500 * time.Millisecond):
			if tc.want {
				t.Errorf("peer %s: not accepted", tc.peer)
			}
			_ = inner.Close() // unblock the pending Accept
		}
		_ = c.Close()
	}
}

func TestTaskDriveAndImageName(t *testing.T) {
	f := newFixture(t, exitsCleanly)
	var staged []byte
	f.host.onStart = func(string) {
		staged, _ = os.ReadFile(filepath.Join(f.base, f.spec.ID, "task.img"))
	}
	if _, _, err := f.run(context.Background(), t); err != nil {
		t.Fatal(err)
	}
	if len(f.host.reqs) != 1 || f.host.reqs[0].Image != "base" {
		t.Errorf("start requests = %+v, want image base", f.host.reqs)
	}
	if len(staged) != 512 || string(bytes.TrimRight(staged, "\x00")) != `{"prompt":"hello"}` {
		t.Errorf("task.img = %d bytes %q, want the task padded to one sector", len(staged), bytes.TrimRight(staged, "\x00"))
	}
	drives := f.host.config["drives"].([]any)
	if len(drives) != 2 {
		t.Fatalf("drives = %v", drives)
	}
	task := drives[1].(map[string]any)
	if task["path_on_host"] != "task.img" || task["is_read_only"] != true || task["is_root_device"] != false {
		t.Errorf("task drive = %v", task)
	}
}

func TestNoTaskNoDrive(t *testing.T) {
	f := newFixture(t, exitsCleanly)
	f.spec.Task = nil
	if _, _, err := f.run(context.Background(), t); err != nil {
		t.Fatal(err)
	}
	if drives := f.host.config["drives"].([]any); len(drives) != 1 {
		t.Errorf("drives = %v, want only the root filesystem", drives)
	}
	if _, ok := f.host.jailSeen["task.img"]; ok {
		t.Error("task.img staged without a task")
	}
}

func TestRunCollectsDiskAfterCleanExit(t *testing.T) {
	f := newFixture(t, exitsCleanly)
	f.host.disk = "guest bytes"
	var got string
	f.spec.DiskMiB = 64
	f.spec.Collect = func(r io.Reader) error {
		b, err := io.ReadAll(r)
		got = string(b)
		return err
	}
	if _, _, err := f.run(context.Background(), t); err != nil {
		t.Fatal(err)
	}
	if got != "guest bytes" {
		t.Errorf("collected %q", got)
	}
	if f.host.reqs[0].DiskMiB != 64 {
		t.Errorf("start request disk = %d", f.host.reqs[0].DiskMiB)
	}
	// Collected before the jail is removed.
	if i, j := slices.Index(f.host.events, "collect run-1"), slices.Index(f.host.events, "remove run-1"); i < 0 || j < i {
		t.Errorf("events = %v", f.host.events)
	}
	drives, _ := f.host.config["drives"].([]any)
	last, _ := drives[len(drives)-1].(map[string]any)
	if last["drive_id"] != "disk" || last["is_read_only"] != false || last["path_on_host"] != "disk.img" {
		t.Errorf("disk drive = %v", last)
	}
	f.assertCleanedUp(t)
}

func TestRunDoesNotCollectAfterTimeout(t *testing.T) {
	f := newFixture(t, hangsUntilSIGTERM)
	f.spec.Timeout = 50 * time.Millisecond
	f.spec.DiskMiB = 64
	f.spec.Collect = func(io.Reader) error { t.Error("collected a stopped VM's disk"); return nil }
	if res, _, err := f.run(context.Background(), t); err != nil || res.Outcome != sandbox.OutcomeTimedOut {
		t.Fatalf("Run = %v, %v", res.Outcome, err)
	}
	if len(f.host.collected) != 0 {
		t.Errorf("collected %v", f.host.collected)
	}
}

func TestRunCollectFailureFailsTheRun(t *testing.T) {
	f := newFixture(t, exitsCleanly)
	f.host.collectErr = errors.New("helper failed")
	f.spec.DiskMiB = 64
	f.spec.Collect = func(io.Reader) error { return nil }
	if _, _, err := f.run(context.Background(), t); err == nil || !strings.Contains(err.Error(), "helper failed") {
		t.Errorf("err = %v", err)
	}
	f.assertCleanedUp(t)
}

func TestNoDiskNoDrive(t *testing.T) {
	f := newFixture(t, exitsCleanly)
	if _, _, err := f.run(context.Background(), t); err != nil {
		t.Fatal(err)
	}
	drives, _ := f.host.config["drives"].([]any)
	for _, d := range drives {
		if m, _ := d.(map[string]any); m["is_read_only"] != true {
			t.Errorf("writable drive without a disk: %v", m)
		}
	}
}
