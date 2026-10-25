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
	"strconv"
	"strings"

	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
)

// Host performs the only operations that need root. Keeping them behind
// this narrow interface lets the provider itself run unprivileged.
type Host interface {
	// StartVM starts the jailer for req as root, streaming the guest
	// console to console.
	StartVM(req StartRequest, console io.Writer) (Process, error)
	// KillJail kills every process in the sandbox's cgroup.
	KillJail(ctx context.Context, id string) error
	// RemoveJail deletes the sandbox's chroot and cgroup. Missing ones
	// are not an error.
	RemoveJail(ctx context.Context, id string) error
}

// Process is a started VM.
type Process interface {
	Wait() error
	Signal(os.Signal) error
}

// DefaultCgroupRoot is the cgroup v2 mount.
const DefaultCgroupRoot = "/sys/fs/cgroup"

// HelperHost is the production Host: every root operation is one call of
// the root-owned hcc-helper through sudo. The helper builds the jailer
// command line from its own root-owned configuration, so the runner can
// only choose an ID, a slot and the guest size, all re-validated there.
type HelperHost struct {
	// Prefix runs a command as root, normally {"sudo", "-n"}.
	Prefix     []string
	HelperPath string
}

// NewHelperHost returns a HelperHost using "sudo -n".
func NewHelperHost(helperPath string) *HelperHost {
	return &HelperHost{Prefix: []string{"sudo", "-n"}, HelperPath: helperPath}
}

// StartVM implements Host.
func (h *HelperHost) StartVM(req StartRequest, console io.Writer) (Process, error) {
	if err := sandbox.ValidateID(req.ID); err != nil {
		return nil, err
	}
	return startProcess(h.Prefix, []string{h.HelperPath, "start",
		"--id", req.ID, "--slot", strconv.Itoa(req.Slot),
		"--cpus", strconv.Itoa(req.CPUs), "--memory", strconv.Itoa(req.MemoryMiB),
	}, console)
}

// KillJail implements Host.
func (h *HelperHost) KillJail(ctx context.Context, id string) error {
	return h.call(ctx, "kill", id)
}

// RemoveJail implements Host.
func (h *HelperHost) RemoveJail(ctx context.Context, id string) error {
	return h.call(ctx, "remove", id)
}

func (h *HelperHost) call(ctx context.Context, op, id string) error {
	if err := sandbox.ValidateID(id); err != nil {
		return err
	}
	return runQuiet(ctx, h.Prefix, h.HelperPath, op, "--id", id)
}

// SudoHost runs the jailer, rm, rmdir and tee directly through sudo. It
// needs broad sudo rights and exists for development and tests; use
// HelperHost in production.
type SudoHost struct {
	// Prefix runs a command as root, e.g. {"sudo", "-n"}. Empty runs
	// commands directly (tests).
	Prefix     []string
	Jailer     JailerConfig
	CgroupRoot string
}

// NewSudoHost returns a SudoHost using "sudo -n".
func NewSudoHost(jailer JailerConfig) *SudoHost {
	return &SudoHost{Prefix: []string{"sudo", "-n"}, Jailer: jailer, CgroupRoot: DefaultCgroupRoot}
}

// StartVM implements Host.
func (h *SudoHost) StartVM(req StartRequest, console io.Writer) (Process, error) {
	argv, err := h.Jailer.Argv(req)
	if err != nil {
		return nil, err
	}
	return startProcess(h.Prefix, argv, console)
}

// KillJail implements Host.
func (h *SudoHost) KillJail(ctx context.Context, id string) error {
	if err := sandbox.ValidateID(id); err != nil {
		return err
	}
	dir := CgroupDir(h.CgroupRoot, id)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil // already gone
	}
	cmd := command(ctx, h.Prefix, "tee", filepath.Join(dir, "cgroup.kill"))
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
	jail := JailDir(h.Jailer.ChrootBase, id)
	if _, err := os.Lstat(jail); err == nil {
		if err := runQuiet(ctx, h.Prefix, "rm", "-rf", "--one-file-system", "--", jail); err != nil {
			errs = append(errs, err)
		}
	}
	if cg := CgroupDir(h.CgroupRoot, id); dirExists(cg) {
		if err := runQuiet(ctx, h.Prefix, "rmdir", cg); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// JailDir is a sandbox's directory below the chroot base.
func JailDir(base, id string) string { return filepath.Join(base, "firecracker", id) }

// CgroupDir is a sandbox's cgroup; "firecracker" is the jailer's default
// parent cgroup.
func CgroupDir(root, id string) string { return filepath.Join(root, "firecracker", id) }

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func command(ctx context.Context, prefix []string, argv ...string) *exec.Cmd {
	full := append(append([]string{}, prefix...), argv...)
	return exec.CommandContext(ctx, full[0], full[1:]...) //nolint:gosec // argv is built from validated ids and configured paths
}

func runQuiet(ctx context.Context, prefix []string, argv ...string) error {
	if out, err := command(ctx, prefix, argv...).CombinedOutput(); err != nil {
		return fmt.Errorf("firecracker: %s: %w: %s", filepath.Base(argv[0]), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func startProcess(prefix, argv []string, console io.Writer) (Process, error) {
	// No context: the provider stops the VM itself (SIGTERM, then
	// cgroup.kill), because SIGKILL on sudo would orphan the VMM.
	cmd := command(context.Background(), prefix, argv...)
	cmd.Stdin = nil
	cmd.Stdout = console
	cmd.Stderr = console
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("firecracker: start: %w", err)
	}
	return cmdProcess{cmd}, nil
}

type cmdProcess struct{ cmd *exec.Cmd }

func (p cmdProcess) Wait() error              { return p.cmd.Wait() }
func (p cmdProcess) Signal(s os.Signal) error { return p.cmd.Process.Signal(s) }
