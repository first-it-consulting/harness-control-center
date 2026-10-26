package vz

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
)

// The provider runs on macOS; its logic is tested on every Unix (POSIX
// paths and file modes).
func TestMain(m *testing.M) {
	if runtime.GOOS == "windows" {
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeMachine plays a VM: run decides what the "guest" does once started.
type fakeMachine struct {
	mc   MachineConfig
	run  func(m *fakeMachine)
	done chan struct{}
	once sync.Once

	mu      sync.Mutex
	err     error
	stopped bool
	l       net.Listener
}

func (m *fakeMachine) Start() error {
	go m.run(m)
	return nil
}

func (m *fakeMachine) exit(err error) {
	m.once.Do(func() {
		m.mu.Lock()
		m.err = err
		m.mu.Unlock()
		close(m.done)
	})
}

func (m *fakeMachine) Done() <-chan struct{} { return m.done }

func (m *fakeMachine) Err() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.err
}

func (m *fakeMachine) Stop() error {
	m.mu.Lock()
	m.stopped = true
	m.mu.Unlock()
	m.exit(nil)
	return nil
}

func (m *fakeMachine) Listen(uint32) (net.Listener, error) {
	if !m.mc.Vsock {
		return nil, errors.New("no vsock device")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	m.mu.Lock()
	m.l = l
	m.mu.Unlock()
	return l, err
}

type fixture struct {
	p        *Provider
	dir      string
	run      func(m *fakeMachine)
	machines []*fakeMachine
	mu       sync.Mutex
}

func (f *fixture) machinesRun(m *fakeMachine) { f.run(m) }

func newFixture(t *testing.T, run func(m *fakeMachine)) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{dir: dir, run: run}
	p, err := New(Config{
		StagingBase: filepath.Join(dir, "staging"),
		Images: map[string]Image{
			"base":        {Kernel: "/img/Image", RootFS: "/img/base.ext4"},
			"claude-code": {Kernel: "/img/Image", RootFS: "/img/cc.ext4", Tools: "/img/tools.ext4"},
		},
		Slots:     1,
		StopGrace: time.Second,
		NewMachine: func(mc MachineConfig) (Machine, error) {
			m := &fakeMachine{mc: mc, run: run, done: make(chan struct{})}
			f.mu.Lock()
			f.machines = append(f.machines, m)
			f.mu.Unlock()
			return m, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.p = p
	return f
}

func spec(id string) sandbox.Spec {
	return sandbox.Spec{ID: id, Image: "base", CPUs: 2, MemoryMiB: 512, Timeout: 5 * time.Second}
}

func TestNewChecksConfig(t *testing.T) {
	ok := Config{StagingBase: "/s", Images: map[string]Image{"base": {Kernel: "/k", RootFS: "/r"}}}
	if _, err := New(ok); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config){
		"relative staging": func(c *Config) { c.StagingBase = "s" },
		"no images":        func(c *Config) { c.Images = nil },
		"bad image name":   func(c *Config) { c.Images = map[string]Image{"Bad Name": {Kernel: "/k", RootFS: "/r"}} },
		"relative kernel":  func(c *Config) { c.Images = map[string]Image{"base": {Kernel: "k", RootFS: "/r"}} },
		"unclean rootfs":   func(c *Config) { c.Images = map[string]Image{"base": {Kernel: "/k", RootFS: "/a/../r"}} },
		"relative tools":   func(c *Config) { c.Images = map[string]Image{"base": {Kernel: "/k", RootFS: "/r", Tools: "t"}} },
		"too many slots":   func(c *Config) { c.Slots = MaxSlots + 1 },
		"bad proxy port":   func(c *Config) { c.ProxyPort = 70000 },
	} {
		c := ok
		c.Images = map[string]Image{"base": {Kernel: "/k", RootFS: "/r"}}
		mutate(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The guest sees the drives in Firecracker's order and gets the same
// kernel arguments, plus the vsock port instead of an address.
func TestStageMatchesTheGuestContract(t *testing.T) {
	f := newFixture(t, nil)
	s := spec("j1")
	s.Image = "claude-code"
	s.Task = []byte(`{"prompt":"hi"}`)
	s.Tools = true
	s.Bundle = []byte("bundle")
	s.DiskMiB = 64
	s.Network = &sandbox.Network{Proxy: nopProxy{}}
	s.BootArgs = []string{"hcc.mode=results"}
	dir := filepath.Join(f.dir, "stage")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	mc, err := f.p.stage(dir, &s, f.p.cfg.Images["claude-code"])
	if err != nil {
		t.Fatal(err)
	}
	want := []Disk{
		{"/img/cc.ext4", true},
		{filepath.Join(dir, "task.img"), true},
		{"/img/tools.ext4", true},
		{filepath.Join(dir, "bundle.img"), true},
		{filepath.Join(dir, "disk.img"), false},
	}
	if len(mc.Disks) != len(want) {
		t.Fatalf("disks = %+v", mc.Disks)
	}
	for i := range want {
		if mc.Disks[i] != want[i] {
			t.Errorf("disk %d = %+v, want %+v", i, mc.Disks[i], want[i])
		}
	}
	for _, a := range []string{"console=hvc0", "root=/dev/vda", "ro", "init=/init", "panic=0", "hcc.shutdown=poweroff", "hcc.mode=results", "hcc.vsock=1024", "hcc.proxy=127.0.0.1:3128", "hcc.tools=vdc", "hcc.bundle=vdd"} {
		if !strings.Contains(" "+mc.CmdLine+" ", " "+a+" ") {
			t.Errorf("command line %q lacks %q", mc.CmdLine, a)
		}
	}
	if !mc.Vsock || mc.CPUs != 2 || mc.MemoryMiB != 512 || mc.Kernel != "/img/Image" {
		t.Errorf("machine config = %+v", mc)
	}
	task, _ := os.ReadFile(filepath.Join(dir, "task.img"))
	if len(task) != 512 || !bytes.HasPrefix(task, s.Task) {
		t.Errorf("task.img is %d bytes", len(task))
	}
	if fi, err := os.Stat(filepath.Join(dir, "disk.img")); err != nil || fi.Size() != 64<<20 || fi.Mode().Perm() != 0o600 {
		t.Errorf("disk.img = %v %v", fi, err)
	}

	// Without network there is no vsock device and no proxy argument.
	s2 := spec("j2")
	dir2 := filepath.Join(f.dir, "stage2")
	_ = os.Mkdir(dir2, 0o700)
	mc2, err := f.p.stage(dir2, &s2, f.p.cfg.Images["base"])
	if err != nil {
		t.Fatal(err)
	}
	if mc2.Vsock || strings.Contains(mc2.CmdLine, "hcc.proxy") || strings.Contains(mc2.CmdLine, "hcc.vsock") || len(mc2.Disks) != 1 {
		t.Errorf("offline machine = %+v", mc2)
	}
}

func TestRunExitsAndCollects(t *testing.T) {
	f := newFixture(t, func(m *fakeMachine) {
		_, _ = io.WriteString(m.mc.Console, "HCC-GUEST-UP\n")
		// The "guest" writes its results to the writable disk.
		_ = os.WriteFile(m.mc.Disks[len(m.mc.Disks)-1].Path, []byte("results"), 0o600)
		m.exit(nil)
	})
	s := spec("j1")
	s.DiskMiB = 1
	var got []byte
	s.Collect = func(r io.Reader) error {
		var err error
		got, err = io.ReadAll(r)
		return err
	}
	var console bytes.Buffer
	res, err := f.p.Run(context.Background(), s, &console)
	if err != nil || res.Outcome != sandbox.OutcomeExited {
		t.Fatalf("Run = %v, %v", res, err)
	}
	if string(got) != "results" || console.String() != "HCC-GUEST-UP\n" {
		t.Errorf("collected %q, console %q", got, console.String())
	}
	if _, err := os.Stat(filepath.Join(f.dir, "staging", "j1")); !os.IsNotExist(err) {
		t.Errorf("staging left behind: %v", err)
	}
}

// A VM that fails is reported, not counted as a guest that exited.
func TestRunVMError(t *testing.T) {
	f := newFixture(t, func(m *fakeMachine) { m.exit(errors.New("hypervisor error")) })
	s := spec("j1")
	s.DiskMiB = 1
	collected := false
	s.Collect = func(io.Reader) error { collected = true; return nil }
	res, err := f.p.Run(context.Background(), s, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "hypervisor error") || res.Outcome != sandbox.OutcomeFailed || collected {
		t.Errorf("Run = %v, %v, collected %v", res, err, collected)
	}
}

func TestRunTimesOutAndIsKilled(t *testing.T) {
	hang := func(*fakeMachine) {}
	f := newFixture(t, hang)
	s := spec("j1")
	s.Timeout = 50 * time.Millisecond
	res, err := f.p.Run(context.Background(), s, io.Discard)
	if err != nil || res.Outcome != sandbox.OutcomeTimedOut || !f.machines[0].stopped {
		t.Errorf("timeout: %v, %v, stopped %v", res, err, f.machines[0].stopped)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	res, err = f.p.Run(ctx, spec("j2"), io.Discard)
	if err != nil || res.Outcome != sandbox.OutcomeKilled || !f.machines[1].stopped {
		t.Errorf("kill: %v, %v", res, err)
	}
}

// A VM that does not stop keeps its slot and its files.
func TestStuckVMKeepsItsSlot(t *testing.T) {
	f := newFixture(t, func(*fakeMachine) {})
	f.p.cfg.NewMachine = func(mc MachineConfig) (Machine, error) {
		return &stuckMachine{fakeMachine{mc: mc, run: func(*fakeMachine) {}, done: make(chan struct{})}}, nil
	}
	s := spec("j1")
	s.Timeout = 20 * time.Millisecond
	if _, err := f.p.Run(context.Background(), s, io.Discard); err == nil || !strings.Contains(err.Error(), "did not stop") {
		t.Fatalf("Run = %v", err)
	}
	if _, err := f.p.Run(context.Background(), spec("j2"), io.Discard); err == nil || !strings.Contains(err.Error(), "slots are in use") {
		t.Errorf("second run = %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "staging", "j1")); err != nil {
		t.Errorf("a stuck VM's files were removed: %v", err)
	}
}

type stuckMachine struct{ fakeMachine }

func (m *stuckMachine) Stop() error { return errors.New("refused") }

func TestRunRefuses(t *testing.T) {
	f := newFixture(t, func(m *fakeMachine) { m.exit(nil) })
	for name, s := range map[string]sandbox.Spec{
		"unknown image":   {ID: "a", Image: "nope", CPUs: 1, MemoryMiB: 256, Timeout: time.Second},
		"no tool catalog": {ID: "b", Image: "base", CPUs: 1, MemoryMiB: 256, Timeout: time.Second, Tools: true},
		"invalid spec":    {ID: "../x", Image: "base", CPUs: 1, MemoryMiB: 256, Timeout: time.Second},
	} {
		if _, err := f.p.Run(context.Background(), s, io.Discard); err == nil {
			t.Errorf("%s: ran", name)
		}
	}
	if len(f.machines) != 0 {
		t.Errorf("%d machines were created", len(f.machines))
	}
	// Two runs with the same ID never share a staging directory.
	if err := os.MkdirAll(filepath.Join(f.dir, "staging", "dup"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.Run(context.Background(), spec("dup"), io.Discard); err == nil {
		t.Error("ran in an existing staging directory")
	}
}

// The proxy is served on the VM's vsock port, and its connections report
// the guest's proxy address.
func TestRunServesTheProxy(t *testing.T) {
	got := make(chan string, 1)
	seen := make(chan struct{})
	proxy := proxyFunc(func(ctx context.Context, l net.Listener) error {
		go func() {
			if c, err := l.Accept(); err == nil {
				got <- c.LocalAddr().String()
				_ = c.Close()
				close(seen)
			}
		}()
		<-ctx.Done()
		return nil
	})
	f := newFixture(t, func(m *fakeMachine) {
		// The "guest" connects to the vsock port, then shuts down.
		m.mu.Lock()
		l := m.l
		m.mu.Unlock()
		if c, err := net.Dial("tcp", l.Addr().String()); err == nil {
			_ = c.Close()
		}
		select {
		case <-seen:
		case <-time.After(2 * time.Second):
		}
		m.exit(nil)
	})
	f.p.cfg.NewMachine = func(mc MachineConfig) (Machine, error) {
		m := &fakeMachine{mc: mc, done: make(chan struct{})}
		m.run = func(m *fakeMachine) {
			// Start runs before Listen: wait for the listener.
			for {
				m.mu.Lock()
				ready := m.l != nil
				m.mu.Unlock()
				if ready {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			f.machinesRun(m)
		}
		return m, nil
	}
	s := spec("j1")
	s.Network = &sandbox.Network{Proxy: proxy}
	if res, err := f.p.Run(context.Background(), s, io.Discard); err != nil || res.Outcome != sandbox.OutcomeExited {
		t.Fatalf("Run = %v, %v", res, err)
	}
	select {
	case addr := <-got:
		if addr != "127.0.0.1:3128" {
			t.Errorf("local address = %s", addr)
		}
	default:
		t.Error("the proxy got no connection")
	}
}

func TestRecoverRemovesStaging(t *testing.T) {
	f := newFixture(t, nil)
	left := filepath.Join(f.dir, "staging", "old-job")
	if err := os.MkdirAll(left, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := f.p.Recover(context.Background(), []string{"old-job"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(left); !os.IsNotExist(err) {
		t.Errorf("left behind: %v", err)
	}
	if err := f.p.Recover(context.Background(), []string{"../escape"}); err == nil {
		t.Error("an invalid ID was accepted")
	}
}

type nopProxy struct{}

func (nopProxy) Serve(ctx context.Context, _ net.Listener) error { <-ctx.Done(); return nil }

type proxyFunc func(ctx context.Context, l net.Listener) error

func (f proxyFunc) Serve(ctx context.Context, l net.Listener) error { return f(ctx, l) }
