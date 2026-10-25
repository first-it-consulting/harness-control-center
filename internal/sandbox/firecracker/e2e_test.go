//go:build e2e

// End-to-end tests against real Firecracker. They need Linux with
// /dev/kvm, non-interactive sudo, and HCC_E2E_ASSETS pointing to a
// directory with: firecracker, jailer, vmlinux, alpine-minirootfs.tar.gz
// (as produced by hack/firecracker-poc/run.sh in ~/.cache/hcc-poc).
//
//	go test -tags e2e -run E2E ./internal/sandbox/firecracker
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
	assets := os.Getenv("HCC_E2E_ASSETS")
	if assets == "" {
		t.Skip("HCC_E2E_ASSETS not set")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("no /dev/kvm")
	}
	dir := t.TempDir()
	images := filepath.Join(dir, "images")
	if err := os.Mkdir(images, 0o700); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "cp", filepath.Join(assets, "vmlinux"), filepath.Join(images, "vmlinux"))
	rootfs := buildRootFS(t, filepath.Join(assets, "alpine-minirootfs.tar.gz"), dir, images)
	base := filepath.Join(dir, "jails")
	p, err := New(Config{
		JailerPath:      filepath.Join(assets, "jailer"),
		FirecrackerPath: filepath.Join(assets, "firecracker"),
		ChrootBase:      base,
		ImageRoot:       images,
		UID:             64000, GID: 64000,
		Host: NewSudoHost(base),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &e2eEnv{images: images, base: base, rootfs: rootfs, provider: p}
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
	if !strings.Contains(out, "HCC-GUEST-NET lo ") {
		t.Errorf("guest must see only the loopback interface:\n%s", out)
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

func TestE2EConcurrent(t *testing.T) {
	e := setupE2E(t)
	const n = 2
	var wg sync.WaitGroup
	errs := make(chan error, n)
	ids := make([]string, n)
	for i := range n {
		ids[i] = fmt.Sprintf("e2e-par-%d-%d", i, time.Now().UnixNano())
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			var c console
			res, err := e.provider.Run(context.Background(), e.spec(id, 30*time.Second), &c)
			if err == nil && res.Outcome != sandbox.OutcomeExited {
				err = fmt.Errorf("outcome %v", res.Outcome)
			}
			if err != nil {
				errs <- fmt.Errorf("%s: %w\n%s", id, err, c.String())
			}
		}(ids[i])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for _, id := range ids {
		e.assertNoTrace(t, id)
	}
}
