package local

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/first-it-consulting/harness-control-center/internal/sandbox/vz"
)

// vm is a fake VM that prints console lines and shuts down.
type vm struct {
	mc   vz.MachineConfig
	say  string
	done chan struct{}
}

func (m *vm) Start() error {
	go func() {
		_, _ = io.WriteString(m.mc.Console, m.say)
		close(m.done)
	}()
	return nil
}
func (m *vm) Done() <-chan struct{}               { return m.done }
func (m *vm) Err() error                          { return nil }
func (m *vm) Stop() error                         { return nil }
func (m *vm) Listen(uint32) (net.Listener, error) { return nil, errors.New("no vsock") }

type fakeLaunchd struct {
	mu     sync.Mutex
	calls  []string
	loaded bool
}

func (f *fakeLaunchd) run(_ context.Context, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args[0])
	switch args[0] {
	case "bootstrap":
		f.loaded = true
	case "bootout":
		if !f.loaded {
			return errors.New("not loaded")
		}
		f.loaded = false
	case "print":
		if !f.loaded {
			return errors.New("not loaded")
		}
	}
	return nil
}

type setupFixture struct {
	opts     Options
	release  string
	launchd  *fakeLaunchd
	machines []vz.MachineConfig
	say      string
	steps    []Step
	pkg      []byte
}

func newSetup(t *testing.T) *setupFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the local runner is for macOS; its file checks need a Unix file system")
	}
	dir := t.TempDir()
	f := &setupFixture{release: filepath.Join(dir, "release"), launchd: &fakeLaunchd{}, pkg: []byte("claude code package")}
	f.say = "HCC-INSTALL-OK 9.9.9 (Claude Code)\n"
	if err := os.Mkdir(f.release, 0o700); err != nil {
		t.Fatal(err)
	}
	var files []File
	for name, content := range map[string][]byte{
		"vz-kernel-arm64.gz":           gz(t, []byte("kernel")),
		"vz-base-arm64.ext4.gz":        gz(t, []byte("base")),
		"vz-claude-code-arm64.ext4.gz": gz(t, []byte("claude-code image")),
	} {
		if err := os.WriteFile(filepath.Join(f.release, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
		s := sha256.Sum256(content)
		files = append(files, File{Name: name, SHA256: hex.EncodeToString(s[:])})
	}
	npm := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(f.pkg) }))
	t.Cleanup(npm.Close)
	s := sha512.Sum512(f.pkg)
	f.opts = Options{
		Layout:     Layout{Dir: filepath.Join(dir, "local")},
		Source:     &Source{Dir: f.release, Client: npm.Client()},
		Version:    "1.2.3",
		Executable: "/opt/hcc/bin/hcc",
		Service:    Service{Plist: filepath.Join(dir, "LaunchAgents", ServiceLabel+".plist"), Launchctl: f.launchd.run},
		Progress:   func(s Step) { f.steps = append(f.steps, s) },
		NewMachine: func(mc vz.MachineConfig) (vz.Machine, error) {
			f.machines = append(f.machines, mc)
			return &vm{mc: mc, say: f.say, done: make(chan struct{})}, nil
		},
		supported:  func() error { return nil },
		images:     func() ([]File, error) { return files, nil },
		claude:     &claudePin{Version: "9.9.9", URL: npm.URL + "/claude.tgz", Integrity: Check("sha512-" + base64.StdEncoding.EncodeToString(s[:]))},
		waitRunner: func(context.Context, string) error { return nil },
	}
	return f
}

func (f *setupFixture) final(id string) Step {
	var last Step
	for _, s := range f.steps {
		if s.ID == id {
			last = s
		}
	}
	return last
}

func TestUpSetsEverythingUp(t *testing.T) {
	f := newSetup(t)
	if err := Up(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	l := f.opts.Layout
	for p, want := range map[string]string{l.kernel(): "kernel", l.baseRootFS(): "base", l.claudeRootFS(): "claude-code image"} {
		if got, err := os.ReadFile(p); err != nil || string(got) != want {
			t.Errorf("%s: %q, %v", p, got, err)
		}
	}
	// Claude Code was added in a VM: base read-only as root, the package
	// read-only, the new image writable; no network.
	if len(f.machines) != 1 {
		t.Fatalf("%d VMs", len(f.machines))
	}
	mc := f.machines[0]
	if len(mc.Disks) != 3 || !mc.Disks[0].ReadOnly || mc.Disks[0].Path != l.baseRootFS() || !mc.Disks[1].ReadOnly || mc.Disks[2].ReadOnly || mc.Vsock {
		t.Errorf("VM %+v", mc)
	}
	if !strings.Contains(mc.CmdLine, "hcc.mode=install-claude hcc.size="+strconv.Itoa(len(f.pkg))) {
		t.Errorf("command line %q", mc.CmdLine)
	}
	if _, err := os.Stat(l.downloads()); !errors.Is(err, os.ErrNotExist) {
		t.Error("the download was not cleaned up")
	}
	// The runner's settings and its service.
	cfg, err := os.ReadFile(l.Config())
	if err != nil || !strings.Contains(string(cfg), "provider: vz") || !strings.Contains(string(cfg), "target: local") {
		t.Fatalf("runner.yaml: %s %v", cfg, err)
	}
	if fi, _ := os.Stat(l.Config()); fi.Mode().Perm() != 0o600 {
		t.Errorf("runner.yaml mode %v", fi.Mode().Perm())
	}
	if !f.launchd.loaded {
		t.Error("service not started")
	}
	for _, id := range []string{"images", "config", "service"} {
		if s := f.final(id); s.State != "done" {
			t.Errorf("step %s: %+v", id, s)
		}
	}
}

func TestUpAgainSkipsWhatIsDone(t *testing.T) {
	f := newSetup(t)
	if err := Up(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	f.steps = nil
	f.launchd.calls = nil
	if err := Up(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"images", "config", "service"} {
		if s := f.final(id); s.State != "skipped" {
			t.Errorf("step %s: %+v", id, s)
		}
	}
	if len(f.machines) != 1 || slices.Contains(f.launchd.calls, "bootstrap") {
		t.Errorf("redone: %d VMs, launchctl %v", len(f.machines), f.launchd.calls)
	}
}

func TestUpKeepsTheUsersSettings(t *testing.T) {
	f := newSetup(t)
	if err := Up(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	l := f.opts.Layout
	data, _ := os.ReadFile(l.Config())
	mine := strings.Replace(string(data), "maxConcurrent: 2", "maxConcurrent: 3", 1)
	if err := os.WriteFile(l.Config(), []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Up(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(l.Config()); string(got) != mine {
		t.Error("runner.yaml was overwritten")
	}
}

func TestUpNewClaudeCodeRedoesTheImage(t *testing.T) {
	f := newSetup(t)
	if err := Up(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	f.opts.claude.Version = "9.9.10"
	f.say = "HCC-INSTALL-OK 9.9.10 (Claude Code)\n"
	if err := Up(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	if len(f.machines) != 2 {
		t.Errorf("%d VMs", len(f.machines))
	}
	if got := f.opts.Layout.readInstalled().ClaudeCode; got != "9.9.10" {
		t.Errorf("installed Claude Code %q", got)
	}
}

func TestUpRefusesATamperedImage(t *testing.T) {
	f := newSetup(t)
	if err := os.WriteFile(filepath.Join(f.release, "vz-base-arm64.ext4.gz"), gz(t, []byte("evil")), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Up(context.Background(), f.opts)
	if err == nil || !strings.Contains(err.Error(), "CHECKSUM MISMATCH") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(f.opts.Layout.baseRootFS()); !errors.Is(err, os.ErrNotExist) {
		t.Error("a tampered image was installed")
	}
	if f.final("images").State != "failed" || f.launchd.loaded {
		t.Error("setup went on")
	}
}

func TestUpRefusesATamperedClaudeCode(t *testing.T) {
	f := newSetup(t)
	f.pkg = []byte("something else")
	if err := Up(context.Background(), f.opts); err == nil || !strings.Contains(err.Error(), "CHECKSUM MISMATCH") {
		t.Fatalf("got %v", err)
	}
	l := f.opts.Layout
	if len(f.machines) != 0 || exists(l.claudeRootFS()) || exists(l.claudeRootFS()+".new") {
		t.Error("an image without a verified Claude Code was kept")
	}
}

func TestUpFailsWhenTheInstallVMFails(t *testing.T) {
	f := newSetup(t)
	f.say = "HCC-INSTALL-ERROR could not add Claude Code to the image\n"
	if err := Up(context.Background(), f.opts); err == nil || !strings.Contains(err.Error(), "could not add") {
		t.Fatalf("got %v", err)
	}
	if exists(f.opts.Layout.claudeRootFS()) {
		t.Error("a broken image was kept")
	}
	// No marker at all is a failure too.
	f = newSetup(t)
	f.say = "kernel panic\n"
	if err := Up(context.Background(), f.opts); err == nil {
		t.Fatal("accepted a VM that said nothing")
	}
}

func TestUpInADevelopmentBuild(t *testing.T) {
	f := newSetup(t)
	f.opts.images = func() ([]File, error) { return nil, ErrNoImages }
	if err := Up(context.Background(), f.opts); err == nil || !strings.Contains(err.Error(), "build-images.sh") {
		t.Fatalf("got %v", err)
	}
	// With images built on this Mac, it uses them.
	l := f.opts.Layout
	for _, p := range []string{l.kernel(), l.baseRootFS(), l.claudeRootFS()} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Up(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	if f.final("images").State != "skipped" || len(f.machines) != 0 {
		t.Errorf("images %+v, %d VMs", f.final("images"), len(f.machines))
	}
}

func TestUpNeedsAppleSilicon(t *testing.T) {
	f := newSetup(t)
	f.opts.supported = nil
	err := Up(context.Background(), f.opts)
	if (runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") != (err == nil || !strings.Contains(err.Error(), "Apple Silicon")) {
		t.Errorf("got %v", err)
	}
}

func TestPlist(t *testing.T) {
	l := Layout{Dir: "/Users/a&b/Library/Application Support/hcc/local"}
	p := plist("/opt/hcc <1>/hcc", l)
	dec := xml.NewDecoder(strings.NewReader(string(p)))
	dec.Strict = true
	var strs []string
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("invalid XML: %v\n%s", err, p)
		}
		if cd, ok := tok.(xml.CharData); ok && strings.TrimSpace(string(cd)) != "" {
			strs = append(strs, string(cd))
		}
	}
	want := []string{"Label", ServiceLabel, "ProgramArguments", "/opt/hcc <1>/hcc", "runner", "serve", "--config", l.Config()}
	if len(strs) < len(want) || !slices.Equal(strs[:len(want)], want) {
		t.Errorf("strings %q", strs)
	}
}

func TestServiceStop(t *testing.T) {
	dir := t.TempDir()
	fl := &fakeLaunchd{}
	s := Service{Plist: filepath.Join(dir, "x.plist"), Launchctl: fl.run}
	if _, err := s.Install(context.Background(), "/bin/hcc", Layout{Dir: dir}, false); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(s.Plist); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o644) {
		t.Fatalf("plist: %v %v", fi, err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fl.loaded || exists(s.Plist) {
		t.Error("still installed")
	}
	// Stopping what is not running is fine.
	if err := s.Stop(context.Background()); err != nil {
		t.Error(err)
	}
}
