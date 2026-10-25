// Package sandbox defines how agents are isolated, independent of the
// technology that isolates them. Providers (Firecracker, Proxmox, ...)
// implement Provider; the runner only ever talks to this interface.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	// Kernel and RootFS are absolute paths to the guest kernel and the
	// root filesystem image. The root filesystem is attached read-only.
	Kernel string
	RootFS string
	// CPUs and MemoryMiB size the guest.
	CPUs      int
	MemoryMiB int
	// Timeout bounds the run; it is mandatory.
	Timeout time.Duration
	// BootArgs are extra kernel parameters ("key" or "key=value"). Keys
	// the provider controls (init, console, root ...) are rejected.
	BootArgs []string
}

// Limits for Spec fields.
const (
	MaxCPUs      = 32
	MinMemoryMiB = 128
	MaxMemoryMiB = 64 * 1024
	MaxTimeout   = 24 * time.Hour
)

var (
	idRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	bootArgRe = regexp.MustCompile(`^[a-z0-9_.]+(=[A-Za-z0-9_.,:/-]+)?$`)
	// Kernel parameters that would let a caller change how the guest boots
	// or what it runs.
	reservedBootArgs = map[string]bool{
		"init": true, "rdinit": true, "console": true, "root": true, "rootfstype": true,
		"rw": true, "ro": true, "reboot": true, "panic": true, "pci": true,
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
	for name, path := range map[string]string{"kernel": s.Kernel, "rootfs": s.RootFS} {
		if err := checkFile(path); err != nil {
			errs = append(errs, fmt.Errorf("sandbox: %s: %w", name, err))
		}
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

func checkFile(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("path %q must be absolute", path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%q is not a regular file", path)
	}
	return nil
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
