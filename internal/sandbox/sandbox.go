// Package sandbox defines how agents are isolated, independent of the
// technology that isolates them. Providers (Firecracker, Proxmox, ...)
// implement Provider; the runner only ever talks to this interface.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"time"
)

// Provider runs sandboxes.
type Provider interface {
	// Name identifies the provider, e.g. "firecracker".
	Name() string
	// Run boots a sandbox, streams its console to console and blocks
	// until it exits, times out (Spec.Timeout) or ctx is cancelled. On
	// return the sandbox is gone and its resources are released; if
	// that cannot be guaranteed, Run returns an error.
	Run(ctx context.Context, spec Spec, console io.Writer) (Result, error)
}

// Spec describes one sandbox run.
type Spec struct {
	// ID is unique per run. It ends up in host paths, so it is restricted
	// to lowercase letters, digits and '-'.
	ID string
	// Image names a bootable image the provider knows (it resolves the
	// name itself; callers never pass paths). Its root filesystem is
	// attached read-only.
	Image string
	// Task, if set, is given to the guest read-only (drive vdb), e.g. the
	// agent's prompt and settings. At most MaxTaskBytes.
	Task []byte
	// CPUs and MemoryMiB size the guest.
	CPUs      int
	MemoryMiB int
	// Timeout bounds the run; it is mandatory.
	Timeout time.Duration
	// BootArgs are extra kernel parameters ("key" or "key=value"). Keys
	// the provider controls (init, console, root, the network ...) are
	// rejected.
	BootArgs []string
	// Network, if set, gives the sandbox one network interface whose only
	// reachable peer is an egress proxy served by Network.Proxy. Without
	// it the sandbox has no network at all.
	Network *Network
	// DiskMiB, if set, gives the guest a blank writable drive of this
	// size, its only writable drive. This is how results get out.
	DiskMiB int
	// Tools, if set, attaches the image's tool catalog read-only, for the
	// guest's init to install from before the agent starts (ADR-0010).
	// The provider fails the run if the image has none.
	Tools bool
	// Collect, if set, gets the disk's raw contents after the guest shut
	// itself down (not after a timeout or kill: a half-written disk is
	// not worth reading). The guest wrote them, so they are hostile input.
	Collect func(disk io.Reader) error
}

// Network is a sandbox's network access.
type Network struct {
	Proxy Proxy
}

// Proxy serves a sandbox's egress proxy on a listener the provider binds
// to the host side of the sandbox's link, until ctx ends.
type Proxy interface {
	Serve(ctx context.Context, l net.Listener) error
}

// Limits for Spec fields.
const (
	MaxCPUs      = 32
	MinMemoryMiB = 128
	MaxMemoryMiB = 64 * 1024
	MaxTimeout   = 24 * time.Hour
	MaxTaskBytes = 1 << 20
	MaxDiskMiB   = 256 * 1024
)

var (
	idRe        = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	imageNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	bootArgRe   = regexp.MustCompile(`^[a-z0-9_.]+(=[A-Za-z0-9_.,:/-]+)?$`)
	// Kernel parameters that would let a caller change how the guest boots
	// or what it runs.
	reservedBootArgs = map[string]bool{
		"init": true, "rdinit": true, "console": true, "root": true, "rootfstype": true,
		"rw": true, "ro": true, "reboot": true, "panic": true, "pci": true,
		"ip": true, "hcc.ip": true, "hcc.proxy": true, "hcc.tools": true,
	}
)

// ValidateID checks that id is safe to use as a single path component.
func ValidateID(id string) error {
	if !idRe.MatchString(id) {
		return fmt.Errorf("sandbox: invalid id %q (1-63 of a-z, 0-9, '-')", id)
	}
	return nil
}

// Validate checks every field and returns all problems at once.
func (s *Spec) Validate() error {
	var errs []error
	if err := ValidateID(s.ID); err != nil {
		errs = append(errs, err)
	}
	if !imageNameRe.MatchString(s.Image) {
		errs = append(errs, fmt.Errorf("sandbox: invalid image name %q", s.Image))
	}
	if len(s.Task) > MaxTaskBytes {
		errs = append(errs, fmt.Errorf("sandbox: task is %d bytes; the limit is %d", len(s.Task), MaxTaskBytes))
	}
	if s.CPUs < 1 || s.CPUs > MaxCPUs {
		errs = append(errs, fmt.Errorf("sandbox: cpus must be between 1 and %d", MaxCPUs))
	}
	if s.MemoryMiB < MinMemoryMiB || s.MemoryMiB > MaxMemoryMiB {
		errs = append(errs, fmt.Errorf("sandbox: memory must be between %d and %d MiB", MinMemoryMiB, MaxMemoryMiB))
	}
	if s.Timeout <= 0 || s.Timeout > MaxTimeout {
		errs = append(errs, fmt.Errorf("sandbox: timeout must be greater than 0 and at most %s", MaxTimeout))
	}
	if s.DiskMiB < 0 || s.DiskMiB > MaxDiskMiB {
		errs = append(errs, fmt.Errorf("sandbox: disk must be between 0 and %d MiB", MaxDiskMiB))
	}
	if s.Collect != nil && s.DiskMiB == 0 {
		errs = append(errs, errors.New("sandbox: collect without a disk"))
	}
	if s.Network != nil && s.Network.Proxy == nil {
		errs = append(errs, errors.New("sandbox: network without a proxy"))
	}
	for _, a := range s.BootArgs {
		key, _, _ := strings.Cut(a, "=")
		switch {
		case !bootArgRe.MatchString(a):
			errs = append(errs, fmt.Errorf("sandbox: invalid boot argument %q", a))
		case reservedBootArgs[key]:
			errs = append(errs, fmt.Errorf("sandbox: boot argument %q is controlled by the provider", key))
		}
	}
	return errors.Join(errs...)
}

// Outcome says how a run ended.
type Outcome int

// Outcomes.
const (
	// OutcomeExited: the guest shut itself down.
	OutcomeExited Outcome = iota
	// OutcomeTimedOut: Spec.Timeout elapsed and the sandbox was stopped.
	OutcomeTimedOut
	// OutcomeKilled: the caller cancelled the run (kill switch).
	OutcomeKilled
	// OutcomeFailed: the sandbox could not be started or crashed.
	OutcomeFailed
)

func (o Outcome) String() string {
	switch o {
	case OutcomeExited:
		return "exited"
	case OutcomeTimedOut:
		return "timed_out"
	case OutcomeKilled:
		return "killed"
	default:
		return "failed"
	}
}

// Result describes a finished run.
type Result struct {
	Outcome  Outcome
	Duration time.Duration
}
