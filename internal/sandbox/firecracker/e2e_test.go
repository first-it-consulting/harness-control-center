//go:build e2e

// End-to-end tests against real Firecracker on the production path. Run
// on a host installed with "make deploy-runner", as the runner user, with
// the daemon stopped (the two would share VM identities):
//
//	HCC_E2E_CONFIG=/etc/hcc/runner.yaml ./fc-e2e.test -test.run E2E
//
// VMs are started through the installed hcc-helper and boot the
// installed "base" image. See make e2e.
package firecracker_test

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/credentials"
	"github.com/first-it-consulting/harness-control-center/internal/egress"
	"github.com/first-it-consulting/harness-control-center/internal/policy"
	"github.com/first-it-consulting/harness-control-center/internal/results"
	"github.com/first-it-consulting/harness-control-center/internal/runnerd"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox/firecracker"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

type e2eEnv struct {
	base, staging string
	provider      *firecracker.Provider
	network       *firecracker.NetworkConfig
}

func setupE2E(t *testing.T) *e2eEnv {
	t.Helper()
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("no /dev/kvm")
	}
	cfgPath := os.Getenv("HCC_E2E_CONFIG")
	if cfgPath == "" {
		t.Skip("HCC_E2E_CONFIG not set")
	}
	return setupHelperE2E(t, cfgPath)
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
	if _, ok := cfg.Images["base"]; !ok {
		t.Fatal(`config has no "base" image`)
	}
	p, err := firecracker.New(firecracker.Config{
		StagingBase: cfg.Firecracker.StagingDir,
		Identities:  cfg.MaxConcurrent,
		Host:        firecracker.NewHelperHost(cfg.Firecracker.Helper),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConcurrent < 2 {
		t.Fatal("maxConcurrent must be at least 2 for the concurrency test")
	}
	netCfg, err := cfg.NetworkConfig()
	if err != nil {
		t.Fatal(err)
	}
	return &e2eEnv{base: cfg.Firecracker.ChrootBase, staging: cfg.Firecracker.StagingDir, provider: p, network: netCfg}
}

func (e *e2eEnv) spec(id string, timeout time.Duration, bootArgs ...string) sandbox.Spec {
	return sandbox.Spec{
		ID: id, Image: "base",
		CPUs: 1, MemoryMiB: 128, Timeout: timeout, BootArgs: bootArgs,
	}
}

// assertNoTrace checks that a run left nothing behind on the host.
func (e *e2eEnv) assertNoTrace(t *testing.T, id string) {
	t.Helper()
	if _, err := os.Stat(firecracker.JailDir(e.base, id)); !os.IsNotExist(err) {
		t.Errorf("jail %s left behind (%v)", id, err)
	}
	if _, err := os.Stat(filepath.Join(e.staging, id)); !os.IsNotExist(err) {
		t.Errorf("staging %s left behind (%v)", id, err)
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
	if res.Duration > 3*time.Second+2*firecracker.DefaultStopGrace {
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

// A networked VM reaches exactly what its policy allows, through the
// proxy, and nothing else: no direct connections, no other host ports.
func TestE2ENetwork(t *testing.T) {
	e := setupE2E(t)
	if e.network == nil {
		t.Skip("the runner configuration has no network section")
	}
	h, err := spec.Parse([]byte(netHarness))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := policy.New(h)
	if err != nil {
		t.Fatal(err)
	}
	var proxyLog console
	id := fmt.Sprintf("e2e-net-%d", time.Now().UnixNano())
	sp := e.spec(id, time.Minute, "hcc.mode=nettest")
	// A credential for example.com: the guest must receive the job CA, and
	// the CONNECT still succeeds (the broker then expects TLS).
	ca, err := egress.NewCA([]string{"example.com"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sp.Network = &sandbox.Network{Proxy: &egress.Proxy{
		Policy:  engine,
		Blocked: []netip.Prefix{e.network.Subnet},
		JobID:   id,
		Log:     slog.New(slog.NewTextHandler(&proxyLog, nil)),
		CA:      ca,
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
	want := map[string]string{
		"HCC-NET-ALLOWED":  "200",
		"HCC-NET-DENIED":   "403",
		"HCC-NET-DIRECT":   "blocked",
		"HCC-NET-HOSTPORT": "blocked",
		"HCC-NET-CA":       "present",
	}
	for marker, expect := range want {
		line := consoleLine(out, marker)
		if !strings.Contains(line, expect) {
			t.Errorf("%s: got %q, want it to contain %q", marker, line, expect)
		}
	}
	if ifs := guestInterfaces(out); len(ifs) != 2 || ifs[0] != "eth0" || ifs[1] != "lo" {
		t.Errorf("guest interfaces = %v, want [eth0 lo]", ifs)
	}
	if !strings.Contains(proxyLog.String(), "host=example.org") {
		t.Errorf("refusal not logged:\n%s", proxyLog.String())
	}
	e.assertNoTrace(t, id)
	if links, _ := filepath.Glob("/sys/class/net/hcc*"); len(links) != 0 {
		t.Errorf("sandbox links left behind: %v", links)
	}
	t.Logf("console:\n%s", out)
}

func consoleLine(console, marker string) string {
	for _, line := range strings.Split(console, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), marker) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// The guest hands a directory back on its disk; only what passes the
// sanitizer arrives.
func TestE2EResults(t *testing.T) {
	e := setupE2E(t)
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
	if _, ok := got["link"]; ok {
		t.Error("a symlink passed")
	}
	if sum.Files != 1 || len(sum.Skipped) != 2 {
		t.Errorf("summary = %+v (want 1 file; the link and the fifo skipped)", sum)
	}
	e.assertNoTrace(t, id)
}
