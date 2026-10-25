//go:build e2e

// End-to-end tests against real Firecracker, on Linux with /dev/kvm, in
// one of two modes:
//
//   - HCC_E2E_CONFIG=/etc/hcc/runner.yaml: the production path. Run as the
//     configured runner user (with the daemon stopped, so the two do not
//     share identities); VMs are started through the installed hcc-helper
//     and boot the installed "base" image. See make e2e-helper.
//   - HCC_E2E_ASSETS=<dir with firecracker, jailer, vmlinux,
//     alpine-minirootfs.tar.gz>: development path through broad sudo
//     (SudoHost), with images built in a temp dir. See make e2e-firecracker.
package firecracker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/runnerd"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
)

// The guest init exits by default and hangs with hcc.mode=hang.
const e2eInit = `#!/bin/sh
mount -t proc proc /proc
mount -t sysfs sys /sys
echo "HCC-GUEST-UP"
echo "HCC-GUEST-NET $(ls /sys/class/net | tr '\n' ' ')"
case " $(cat /proc/cmdline) " in
*" hcc.mode=hang "*) echo "HCC-GUEST-HANG"; while :; do sleep 1; done ;;
esac
reboot -f
`

type e2eEnv struct {
	images, base, rootfs string
	provider             *Provider
}

func setupE2E(t *testing.T) *e2eEnv {
	t.Helper()
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("no /dev/kvm")
	}
	if cfgPath := os.Getenv("HCC_E2E_CONFIG"); cfgPath != "" {
		return setupHelperE2E(t, cfgPath)
	}
	assets := os.Getenv("HCC_E2E_ASSETS")
	if assets == "" {
		t.Skip("neither HCC_E2E_CONFIG nor HCC_E2E_ASSETS is set")
	}
	dir := t.TempDir()
	images := filepath.Join(dir, "images")
	if err := os.Mkdir(images, 0o700); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "cp", filepath.Join(assets, "vmlinux"), filepath.Join(images, "vmlinux"))
	rootfs := buildRootFS(t, filepath.Join(assets, "alpine-minirootfs.tar.gz"), dir, images)
	base := filepath.Join(dir, "jails")
	jailer := JailerConfig{
		JailerPath:      filepath.Join(assets, "jailer"),
		FirecrackerPath: filepath.Join(assets, "firecracker"),
		ChrootBase:      base,
		UID:             64000, GID: 64000, Identities: 2,
		ForbiddenIDs: []int{0, os.Getuid(), os.Getgid()},
	}
	if err := jailer.Validate(); err != nil {
		t.Fatal(err)
	}
	p, err := New(Config{ChrootBase: base, ImageRoot: images, Identities: 2, Host: NewSudoHost(jailer)})
	if err != nil {
		t.Fatal(err)
	}
	return &e2eEnv{images: images, base: base, rootfs: rootfs, provider: p}
}

// setupHelperE2E builds the provider exactly as the daemon does from an
// installed configuration.
func setupHelperE2E(t *testing.T, cfgPath string) *e2eEnv {
	t.Helper()
	cfg, err := runnerd.LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Firecracker.Helper == "" {
		t.Fatal("config has no firecracker.helper")
	}
	img, ok := cfg.Images["base"]
	if !ok {
		t.Fatal(`config has no "base" image`)
	}
	if filepath.Base(img.Kernel) != "vmlinux" {
		t.Fatalf("base kernel must be named vmlinux, got %s", img.Kernel)
	}
	p, err := New(Config{
		ChrootBase: cfg.Firecracker.ChrootBase,
		ImageRoot:  cfg.Firecracker.ImageRoot,
		Identities: cfg.MaxConcurrent,
		Host:       NewHelperHost(cfg.Firecracker.Helper),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConcurrent < 2 {
		t.Fatal("maxConcurrent must be at least 2 for the concurrency test")
	}
	return &e2eEnv{images: filepath.Dir(img.Kernel), base: cfg.Firecracker.ChrootBase, rootfs: img.RootFS, provider: p}
}

func buildRootFS(t *testing.T, tarball, dir, images string) string {
	t.Helper()
	tree := filepath.Join(dir, "tree")
	img := filepath.Join(images, "rootfs.ext4")
	if err := os.Mkdir(tree, 0o755); err != nil { //nolint:gosec // becomes the guest's "/" and must be world-traversable
		t.Fatal(err)
	}
	mustRun(t, "tar", "-xzf", tarball, "-C", tree)
	if err := os.WriteFile(filepath.Join(tree, "init"), []byte(e2eInit), 0o755); err != nil { //nolint:gosec // the guest's init must be executable
		t.Fatal(err)
	}
	mustRun(t, "mkfs.ext4", "-q", "-F", "-d", tree, "-E", "root_owner=0:0", img, "64M")
	return img
}

func mustRun(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func (e *e2eEnv) spec(id string, timeout time.Duration, bootArgs ...string) sandbox.Spec {
	return sandbox.Spec{
		ID: id, Kernel: filepath.Join(e.images, "vmlinux"), RootFS: e.rootfs,
		CPUs: 1, MemoryMiB: 128, Timeout: timeout, BootArgs: bootArgs,
	}
}

// assertNoTrace checks that a run left nothing behind on the host.
func (e *e2eEnv) assertNoTrace(t *testing.T, id string) {
	t.Helper()
	if _, err := os.Stat(jailDir(e.base, id)); !os.IsNotExist(err) {
		t.Errorf("jail %s left behind", id)
	}
	if _, err := os.Stat(filepath.Join("/sys/fs/cgroup/firecracker", id)); !os.IsNotExist(err) {
		t.Errorf("cgroup %s left behind", id)
	}
	if out, _ := exec.Command("pgrep", "-f", "--id "+id).Output(); len(bytes.TrimSpace(out)) > 0 {
		t.Errorf("processes for %s still running: %s", id, out)
	}
}

// console is a thread-safe buffer that can wait for a marker.
type console struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *console) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *console) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *console) waitFor(marker string, timeout time.Duration) bool {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if strings.Contains(c.String(), marker) {
			return true
		}
	}
	return false
}

func TestE2EExit(t *testing.T) {
	e := setupE2E(t)
	id := fmt.Sprintf("e2e-exit-%d", time.Now().UnixNano())
	var c console
	res, err := e.provider.Run(context.Background(), e.spec(id, 30*time.Second), &c)
	if err != nil || res.Outcome != sandbox.OutcomeExited {
		t.Fatalf("Run = %v, %v\nconsole:\n%s", res.Outcome, err, c.String())
	}
	out := c.String()
	if !strings.Contains(out, "HCC-GUEST-UP") {
		t.Errorf("guest never came up:\n%s", out)
	}
	if ifs := guestInterfaces(out); len(ifs) != 1 || ifs[0] != "lo" {
		t.Errorf("guest must see only the loopback interface, saw %v:\n%s", ifs, out)
	}
	t.Logf("exited after %s", res.Duration)
	e.assertNoTrace(t, id)
}

func TestE2ETimeout(t *testing.T) {
	e := setupE2E(t)
	id := fmt.Sprintf("e2e-timeout-%d", time.Now().UnixNano())
	var c console
	res, err := e.provider.Run(context.Background(), e.spec(id, 3*time.Second, "hcc.mode=hang"), &c)
	if err != nil || res.Outcome != sandbox.OutcomeTimedOut {
		t.Fatalf("Run = %v, %v\nconsole:\n%s", res.Outcome, err, c.String())
	}
	if !strings.Contains(c.String(), "HCC-GUEST-HANG") {
		t.Errorf("guest did not reach the hang:\n%s", c.String())
	}
	if res.Duration > 3*time.Second+2*DefaultStopGrace {
		t.Errorf("stopping took too long: %s", res.Duration)
	}
	t.Logf("timed out and stopped after %s", res.Duration)
	e.assertNoTrace(t, id)
}

func TestE2EKillSwitch(t *testing.T) {
	e := setupE2E(t)
	id := fmt.Sprintf("e2e-kill-%d", time.Now().UnixNano())
	var c console
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if c.waitFor("HCC-GUEST-HANG", 20*time.Second) {
			cancel()
		}
	}()
	start := time.Now()
	res, err := e.provider.Run(ctx, e.spec(id, time.Minute, "hcc.mode=hang"), &c)
	if err != nil || res.Outcome != sandbox.OutcomeKilled {
		t.Fatalf("Run = %v, %v\nconsole:\n%s", res.Outcome, err, c.String())
	}
	if time.Since(start) > 30*time.Second {
		t.Errorf("kill took %s", time.Since(start))
	}
	e.assertNoTrace(t, id)
}

// Two VMs at once, each under its own UID/GID, verified on the host.
func TestE2EConcurrent(t *testing.T) {
	e := setupE2E(t)
	const n = 2
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	consoles := make([]*console, n)
	ids := make([]string, n)
	for i := range n {
		ids[i] = fmt.Sprintf("e2e-par-%d-%d", i, time.Now().UnixNano())
		consoles[i] = &console{}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := e.provider.Run(ctx, e.spec(ids[i], time.Minute, "hcc.mode=hang"), consoles[i])
			if err == nil && res.Outcome != sandbox.OutcomeKilled {
				err = fmt.Errorf("outcome %v", res.Outcome)
			}
			if err != nil {
				errs <- fmt.Errorf("%s: %w\n%s", ids[i], err, consoles[i].String())
			}
		}(i)
	}
	for i := range n {
		if !consoles[i].waitFor("HCC-GUEST-HANG", 20*time.Second) {
			t.Fatalf("%s never came up:\n%s", ids[i], consoles[i].String())
		}
	}
	out, err := exec.Command("ps", "-o", "uid=,gid=", "-C", "firecracker").Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	pairs := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pairs[strings.Join(strings.Fields(line), ":")] = true
	}
	if len(pairs) != n || !pairs["64000:64000"] || !pairs["64001:64001"] {
		t.Errorf("VMM identities = %v, want 64000:64000 and 64001:64001", pairs)
	}
	cancel()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for _, id := range ids {
		e.assertNoTrace(t, id)
	}
}

// guestInterfaces parses the guest's "HCC-GUEST-NET [interfaces:] ..." line.
func guestInterfaces(console string) []string {
	for _, line := range strings.Split(console, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "HCC-GUEST-NET"); ok {
			rest = strings.TrimPrefix(strings.TrimSpace(rest), "interfaces:")
			return strings.Fields(rest)
		}
	}
	return nil
}
