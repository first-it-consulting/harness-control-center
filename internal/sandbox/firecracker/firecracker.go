// Package firecracker runs sandboxes as Firecracker microVMs under the
// jailer (ADR-0001). The provider itself is unprivileged; everything that
// needs root goes through Host.
//
// Per run, the provider:
//  1. stages the kernel, root filesystem and VM configuration in
//     <StagingBase>/<id> (its own directory),
//  2. asks its Host to start the VM: root-side, hcc-helper builds a
//     root-owned jail from the staged files, then the jailer applies
//     cgroup v2 limits, pivot_roots into a private mount namespace, drops
//     to the slot's UID/GID and execs Firecracker,
//  3. stops the VM on timeout or cancellation (SIGTERM, then
//     cgroup.kill), and
//  4. always removes the staging directory, jail and cgroup.
package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
)

// Defaults.
const (
	DefaultStopGrace = 5 * time.Second
	// MemoryOverheadMiB is added to the guest memory for the cgroup limit,
	// covering the VMM itself.
	MemoryOverheadMiB = 64
	// MaxPids bounds the processes in a sandbox's cgroup (VMM + vCPU and
	// I/O threads).
	MaxPids = 64
	// MaxIdentities bounds Config.Identities.
	MaxIdentities = 1024
)

// Fixed kernel parameters. Callers cannot override these (see
// sandbox.Spec.BootArgs).
var baseBootArgs = []string{"console=ttyS0", "quiet", "loglevel=1", "reboot=k", "panic=1", "pci=off", "ro", "init=/init"}

// Config configures the provider. Which binaries run and under which
// UIDs is decided root-side (JailerConfig); the provider only picks a
// free identity slot per VM.
type Config struct {
	// StagingBase is the runner's own directory for preparing VM files;
	// the Host copies them into a root-owned jail. It must match the
	// Host's configuration.
	StagingBase string
	// ImageRoot bounds which files can become a guest's kernel or root
	// filesystem: Spec.Kernel and Spec.RootFS must resolve (symlinks
	// included) to regular files inside it.
	ImageRoot string
	// Identities is how many VMs can run at once (default 1); it must not
	// exceed the Host's.
	Identities int
	// StopGrace is how long to wait after SIGTERM, and again after
	// cgroup.kill, before giving up.
	StopGrace time.Duration
	Host      Host
}

// Provider implements sandbox.Provider with Firecracker.
type Provider struct {
	cfg Config

	mu    sync.Mutex
	inUse []bool // identity slots; a slot whose VM could not be stopped stays taken
}

var _ sandbox.Provider = (*Provider)(nil)

// New validates cfg and returns a provider.
func New(cfg Config) (*Provider, error) {
	var errs []error
	for name, p := range map[string]string{"StagingBase": cfg.StagingBase, "ImageRoot": cfg.ImageRoot} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			errs = append(errs, fmt.Errorf("%s must be a clean absolute path, got %q", name, p))
		}
	}
	if cfg.Identities == 0 {
		cfg.Identities = 1
	}
	if cfg.Identities < 0 || cfg.Identities > MaxIdentities {
		errs = append(errs, fmt.Errorf("identities must be between 1 and %d", MaxIdentities))
	}
	if cfg.Host == nil {
		errs = append(errs, errors.New("Host is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("firecracker: invalid config: %w", err)
	}
	if cfg.StopGrace <= 0 {
		cfg.StopGrace = DefaultStopGrace
	}
	return &Provider{cfg: cfg, inUse: make([]bool, cfg.Identities)}, nil
}

// acquire reserves a free identity slot.
func (p *Provider) acquire() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, used := range p.inUse {
		if !used {
			p.inUse[i] = true
			return i, nil
		}
	}
	return 0, fmt.Errorf("firecracker: all %d sandbox identities are in use", len(p.inUse))
}

func (p *Provider) release(slot int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inUse[slot] = false
}

// Name implements sandbox.Provider.
func (p *Provider) Name() string { return "firecracker" }

// Run implements sandbox.Provider.
func (p *Provider) Run(ctx context.Context, spec sandbox.Spec, console io.Writer) (res sandbox.Result, err error) {
	start := time.Now()
	res.Outcome = sandbox.OutcomeFailed
	if err := spec.Validate(); err != nil {
		return res, err
	}
	kernel, err := p.openImage(spec.Kernel)
	if err != nil {
		return res, err
	}
	defer kernel.Close()
	rootfs, err := p.openImage(spec.RootFS)
	if err != nil {
		return res, err
	}
	defer rootfs.Close()

	slot, err := p.acquire()
	if err != nil {
		return res, err
	}
	// Released after cleanup (defers run last-in, first-out), and never
	// if the VM could not be stopped: a live VMM must not share its
	// identity with the next one.
	stuck := false
	defer func() {
		if !stuck {
			p.release(slot)
		}
	}()

	if err := os.MkdirAll(p.cfg.StagingBase, 0o700); err != nil {
		return res, fmt.Errorf("firecracker: %w", err)
	}
	staging := filepath.Join(p.cfg.StagingBase, spec.ID)
	// Mkdir (not MkdirAll) fails if it exists, so two runs can never
	// share one, and we never clean up a run that is not ours.
	if err := os.Mkdir(staging, 0o700); err != nil {
		return res, fmt.Errorf("firecracker: create staging: %w", err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if cerr := p.cfg.Host.RemoveJail(cctx, spec.ID); cerr != nil {
			err = errors.Join(err, fmt.Errorf("firecracker: cleanup: %w", cerr))
		}
		if cerr := os.RemoveAll(staging); cerr != nil {
			err = errors.Join(err, fmt.Errorf("firecracker: cleanup: %w", cerr))
		}
		res.Duration = time.Since(start)
	}()

	if err := p.stage(staging, &spec, kernel, rootfs); err != nil {
		return res, err
	}

	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	proc, err := p.cfg.Host.StartVM(StartRequest{ID: spec.ID, Slot: slot, CPUs: spec.CPUs, MemoryMiB: spec.MemoryMiB}, console)
	if err != nil {
		return res, err
	}
	done := make(chan error, 1)
	go func() { done <- proc.Wait() }()

	select {
	case werr := <-done:
		if werr != nil {
			return res, fmt.Errorf("firecracker: vmm exited: %w", werr)
		}
		res.Outcome = sandbox.OutcomeExited
		return res, nil
	case <-runCtx.Done():
	}

	outcome := sandbox.OutcomeKilled
	if ctx.Err() == nil { // our own deadline, not the caller's
		outcome = sandbox.OutcomeTimedOut
	}
	if err := p.stop(spec.ID, proc, done); err != nil {
		stuck = true
		return res, err
	}
	res.Outcome = outcome
	return res, nil
}

// stop ends a running VM: SIGTERM to the jailer chain first, then
// cgroup.kill if it is still running after StopGrace.
func (p *Provider) stop(id string, proc Process, done <-chan error) error {
	_ = proc.Signal(syscall.SIGTERM) // the process may already be gone
	select {
	case werr := <-done:
		_ = werr // a deliberately stopped VM always exits with a signal status
		return nil
	case <-time.After(p.cfg.StopGrace):
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.StopGrace)
	defer cancel()
	kerr := p.cfg.Host.KillJail(ctx, id)
	select {
	case werr := <-done:
		_ = werr
		return nil
	case <-time.After(p.cfg.StopGrace):
		return errors.Join(fmt.Errorf("firecracker: sandbox %s did not stop", id), kerr)
	}
}

// stage writes the VM's files into the staging directory. They are
// copies, so the VM can never affect the originals; the helper copies
// them once more into the root-owned jail.
func (p *Provider) stage(dir string, spec *sandbox.Spec, kernel, rootfs *os.File) error {
	if err := writeStaged(kernel, "vmlinux", filepath.Join(dir, "vmlinux")); err != nil {
		return err
	}
	if err := writeStaged(rootfs, "rootfs.ext4", filepath.Join(dir, "rootfs.ext4")); err != nil {
		return err
	}
	cfg, err := json.MarshalIndent(vmConfig(spec), "", "  ")
	if err != nil {
		return fmt.Errorf("firecracker: %w", err)
	}
	return writeStaged(bytes.NewReader(cfg), "config.json", filepath.Join(dir, "config.json"))
}

// vmConfig is Firecracker's configuration file. Built from structs, never
// from string templates, so no field can inject JSON.
func vmConfig(spec *sandbox.Spec) map[string]any {
	args := append(append([]string{}, baseBootArgs...), spec.BootArgs...)
	return map[string]any{
		"boot-source": map[string]any{
			"kernel_image_path": "vmlinux",
			"boot_args":         strings.Join(args, " "),
		},
		"drives": []map[string]any{{
			"drive_id":       "rootfs",
			"path_on_host":   "rootfs.ext4",
			"is_root_device": true,
			"is_read_only":   true,
		}},
		"machine-config": map[string]any{
			"vcpu_count":   spec.CPUs,
			"mem_size_mib": spec.MemoryMiB,
		},
	}
}

// openImage resolves path, checks that it lies inside ImageRoot, opens it
// and verifies the opened file (not the path) is a regular file. Copying
// from this handle means a symlink swapped in after the check cannot
// redirect what ends up in the guest.
func (p *Provider) openImage(path string) (*os.File, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("firecracker: image %s: %w", path, err)
	}
	root, err := filepath.EvalSymlinks(p.cfg.ImageRoot)
	if err != nil {
		return nil, fmt.Errorf("firecracker: image root: %w", err)
	}
	if rel, err := filepath.Rel(root, resolved); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("firecracker: image %s is outside the image root %s", path, p.cfg.ImageRoot)
	}
	f, err := os.Open(resolved) // confined to ImageRoot above
	if err != nil {
		return nil, fmt.Errorf("firecracker: %w", err)
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("firecracker: image %s is not a regular file", path)
	}
	return f, nil
}

// writeStaged creates dst exclusively (owner-only) and fills it from r.
func writeStaged(r io.Reader, name, dst string) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // dst is inside our own staging directory
	if err != nil {
		return fmt.Errorf("firecracker: %w", err)
	}
	if _, err := io.Copy(out, r); err != nil {
		_ = out.Close() // the copy error is the one worth reporting
		return fmt.Errorf("firecracker: write %s: %w", name, err)
	}
	return out.Close()
}
