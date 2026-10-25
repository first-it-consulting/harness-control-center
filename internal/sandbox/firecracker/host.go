package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
)

// Host performs the only operations that need root. Keeping them behind
// this narrow interface lets the provider itself run unprivileged, and
// lets a dedicated privileged helper replace sudo without touching it.
type Host interface {
	// StartJailer starts the jailer with argv (argv[0] is the jailer
	// binary) as root, streaming the guest console to console.
	StartJailer(argv []string, console io.Writer) (Process, error)
	// KillJail kills every process in the sandbox's cgroup.
	KillJail(ctx context.Context, id string) error
	// RemoveJail deletes the sandbox's chroot and cgroup. Missing ones
	// are not an error.
	RemoveJail(ctx context.Context, id string) error
}

// Process is a started jailer.
type Process interface {
	Wait() error
	Signal(os.Signal) error
}

// SudoHost implements Host with non-interactive sudo. Its sudoers rule
// can be restricted to exactly the jailer, rm and tee invocations below.
type SudoHost struct {
	// Prefix runs a command as root, e.g. {"sudo", "-n"}. Empty runs
	// commands directly (tests).
	Prefix []string
	// ChrootBase must equal the provider's Config.ChrootBase.
	ChrootBase string
	// CgroupRoot is the cgroup v2 mount, normally /sys/fs/cgroup.
	CgroupRoot string
}

// NewSudoHost returns a SudoHost using "sudo -n" and /sys/fs/cgroup.
func NewSudoHost(chrootBase string) *SudoHost {
	return &SudoHost{Prefix: []string{"sudo", "-n"}, ChrootBase: chrootBase, CgroupRoot: "/sys/fs/cgroup"}
}

func (h *SudoHost) command(ctx context.Context, argv ...string) *exec.Cmd {
	full := append(append([]string{}, h.Prefix...), argv...)
	return exec.CommandContext(ctx, full[0], full[1:]...) //nolint:gosec // argv is built from validated ids and configured paths
}

// StartJailer implements Host.
func (h *SudoHost) StartJailer(argv []string, console io.Writer) (Process, error) {
	if len(argv) == 0 {
		return nil, errors.New("firecracker: empty jailer command")
	}
	// No context: the provider stops the VM itself (SIGTERM, then
	// cgroup.kill), because SIGKILL on sudo would orphan the VMM.
	cmd := h.command(context.Background(), argv...)
	cmd.Stdin = nil
	cmd.Stdout = console
	cmd.Stderr = console
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("firecracker: start jailer: %w", err)
	}
	return cmdProcess{cmd}, nil
}

// KillJail implements Host by writing to cgroup.kill, which kills every
// process in the cgroup, even ones that ignore signals.
func (h *SudoHost) KillJail(ctx context.Context, id string) error {
	if err := sandbox.ValidateID(id); err != nil {
		return err
	}
	dir := h.cgroupDir(id)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil // already gone
	}
	cmd := h.command(ctx, "tee", filepath.Join(dir, "cgroup.kill"))
	cmd.Stdin = strings.NewReader("1\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("firecracker: kill %s: %w: %s", id, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RemoveJail implements Host.
func (h *SudoHost) RemoveJail(ctx context.Context, id string) error {
	if err := sandbox.ValidateID(id); err != nil {
		return err
	}
	var errs []error
	jail := jailDir(h.ChrootBase, id)
	if _, err := os.Lstat(jail); err == nil {
		// --one-file-system: never descend into anything mounted below.
		cmd := h.command(ctx, "rm", "-rf", "--one-file-system", "--", jail)
		if out, err := cmd.CombinedOutput(); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w: %s", jail, err, strings.TrimSpace(string(out))))
		}
	}
	if cg := h.cgroupDir(id); dirExists(cg) {
		if out, err := h.command(ctx, "rmdir", cg).CombinedOutput(); err != nil {
			errs = append(errs, fmt.Errorf("remove cgroup %s: %w: %s", cg, err, strings.TrimSpace(string(out))))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("firecracker: %w", errors.Join(errs...))
	}
	return nil
}

func (h *SudoHost) cgroupDir(id string) string {
	// The jailer's default parent cgroup is "firecracker".
	return filepath.Join(h.CgroupRoot, "firecracker", id)
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

type cmdProcess struct{ cmd *exec.Cmd }

func (p cmdProcess) Wait() error              { return p.cmd.Wait() }
func (p cmdProcess) Signal(s os.Signal) error { return p.cmd.Process.Signal(s) }
