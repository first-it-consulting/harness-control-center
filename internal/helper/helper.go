//go:build unix

// Package helper is hcc-helper, the only code that runs as root. The
// runner calls it through sudo with a single sudoers rule; it performs
// exactly three operations and trusts nothing it is given:
//
//	start  --id ID --slot N --cpus C --memory M   exec the jailer for a prepared jail
//	kill   --id ID                                 kill everything in the jail's cgroup
//	remove --id ID                                 delete the jail and its cgroup
//
// Binaries, the chroot base and the UID range come from the root-owned
// configuration, never from the caller. Only the configured runner user
// may call it, and it refuses jail trees containing symlinks or owned by
// anyone else, so a compromised runner cannot redirect the chroot (or a
// deletion) to another place on the host.
package helper

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox/firecracker"
)

// Config is the helper's root-owned configuration.
type Config struct {
	Jailer     firecracker.JailerConfig
	CgroupRoot string
	// RunnerUID is the only user allowed to call the helper.
	RunnerUID int
}

// Deps are the side effects, replaceable in tests.
type Deps struct {
	// Exec replaces the process (syscall.Exec in production), so the
	// runner's signals reach the jailer and Firecracker directly.
	Exec func(argv0 string, argv, env []string) error
	// Run runs a command to completion.
	Run func(ctx context.Context, argv ...string) error
}

// execEnv is the entire environment the jailer gets.
var execEnv = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}

// Main runs one helper invocation. callerUID is the real caller (sudo's
// SUDO_UID). It returns the process exit code.
func Main(cfg Config, args []string, callerUID int, d Deps, stderr io.Writer) int {
	if err := run(cfg, args, callerUID, d); err != nil {
		fmt.Fprintln(stderr, "hcc-helper:", err)
		return 1
	}
	return 0
}

func run(cfg Config, args []string, callerUID int, d Deps) error {
	if callerUID != cfg.RunnerUID {
		return fmt.Errorf("caller uid %d is not the runner (uid %d)", callerUID, cfg.RunnerUID)
	}
	if err := cfg.Jailer.Validate(); err != nil {
		return err
	}
	if len(args) == 0 {
		return errors.New("expected start, kill or remove")
	}
	op, rest := args[0], args[1:]
	fs := flag.NewFlagSet(op, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	id := fs.String("id", "", "")
	var slot, cpus, memory *int
	if op == "start" {
		slot = fs.Int("slot", -1, "")
		cpus = fs.Int("cpus", 0, "")
		memory = fs.Int("memory", 0, "")
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if err := sandbox.ValidateID(*id); err != nil {
		return err
	}
	jail := firecracker.JailDir(cfg.Jailer.ChrootBase, *id)
	cgroup := firecracker.CgroupDir(cfg.CgroupRoot, *id)

	switch op {
	case "start":
		argv, err := cfg.Jailer.Argv(firecracker.StartRequest{ID: *id, Slot: *slot, CPUs: *cpus, MemoryMiB: *memory})
		if err != nil {
			return err
		}
		if err := checkJail(cfg.Jailer.ChrootBase, filepath.Join(jail, "root"), cfg.RunnerUID); err != nil {
			return err
		}
		return d.Exec(argv[0], argv, execEnv)
	case "kill":
		if _, err := os.Lstat(cgroup); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return writeCgroupKill(filepath.Join(cgroup, "cgroup.kill"))
	case "remove":
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var errs []error
		if _, err := os.Lstat(jail); err == nil {
			if err := noSymlinks(jail); err != nil {
				return err
			}
			// rm does not follow symlinks below jail; --one-file-system
			// keeps it out of anything mounted there.
			if err := d.Run(ctx, "rm", "-rf", "--one-file-system", "--", jail); err != nil {
				errs = append(errs, err)
			}
		}
		if fi, err := os.Lstat(cgroup); err == nil && fi.IsDir() {
			if err := os.Remove(cgroup); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	default:
		return fmt.Errorf("unknown operation %q", op)
	}
}

// checkJail verifies the prepared jail root before the jailer chroots
// into it: no component of the path may be a symlink, and everything from
// the chroot base down must be a directory owned by the runner.
func checkJail(base, root string, runnerUID int) error {
	if err := noSymlinks(root); err != nil {
		return err
	}
	rel, err := filepath.Rel(base, root)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("jail %s is outside the chroot base", root)
	}
	p := base
	for _, part := range append([]string{""}, strings.Split(rel, string(filepath.Separator))...) {
		p = filepath.Join(p, part)
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !fi.IsDir() || !ok || int(st.Uid) != runnerUID {
			return fmt.Errorf("%s must be a directory owned by the runner", p)
		}
	}
	return nil
}

// noSymlinks fails if any existing component of path is a symlink.
func noSymlinks(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("path %q must be clean and absolute", path)
	}
	for p := path; ; p = filepath.Dir(p) {
		if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; refusing", p)
		}
		if p == filepath.Dir(p) {
			return nil
		}
	}
}

// writeCgroupKill writes "1" to an existing cgroup.kill. It never creates
// the file: if it is missing, the directory is not a cgroup, and root has
// no business writing there.
func writeCgroupKill(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0) //nolint:gosec // path built from the configured cgroup root and a validated id
	if err != nil {
		return err
	}
	if _, err := f.WriteString("1\n"); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
