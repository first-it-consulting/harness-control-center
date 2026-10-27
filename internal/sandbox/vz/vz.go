// Package vz runs sandboxes as Linux VMs with Apple's Virtualization
// framework on macOS (ADR-0016). It needs no privileges: the VMs belong to
// the runner process.
//
// The guest sees what it sees under Firecracker: the image's root file
// system read-only on vda, then the task, the tool catalog, the plugin
// bundle (read-only) and the writable results disk, settings on the
// kernel command line, its console on hvc0. It has no network device:
// with network access, the job's egress proxy is served on one vsock
// port, which the guest's init forwards to a loopback address
// (hcc-guest vsock-proxy).
//
// Per run, the provider stages the task, bundle and disk files in
// <StagingBase>/<id> (its own directory), starts the VM, stops it on
// timeout or cancellation, and always removes the staging directory.
// This file holds everything but the Virtualization framework calls,
// which sit behind Machine.
package vz

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
)

// Defaults.
const (
	DefaultStopGrace = 10 * time.Second
	// DefaultProxyPort is the loopback port the guest's tools are given as
	// their proxy.
	DefaultProxyPort = 3128
	// VsockPort is where the proxy listens on the VM's vsock device.
	VsockPort = 1024
	// MaxSlots bounds Config.Slots.
	MaxSlots = 64
	// CollectTimeout bounds reading a sandbox's disk after it stopped.
	CollectTimeout = 10 * time.Minute
)

// Fixed kernel parameters. Callers cannot override these (see
// sandbox.Spec.BootArgs). The Virtualization framework restarts a guest
// that reboots, so the guest powers off instead (hcc.shutdown), and a
// kernel panic halts rather than reboots: a restarted guest would run its
// agent again. A halted one waits for the run's timeout.
var baseBootArgs = []string{"console=hvc0", "quiet", "loglevel=1", "panic=0", "root=/dev/vda", "rootfstype=ext4", "ro", "init=/init", "hcc.shutdown=poweroff"}

// Image is one bootable image's files.
type Image struct {
	Kernel string
	RootFS string
	// Tools is the image's tool catalog drive (ADR-0010), if it has one.
	Tools string
}

// Config configures the provider.
type Config struct {
	// StagingBase is the runner's own directory for each run's files.
	StagingBase string
	// Images are the bootable images, by name.
	Images map[string]Image
	// Slots is how many VMs may run at once (default 1).
	Slots int
	// ProxyPort is the guest's loopback proxy port (default 3128).
	ProxyPort int
	// StopGrace is how long to wait for a VM to stop after Stop.
	StopGrace time.Duration
	// NewMachine creates a VM (default: the Virtualization framework);
	// replaceable for tests.
	NewMachine func(MachineConfig) (Machine, error)
}

// Disk is one of a VM's drives.
type Disk struct {
	Path     string
	ReadOnly bool
}

// MachineConfig describes one VM.
type MachineConfig struct {
	Kernel    string
	CmdLine   string
	CPUs      int
	MemoryMiB int
	// Disks in the order the guest sees them (vda, vdb, ...).
	Disks []Disk
	// Console gets the guest's console output.
	Console io.Writer
	// Vsock gives the VM a vsock device (for Listen).
	Vsock bool
}

// Machine is one VM.
type Machine interface {
	// Start boots the VM.
	Start() error
	// Done is closed when the VM has stopped; Err then says whether it
	// stopped because of an error.
	Done() <-chan struct{}
	Err() error
	// Stop ends the VM at once.
	Stop() error
	// Listen accepts the guest's connections to a vsock port of the host.
	Listen(port uint32) (net.Listener, error)
}

// Provider implements sandbox.Provider with Apple's Virtualization
// framework.
type Provider struct {
	cfg Config

	mu    sync.Mutex
	inUse int
}

var _ sandbox.Provider = (*Provider)(nil)

// New validates cfg and returns a provider.
func New(cfg Config) (*Provider, error) {
	var errs []error
	if !cleanAbs(cfg.StagingBase) {
		errs = append(errs, fmt.Errorf("StagingBase must be a clean absolute path, got %q", cfg.StagingBase))
	}
	if cfg.Slots == 0 {
		cfg.Slots = 1
	}
	if cfg.Slots < 0 || cfg.Slots > MaxSlots {
		errs = append(errs, fmt.Errorf("slots must be between 1 and %d", MaxSlots))
	}
	if cfg.ProxyPort == 0 {
		cfg.ProxyPort = DefaultProxyPort
	}
	if cfg.ProxyPort < 1 || cfg.ProxyPort > 65535 {
		errs = append(errs, errors.New("proxy port must be between 1 and 65535"))
	}
	if len(cfg.Images) == 0 {
		errs = append(errs, errors.New("at least one image is required"))
	}
	for name, img := range cfg.Images {
		if err := sandbox.ValidateImage(name); err != nil {
			errs = append(errs, err)
		}
		for what, p := range map[string]string{"kernel": img.Kernel, "rootfs": img.RootFS, "tools": img.Tools} {
			if p == "" && what == "tools" {
				continue
			}
			if !cleanAbs(p) {
				errs = append(errs, fmt.Errorf("image %s: %s must be a clean absolute path, got %q", name, what, p))
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("vz: invalid config: %w", err)
	}
	if cfg.StopGrace <= 0 {
		cfg.StopGrace = DefaultStopGrace
	}
	if cfg.NewMachine == nil {
		cfg.NewMachine = newMachine
	}
	return &Provider{cfg: cfg}, nil
}

// Name implements sandbox.Provider.
func (p *Provider) Name() string { return "vz" }

func (p *Provider) acquire() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inUse >= p.cfg.Slots {
		return fmt.Errorf("vz: all %d sandbox slots are in use", p.cfg.Slots)
	}
	p.inUse++
	return nil
}

func (p *Provider) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inUse--
}

// Run implements sandbox.Provider.
func (p *Provider) Run(ctx context.Context, spec sandbox.Spec, console io.Writer) (res sandbox.Result, err error) {
	start := time.Now()
	res.Outcome = sandbox.OutcomeFailed
	if err := spec.Validate(); err != nil {
		return res, err
	}
	img, ok := p.cfg.Images[spec.Image]
	if !ok {
		return res, fmt.Errorf("vz: unknown image %q", spec.Image)
	}
	if spec.Tools && img.Tools == "" {
		return res, fmt.Errorf("vz: image %q has no tool catalog", spec.Image)
	}
	if err := p.acquire(); err != nil {
		return res, err
	}
	// A VM that could not be stopped keeps its slot.
	stuck := false
	defer func() {
		if !stuck {
			p.release()
		}
	}()

	if err := os.MkdirAll(p.cfg.StagingBase, 0o700); err != nil {
		return res, fmt.Errorf("vz: %w", err)
	}
	staging := filepath.Join(p.cfg.StagingBase, spec.ID)
	// Mkdir (not MkdirAll) fails if it exists, so two runs can never
	// share one, and we never clean up a run that is not ours.
	if err := os.Mkdir(staging, 0o700); err != nil {
		return res, fmt.Errorf("vz: create staging: %w", err)
	}
	defer func() {
		if stuck {
			return // its VM may still use the files
		}
		if cerr := os.RemoveAll(staging); cerr != nil {
			err = errors.Join(err, fmt.Errorf("vz: cleanup: %w", cerr))
		}
		res.Duration = time.Since(start)
	}()

	mc, err := p.stage(staging, &spec, img)
	if err != nil {
		return res, err
	}
	mc.Console = console
	m, err := p.cfg.NewMachine(mc)
	if err != nil {
		return res, fmt.Errorf("vz: %w", err)
	}
	if err := m.Start(); err != nil {
		return res, fmt.Errorf("vz: start: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	if spec.Network != nil {
		stopProxy, err := p.serveProxy(m, spec.Network.Proxy)
		if err != nil {
			if serr := p.stop(m); serr != nil {
				stuck = true
				err = errors.Join(err, serr)
			}
			return res, err
		}
		defer func() {
			if perr := stopProxy(); perr != nil {
				err = errors.Join(err, perr)
			}
		}()
	}

	select {
	case <-m.Done():
		if merr := m.Err(); merr != nil {
			return res, fmt.Errorf("vz: vm: %w", merr)
		}
		if spec.Collect != nil {
			if err := collect(filepath.Join(staging, "disk.img"), spec.Collect); err != nil {
				return res, fmt.Errorf("vz: results: %w", err)
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
	if err := p.stop(m); err != nil {
		stuck = true
		return res, err
	}
	res.Outcome = outcome
	return res, nil
}

// stop ends a VM and waits until it is gone.
func (p *Provider) stop(m Machine) error {
	serr := m.Stop()
	select {
	case <-m.Done():
		return nil
	case <-time.After(p.cfg.StopGrace):
		return errors.Join(errors.New("vz: the VM did not stop"), serr)
	}
}

// serveProxy serves the egress proxy on the VM's vsock port. Every
// connection there comes from this VM's guest, so no peer check is
// needed; connections report the guest's proxy address as their local
// address, as on a network link.
func (p *Provider) serveProxy(m Machine, proxy sandbox.Proxy) (func() error, error) {
	l, err := m.Listen(VsockPort)
	if err != nil {
		return nil, fmt.Errorf("vz: proxy listener: %w", err)
	}
	l = &guestAddrListener{Listener: l, addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: p.cfg.ProxyPort}}
	pctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- proxy.Serve(pctx, l) }()
	return func() error {
		cancel()
		select {
		case err := <-served:
			if err != nil {
				return fmt.Errorf("vz: proxy: %w", err)
			}
			return nil
		case <-time.After(10 * time.Second):
			return errors.New("vz: proxy did not stop")
		}
	}, nil
}

// stage writes the run's files and returns the VM's configuration.
func (p *Provider) stage(dir string, spec *sandbox.Spec, img Image) (MachineConfig, error) {
	mc := MachineConfig{Kernel: img.Kernel, CPUs: spec.CPUs, MemoryMiB: spec.MemoryMiB, Vsock: spec.Network != nil}
	args := append(append([]string{}, baseBootArgs...), spec.BootArgs...)
	if spec.Network != nil {
		args = append(args, "hcc.vsock="+strconv.Itoa(VsockPort), "hcc.proxy=127.0.0.1:"+strconv.Itoa(p.cfg.ProxyPort))
	}
	add := func(path string, ro bool) string {
		mc.Disks = append(mc.Disks, Disk{Path: path, ReadOnly: ro})
		i := len(mc.Disks) - 1 // at most five drives
		return "vd" + "abcde"[i:i+1]
	}
	add(img.RootFS, true)
	if len(spec.Task) > 0 {
		// The guest reads its task from /dev/vdb.
		path := filepath.Join(dir, "task.img")
		if err := writeFile(path, sectors(spec.Task)); err != nil {
			return mc, err
		}
		add(path, true)
	}
	if spec.Tools {
		args = append(args, "hcc.tools="+add(img.Tools, true))
	}
	if len(spec.Bundle) > 0 {
		path := filepath.Join(dir, "bundle.img")
		if err := writeFile(path, sectors(spec.Bundle)); err != nil {
			return mc, err
		}
		args = append(args, "hcc.bundle="+add(path, true))
	}
	if spec.DiskMiB > 0 {
		// The guest's only writable drive: a sparse file of the asked size.
		path := filepath.Join(dir, "disk.img")
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // inside our own staging directory
		if err != nil {
			return mc, fmt.Errorf("vz: %w", err)
		}
		terr := f.Truncate(int64(spec.DiskMiB) << 20)
		if err := errors.Join(terr, f.Close()); err != nil {
			return mc, fmt.Errorf("vz: disk: %w", err)
		}
		add(path, false)
	}
	mc.CmdLine = strings.Join(args, " ")
	return mc, nil
}

// Utility is a VM outside of any sandbox run: a setup task of the local
// runner (internal/local), booted from one of our own images with drives
// the caller prepared, no network and no vsock.
type Utility struct {
	Kernel string
	// Disks in the order the guest sees them; the first is the root file
	// system (read-only).
	Disks     []Disk
	BootArgs  []string
	CPUs      int
	MemoryMiB int
	Timeout   time.Duration
	Console   io.Writer
	// NewMachine defaults to the Virtualization framework.
	NewMachine func(MachineConfig) (Machine, error)
}

// RunUtility boots u and waits until the guest shuts itself down; after
// u.Timeout or when ctx ends, it stops the VM and fails.
func RunUtility(ctx context.Context, u Utility) error {
	if len(u.Disks) == 0 || !u.Disks[0].ReadOnly {
		return errors.New("vz: a utility VM needs a read-only root drive")
	}
	if u.Timeout <= 0 {
		return errors.New("vz: a utility VM needs a timeout")
	}
	newM := u.NewMachine
	if newM == nil {
		newM = newMachine
	}
	m, err := newM(MachineConfig{
		Kernel: u.Kernel, CmdLine: strings.Join(append(append([]string{}, baseBootArgs...), u.BootArgs...), " "),
		CPUs: u.CPUs, MemoryMiB: u.MemoryMiB, Disks: u.Disks, Console: u.Console,
	})
	if err != nil {
		return fmt.Errorf("vz: %w", err)
	}
	if err := m.Start(); err != nil {
		return fmt.Errorf("vz: start: %w", err)
	}
	runCtx, cancel := context.WithTimeout(ctx, u.Timeout)
	defer cancel()
	select {
	case <-m.Done():
		if err := m.Err(); err != nil {
			return fmt.Errorf("vz: vm: %w", err)
		}
		return nil
	case <-runCtx.Done():
	}
	serr := m.Stop()
	select {
	case <-m.Done():
	case <-time.After(DefaultStopGrace):
		return errors.Join(errors.New("vz: the VM did not stop"), serr)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("vz: the VM did not finish in time")
}

// Recover removes what runs of a previous runner process left behind.
// Their VMs ended with that process (they belong to it), so only the
// staging directories remain.
func (p *Provider) Recover(_ context.Context, ids []string) error {
	var errs []error
	for _, id := range ids {
		if err := sandbox.ValidateID(id); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.RemoveAll(filepath.Join(p.cfg.StagingBase, id)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func collect(path string, fn func(io.Reader) error) error {
	f, err := os.Open(path) //nolint:gosec // inside our own staging directory
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	done := make(chan error, 1)
	go func() { done <- fn(f) }()
	select {
	case err := <-done:
		return err
	case <-time.After(CollectTimeout):
		return errors.New("reading the disk took too long")
	}
}

// writeFile creates path exclusively (owner-only) with data.
func writeFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // inside our own staging directory
	if err != nil {
		return fmt.Errorf("vz: %w", err)
	}
	_, werr := f.Write(data)
	if err := errors.Join(werr, f.Close()); err != nil {
		return fmt.Errorf("vz: write %s: %w", filepath.Base(path), err)
	}
	return nil
}

// sectors pads data to whole 512-byte sectors with zero bytes; the guest
// strips them.
func sectors(data []byte) []byte {
	out := make([]byte, (len(data)+511)/512*512)
	copy(out, data)
	return out
}

func cleanAbs(p string) bool { return filepath.IsAbs(p) && filepath.Clean(p) == p }

type guestAddrListener struct {
	net.Listener
	addr net.Addr
}

func (l *guestAddrListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &guestAddrConn{Conn: c, addr: l.addr}, nil
}

func (l *guestAddrListener) Addr() net.Addr { return l.addr }

type guestAddrConn struct {
	net.Conn
	addr net.Addr
}

func (c *guestAddrConn) LocalAddr() net.Addr { return c.addr }
