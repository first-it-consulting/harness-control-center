// Package firecracker runs sandboxes as Firecracker microVMs under the
// jailer (ADR-0001). The provider itself is unprivileged; everything that
// needs root goes through Host.
//
// Per run, the provider:
//  1. stages the VM configuration (and the task, if any) in
//     <StagingBase>/<id> (its own directory),
//  2. asks its Host to start the VM by image name: root-side, hcc-helper
//     builds a root-owned jail from the image (from its own configuration)
//     and the staged files, then the jailer applies
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
	"net"
	"net/netip"
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
	// CollectTimeout bounds reading a sandbox's disk after it stopped.
	CollectTimeout = 10 * time.Minute
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
	// Identities is how many VMs can run at once (default 1); it must not
	// exceed the Host's.
	Identities int
	// StopGrace is how long to wait after SIGTERM, and again after
	// cgroup.kill, before giving up.
	StopGrace time.Duration
	Host      Host
	// Listen binds the egress proxy's listener (default net.Listen);
	// replaceable for tests.
	Listen func(network, address string) (net.Listener, error)
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
	if !filepath.IsAbs(cfg.StagingBase) || filepath.Clean(cfg.StagingBase) != cfg.StagingBase {
		errs = append(errs, fmt.Errorf("StagingBase must be a clean absolute path, got %q", cfg.StagingBase))
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
	if cfg.Listen == nil {
		cfg.Listen = net.Listen
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

	var link *NetInfo
	if spec.Network != nil {
		info, stopNet, err := p.startNetwork(ctx, slot, spec.Network.Proxy)
		if err != nil {
			return res, err
		}
		// After the jail is gone (defers run last-in, first-out); kept if
		// the VM could not be stopped, like its identity.
		defer func() {
			if nerr := stopNet(!stuck); nerr != nil {
				err = errors.Join(err, nerr)
			}
		}()
		link = &info
	}

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

	if err := p.stage(staging, &spec, link); err != nil {
		return res, err
	}

	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	proc, err := p.cfg.Host.StartVM(StartRequest{
		ID: spec.ID, Image: spec.Image, Slot: slot, CPUs: spec.CPUs, MemoryMiB: spec.MemoryMiB, DiskMiB: spec.DiskMiB,
	}, console)
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
		if spec.Collect != nil {
			// The VMM has exited: nothing writes the disk any more.
			cctx, cancel := context.WithTimeout(context.Background(), CollectTimeout)
			defer cancel()
			if err := p.cfg.Host.CollectDisk(cctx, spec.ID, spec.Collect); err != nil {
				return res, fmt.Errorf("firecracker: results: %w", err)
			}
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

// Recover cleans up after sandboxes of a previous runner process that
// did not finish (it crashed or was killed): their VMs may still run,
// since the jailer moves them out of the runner's cgroup. For each ID it
// kills the jail's cgroup, removes the jail and the staging directory;
// with network, it deletes every slot's link. Call it before any Run.
func (p *Provider) Recover(ctx context.Context, ids []string, network bool) error {
	var errs []error
	for _, id := range ids {
		if err := sandbox.ValidateID(id); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := p.cfg.Host.KillJail(ctx, id); err != nil {
			errs = append(errs, err)
		}
		// cgroup.kill is asynchronous: retry the removal while the
		// processes die.
		var rerr error
		for range 10 {
			if rerr = p.cfg.Host.RemoveJail(ctx, id); rerr == nil {
				break
			}
			select {
			case <-ctx.Done():
				return errors.Join(append(errs, rerr, ctx.Err())...)
			case <-time.After(p.cfg.StopGrace / 5):
			}
		}
		if rerr != nil {
			errs = append(errs, rerr)
		}
		if err := os.RemoveAll(filepath.Join(p.cfg.StagingBase, id)); err != nil {
			errs = append(errs, err)
		}
	}
	if network {
		for slot := range p.cfg.Identities {
			if err := p.cfg.Host.TeardownNetwork(ctx, slot); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
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

// stage writes the VM configuration and the task into the staging
// directory; the helper copies them into the root-owned jail.
func (p *Provider) stage(dir string, spec *sandbox.Spec, link *NetInfo) error {
	if len(spec.Task) > 0 {
		if err := writeStaged(bytes.NewReader(taskImage(spec.Task)), "task.img", filepath.Join(dir, "task.img")); err != nil {
			return err
		}
	}
	cfg, err := json.MarshalIndent(vmConfig(spec, link), "", "  ")
	if err != nil {
		return fmt.Errorf("firecracker: %w", err)
	}
	return writeStaged(bytes.NewReader(cfg), "config.json", filepath.Join(dir, "config.json"))
}

// vmConfig is Firecracker's configuration file. Built from structs, never
// from string templates, so no field can inject JSON.
func vmConfig(spec *sandbox.Spec, link *NetInfo) map[string]any {
	args := append(append([]string{}, baseBootArgs...), spec.BootArgs...)
	if link != nil {
		// The guest's init configures eth0 and points HTTPS_PROXY at the
		// proxy. There is no gateway: the proxy is on-link, nothing else
		// is reachable.
		args = append(args, "hcc.ip="+link.Guest.String(),
			"hcc.proxy="+netip.AddrPortFrom(link.Host, uint16(link.ProxyPort)).String()) //nolint:gosec // port validated by NetInfo.Valid
	}
	cfg := map[string]any{
		"boot-source": map[string]any{
			"kernel_image_path": "vmlinux",
			"boot_args":         strings.Join(args, " "),
		},
		"drives": drives(spec),
		"machine-config": map[string]any{
			"vcpu_count":   spec.CPUs,
			"mem_size_mib": spec.MemoryMiB,
		},
	}
	if link != nil {
		g := link.Guest.Addr().As4()
		cfg["network-interfaces"] = []map[string]any{{
			"iface_id":      "eth0",
			"host_dev_name": link.Tap,
			"guest_mac":     fmt.Sprintf("06:00:%02x:%02x:%02x:%02x", g[0], g[1], g[2], g[3]),
		}}
	}
	return cfg
}

// startNetwork creates slot's link and serves the egress proxy on its host
// address. The returned stop ends the proxy and, if teardown, deletes the
// link.
func (p *Provider) startNetwork(ctx context.Context, slot int, proxy sandbox.Proxy) (NetInfo, func(teardown bool) error, error) {
	info, err := p.cfg.Host.SetupNetwork(ctx, slot)
	if err != nil {
		return info, nil, err
	}
	down := func() error {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := p.cfg.Host.TeardownNetwork(cctx, slot); err != nil {
			return fmt.Errorf("firecracker: network cleanup: %w", err)
		}
		return nil
	}
	l, err := p.cfg.Listen("tcp", netip.AddrPortFrom(info.Host, uint16(info.ProxyPort)).String()) //nolint:gosec // port validated by NetInfo.Valid
	if err != nil {
		return info, nil, errors.Join(fmt.Errorf("firecracker: proxy listener: %w", err), down())
	}
	// Only this link's guest may use its proxy (the firewall also keeps
	// other machines out).
	l = &peerListener{Listener: l, peer: info.Guest.Addr()}
	pctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- proxy.Serve(pctx, l) }()
	return info, func(teardown bool) error {
		cancel()
		var err error
		select {
		case serr := <-served:
			if serr != nil {
				err = fmt.Errorf("firecracker: proxy: %w", serr)
			}
		case <-time.After(10 * time.Second):
			err = errors.New("firecracker: proxy did not stop")
		}
		if teardown {
			err = errors.Join(err, down())
		}
		return err
	}, nil
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

// peerListener accepts connections only from one address; others are
// closed immediately.
type peerListener struct {
	net.Listener
	peer netip.Addr
}

func (l *peerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if ap, err := netip.ParseAddrPort(c.RemoteAddr().String()); err == nil && ap.Addr().Unmap() == l.peer {
			return c, nil
		}
		_ = c.Close()
	}
}

func drives(spec *sandbox.Spec) []map[string]any {
	d := []map[string]any{{
		"drive_id":       "rootfs",
		"path_on_host":   "rootfs.ext4",
		"is_root_device": true,
		"is_read_only":   true,
	}}
	if len(spec.Task) > 0 {
		// The guest reads its task from /dev/vdb.
		d = append(d, map[string]any{
			"drive_id":       "task",
			"path_on_host":   "task.img",
			"is_root_device": false,
			"is_read_only":   true,
		})
	}
	if spec.DiskMiB > 0 {
		// The guest's only writable drive (vdb or vdc); the helper creates
		// disk.img in the jail.
		d = append(d, map[string]any{
			"drive_id":       "disk",
			"path_on_host":   "disk.img",
			"is_root_device": false,
			"is_read_only":   false,
		})
	}
	return d
}

// taskImage pads the task to whole sectors with zero bytes; the guest
// strips them.
func taskImage(task []byte) []byte {
	n := (len(task) + 511) / 512 * 512
	out := make([]byte, n)
	copy(out, task)
	return out
}
