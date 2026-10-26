//go:build vz_e2e && darwin && cgo

// End-to-end tests with real VMs of Apple's Virtualization framework,
// booting the images hack/vz/build-images.sh built (make e2e-vz). They
// mirror the Firecracker provider's: the guest contract is the same.
package vz_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/credentials"
	"github.com/first-it-consulting/harness-control-center/internal/egress"
	"github.com/first-it-consulting/harness-control-center/internal/policy"
	"github.com/first-it-consulting/harness-control-center/internal/results"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox/vz"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
	"github.com/first-it-consulting/harness-control-center/internal/tools"
)

type env struct {
	images, staging string
	provider        *vz.Provider
}

func setup(t *testing.T) *env {
	t.Helper()
	dir := os.Getenv("HCC_VZ_IMAGES")
	if dir == "" {
		t.Skip("HCC_VZ_IMAGES not set (make e2e-vz)")
	}
	kernel := filepath.Join(dir, "kernel", "Image")
	images := map[string]vz.Image{
		"base":        {Kernel: kernel, RootFS: filepath.Join(dir, "base", "rootfs.ext4")},
		"claude-code": {Kernel: kernel, RootFS: filepath.Join(dir, "claude-code", "rootfs.ext4")},
	}
	if _, err := os.Stat(filepath.Join(dir, "claude-code", "tools.ext4")); err == nil {
		img := images["claude-code"]
		img.Tools = filepath.Join(dir, "claude-code", "tools.ext4")
		images["claude-code"] = img
	}
	staging := filepath.Join(t.TempDir(), "staging")
	p, err := vz.New(vz.Config{StagingBase: staging, Images: images, Slots: 4})
	if err != nil {
		t.Fatal(err)
	}
	return &env{images: dir, staging: staging, provider: p}
}

func (e *env) spec(id string, timeout time.Duration, bootArgs ...string) sandbox.Spec {
	return sandbox.Spec{ID: id, Image: "base", CPUs: 1, MemoryMiB: 256, Timeout: timeout, BootArgs: bootArgs}
}

func (e *env) assertNoTrace(t *testing.T, id string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(e.staging, id)); !os.IsNotExist(err) {
		t.Errorf("staging %s left behind (%v)", id, err)
	}
}

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

func line(console, marker string) string {
	for _, l := range strings.Split(console, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), marker) {
			return strings.TrimSpace(l)
		}
	}
	return ""
}

func interfaces(console string) []string {
	rest := strings.TrimPrefix(line(console, "HCC-GUEST-NET"), "HCC-GUEST-NET")
	return strings.Fields(strings.TrimPrefix(strings.TrimSpace(rest), "interfaces:"))
}

func TestE2EExit(t *testing.T) {
	e := setup(t)
	id := fmt.Sprintf("e2e-exit-%d", time.Now().UnixNano())
	var c console
	res, err := e.provider.Run(context.Background(), e.spec(id, 30*time.Second), &c)
	if err != nil || res.Outcome != sandbox.OutcomeExited {
		t.Fatalf("Run = %v, %v\nconsole:\n%s", res.Outcome, err, c.String())
	}
	out := c.String()
	if !strings.Contains(out, "HCC-GUEST-UP") || !strings.Contains(out, "HCC-GUEST-DONE") {
		t.Errorf("guest did not run through:\n%s", out)
	}
	// No network device at all, not even without network access.
	if ifs := interfaces(out); len(ifs) != 1 || ifs[0] != "lo" {
		t.Errorf("guest must see only the loopback interface, saw %v", ifs)
	}
	t.Logf("exited after %s\n%s", res.Duration, out)
	e.assertNoTrace(t, id)
}

func TestE2ETimeout(t *testing.T) {
	e := setup(t)
	id := fmt.Sprintf("e2e-timeout-%d", time.Now().UnixNano())
	var c console
	res, err := e.provider.Run(context.Background(), e.spec(id, 3*time.Second, "hcc.mode=hang"), &c)
	if err != nil || res.Outcome != sandbox.OutcomeTimedOut {
		t.Fatalf("Run = %v, %v\n%s", res.Outcome, err, c.String())
	}
	if !strings.Contains(c.String(), "HCC-GUEST-HANG") {
		t.Errorf("guest never reached its hang:\n%s", c.String())
	}
	e.assertNoTrace(t, id)
}

func TestE2EKillSwitch(t *testing.T) {
	e := setup(t)
	id := fmt.Sprintf("e2e-kill-%d", time.Now().UnixNano())
	var c console
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		c.waitFor("HCC-GUEST-HANG", 30*time.Second)
		cancel()
	}()
	res, err := e.provider.Run(ctx, e.spec(id, time.Minute, "hcc.mode=hang"), &c)
	if err != nil || res.Outcome != sandbox.OutcomeKilled {
		t.Fatalf("Run = %v, %v\n%s", res.Outcome, err, c.String())
	}
	e.assertNoTrace(t, id)
}

func TestE2EConcurrent(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	consoles := []*console{{}, {}}
	errs := make([]error, 2)
	outcomes := make([]sandbox.Outcome, 2)
	for i := range consoles {
		id := fmt.Sprintf("e2e-conc%d-%d", i, time.Now().UnixNano())
		wg.Go(func() {
			res, err := e.provider.Run(ctx, e.spec(id, time.Minute, "hcc.mode=hang"), consoles[i])
			outcomes[i], errs[i] = res.Outcome, err
		})
	}
	for i, c := range consoles {
		if !c.waitFor("HCC-GUEST-HANG", 30*time.Second) {
			t.Errorf("VM %d did not come up:\n%s", i, c.String())
		}
	}
	cancel()
	wg.Wait()
	for i := range consoles {
		if errs[i] != nil || outcomes[i] != sandbox.OutcomeKilled {
			t.Errorf("VM %d: %v, %v", i, outcomes[i], errs[i])
		}
	}
}

const netHarness = `
apiVersion: hcc/v1alpha1
kind: Harness
metadata: {name: e2e-net}
spec:
  runtime: claude-code
  prompt: probe the network
  permissions:
    network:
      egress:
        - host: example.com
`

// The guest reaches exactly what its policy allows, through the proxy on
// vsock, and nothing else: it has no network device, so there is nothing
// to connect to directly.
func TestE2ENetwork(t *testing.T) {
	e := setup(t)
	h, err := spec.Parse([]byte(netHarness))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := policy.New(h)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := egress.NewCA([]string{"example.com"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var proxyLog console
	id := fmt.Sprintf("e2e-net-%d", time.Now().UnixNano())
	sp := e.spec(id, time.Minute, "hcc.mode=nettest")
	sp.Network = &sandbox.Network{Proxy: &egress.Proxy{
		Policy: engine,
		JobID:  id,
		Log:    slog.New(slog.NewTextHandler(&proxyLog, nil)),
		CA:     ca,
		Credentials: map[string]credentials.Credential{
			"example.com": {Name: "e2e", Hosts: []string{"example.com"}, Scheme: "https", Port: 443, Header: "x-e2e", Value: "e2e-secret"},
		},
	}}
	var c console
	res, err := e.provider.Run(context.Background(), sp, &c)
	if err != nil || res.Outcome != sandbox.OutcomeExited {
		t.Fatalf("Run = %v, %v\nconsole:\n%s", res.Outcome, err, c.String())
	}
	out := c.String()
	for marker, expect := range map[string]string{
		"HCC-NET-ALLOWED":  "200",
		"HCC-NET-DENIED":   "403",
		"HCC-NET-DIRECT":   "blocked",
		"HCC-NET-HOSTPORT": "blocked",
		"HCC-NET-CA":       "present",
	} {
		if l := line(out, marker); !strings.Contains(l, expect) {
			t.Errorf("%s: got %q, want it to contain %q", marker, l, expect)
		}
	}
	if ifs := interfaces(out); len(ifs) != 1 || ifs[0] != "lo" {
		t.Errorf("guest interfaces = %v, want only lo", ifs)
	}
	if !strings.Contains(proxyLog.String(), "host=example.org") {
		t.Errorf("refusal not logged:\n%s", proxyLog.String())
	}
	e.assertNoTrace(t, id)
	t.Logf("console:\n%s", out)
}

func TestE2EResults(t *testing.T) {
	e := setup(t)
	id := fmt.Sprintf("e2e-results-%d", time.Now().UnixNano())
	s := e.spec(id, 30*time.Second, "hcc.mode=results")
	s.DiskMiB = 64
	var archive bytes.Buffer
	var sum results.Summary
	s.Collect = func(disk io.Reader) error {
		var err error
		sum, err = results.Sanitize(&archive, disk, results.Limits{MaxBytes: 64 << 20})
		return err
	}
	var c console
	res, err := e.provider.Run(context.Background(), s, &c)
	if err != nil || res.Outcome != sandbox.OutcomeExited {
		t.Fatalf("Run = %v, %v\nconsole:\n%s", res.Outcome, err, c.String())
	}
	if !strings.Contains(c.String(), "HCC-RESULTS saved") {
		t.Fatalf("guest did not save results:\n%s", c.String())
	}
	got := map[string]string{}
	tr := tar.NewReader(&archive)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(tr)
		got[h.Name] = string(b)
	}
	if got["sub/hello.txt"] != "hello from the guest\n" {
		t.Errorf("archive = %v", got)
	}
	if sum.Files != 1 || len(sum.Skipped) != 2 {
		t.Errorf("summary = %+v (want 1 file; the link and the fifo skipped)", sum)
	}
	e.assertNoTrace(t, id)
}

// A tool from the claude-code image's catalog is installed offline before
// the agent starts (stopped here: it has no model to talk to).
func TestE2ETools(t *testing.T) {
	e := setup(t)
	f, err := os.Open(filepath.Join(e.images, "claude-code", "tools.txt"))
	if err != nil {
		t.Skip("the claude-code image has no tool catalog")
	}
	cat, err := tools.Parse(f)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	pkgs, files, err := cat.Resolve([]string{"make"})
	if err != nil {
		t.Skipf("catalog offers no make: %v", err)
	}
	task, _ := json.Marshal(map[string]any{
		"runtime": "claude-code", "prompt": "hi",
		"tools": map[string]any{"packages": pkgs, "files": files},
	})
	id := fmt.Sprintf("e2e-tools-%d", time.Now().UnixNano())
	s := sandbox.Spec{ID: id, Image: "claude-code", CPUs: 1, MemoryMiB: 1024, Timeout: 2 * time.Minute, Task: task, Tools: true}
	var c console
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := e.provider.Run(ctx, s, &c); done <- err }()
	ok := c.waitFor("HCC-TOOLS installed: make", 90*time.Second)
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
	if !ok || strings.Contains(c.String(), "HCC-TOOLS-ERROR") {
		t.Fatalf("make was not installed:\n%s", c.String())
	}
	e.assertNoTrace(t, id)
}
