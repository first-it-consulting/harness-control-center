package firecracker

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"

	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
)

// StartRequest is what the unprivileged provider may ask for when starting
// a VM. Everything else on the jailer command line comes from
// JailerConfig, which is owned by whoever runs as root.
type StartRequest struct {
	ID string
	// Image names an image from the root-side configuration.
	Image string
	// Slot selects the VM's identity: UID+Slot and GID+Slot.
	Slot      int
	CPUs      int
	MemoryMiB int
	// DiskMiB sizes the guest's writable disk (0: none). The helper
	// creates it in the jail, owned by the slot's identity.
	DiskMiB int
}

// JailerConfig holds the root-side settings for starting VMs.
type JailerConfig struct {
	JailerPath      string
	FirecrackerPath string
	ChrootBase      string
	// UID and GID are the first of Identities consecutive IDs; each
	// running VM gets its own pair, so one VMM can never signal, trace or
	// read another (Firecracker's recommendation).
	UID, GID   int
	Identities int
	// ForbiddenIDs are user/group IDs the range must not include, e.g.
	// the runner's own.
	ForbiddenIDs []int
}

// Validate checks c.
func (c *JailerConfig) Validate() error {
	var errs []error
	for name, p := range map[string]string{"jailer": c.JailerPath, "firecracker": c.FirecrackerPath, "chroot base": c.ChrootBase} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			errs = append(errs, fmt.Errorf("%s path must be clean and absolute, got %q", name, p))
		}
	}
	if c.Identities < 1 || c.Identities > MaxIdentities {
		errs = append(errs, fmt.Errorf("identities must be between 1 and %d", MaxIdentities))
	}
	if c.UID <= 0 || c.GID <= 0 {
		errs = append(errs, errors.New("UID and GID must be non-root"))
	}
	if c.UID > math.MaxInt32-c.Identities || c.GID > math.MaxInt32-c.Identities {
		errs = append(errs, errors.New("UID/GID range overflows"))
	}
	for _, id := range c.ForbiddenIDs {
		if inRange(id, c.UID, c.Identities) || inRange(id, c.GID, c.Identities) {
			errs = append(errs, fmt.Errorf("the UID/GID range must not include %d", id))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("firecracker: invalid jailer config: %w", err)
	}
	return nil
}

// Argv validates req against c and returns the jailer command line.
func (c *JailerConfig) Argv(req StartRequest) ([]string, error) {
	if err := sandbox.ValidateID(req.ID); err != nil {
		return nil, err
	}
	if req.Slot < 0 || req.Slot >= c.Identities {
		return nil, fmt.Errorf("firecracker: slot %d outside 0..%d", req.Slot, c.Identities-1)
	}
	if req.CPUs < 1 || req.CPUs > sandbox.MaxCPUs {
		return nil, fmt.Errorf("firecracker: cpus %d outside 1..%d", req.CPUs, sandbox.MaxCPUs)
	}
	if req.MemoryMiB < sandbox.MinMemoryMiB || req.MemoryMiB > sandbox.MaxMemoryMiB {
		return nil, fmt.Errorf("firecracker: memory %d MiB outside %d..%d", req.MemoryMiB, sandbox.MinMemoryMiB, sandbox.MaxMemoryMiB)
	}
	if req.DiskMiB < 0 || req.DiskMiB > sandbox.MaxDiskMiB {
		return nil, fmt.Errorf("firecracker: disk %d MiB outside 0..%d", req.DiskMiB, sandbox.MaxDiskMiB)
	}
	memMax := int64(req.MemoryMiB+MemoryOverheadMiB) << 20
	return []string{
		c.JailerPath,
		"--id", req.ID,
		"--exec-file", c.FirecrackerPath,
		"--uid", strconv.Itoa(c.UID + req.Slot),
		"--gid", strconv.Itoa(c.GID + req.Slot),
		"--chroot-base-dir", c.ChrootBase,
		"--cgroup-version", "2",
		"--cgroup", "memory.max=" + strconv.FormatInt(memMax, 10),
		"--cgroup", "pids.max=" + strconv.Itoa(MaxPids),
		"--cgroup", fmt.Sprintf("cpu.max=%d 100000", req.CPUs*100000),
		"--",
		"--no-api",
		"--config-file", "config.json",
	}, nil
}

func inRange(id, first, n int) bool { return id >= first && id < first+n }
