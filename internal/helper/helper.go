//go:build unix

// Package helper is hcc-helper, the only code that runs as root. The
// runner calls it through sudo with a single sudoers rule; it performs
// exactly these operations and trusts nothing it is given:
//
//	start    --id ID --slot N --cpus C --memory M   build the jail, exec the jailer
//	kill     --id ID                                 kill everything in the jail's cgroup
//	remove   --id ID                                 delete the jail and its cgroup
//	net-up   --slot N                                create slot N's tap link (prints its NetInfo)
//	net-down --slot N                                delete slot N's tap link
//
// Binaries, directories and the UID range come from the root-owned
// configuration, never from the caller, and only the configured runner
// user may call it.
//
// The jail tree is root's: the chroot base and every directory above it
// must be owned by root and writable by nobody else, and the helper
// creates each jail itself. The runner only prepares the VM's files in
// its own staging directory, which the helper reads without following
// any symlink and copies only if they are regular files owned by the
// runner. A compromised runner therefore cannot swap parts of a jail
// between check and use, redirect a chroot or a deletion, or make root
// copy files it could not read itself.
package helper

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox/firecracker"
)

// Config is the helper's root-owned configuration.
type Config struct {
	Jailer firecracker.JailerConfig
	// StagingBase is the runner's directory of prepared VM files:
	// StagingBase/<id>/{vmlinux,rootfs.ext4,config.json}.
	StagingBase string
	CgroupRoot  string
	// RunnerUID is the only user allowed to call the helper and the
	// required owner of staged files.
	RunnerUID int
	// RootUID must own the jail tree: 0, except in tests.
	RootUID int
	// TrustAnchor is where ownership checks stop walking up: "/" (the
	// default), except in tests.
	TrustAnchor string
	// MaxRootFSBytes caps the staged root filesystem (default
	// DefaultMaxRootFSBytes): root copies it onto a root-owned filesystem.
	MaxRootFSBytes int64
	// Network enables net-up/net-down; nil refuses them.
	Network *firecracker.NetworkConfig
	// IPTool and NFTTool are the absolute paths of ip(8) and nft(8).
	IPTool, NFTTool string
	// SysctlRoot is /proc/sys, except in tests.
	SysctlRoot string
}

// Size caps for staged files. Root copies them, so without a cap a
// compromised runner could fill the root filesystem (including the blocks
// reserved for root).
const (
	MaxKernelBytes        = 128 << 20
	MaxConfigBytes        = 1 << 20
	DefaultMaxRootFSBytes = 8 << 30
)

// Deps are the side effects, replaceable in tests.
type Deps struct {
	// Exec replaces the process (syscall.Exec in production), so the
	// runner's signals reach the jailer and Firecracker directly.
	Exec func(argv0 string, argv, env []string) error
	// Run runs a command to completion, feeding it stdin (may be nil).
	Run func(ctx context.Context, stdin io.Reader, argv ...string) error
	// LinkExists reports whether a network interface exists (default:
	// /sys/class/net/<name>).
	LinkExists func(name string) bool
	// FreeBytes reports unprivileged free space on a path's filesystem
	// (default: statfs).
	FreeBytes func(path string) (uint64, error)
}

// execEnv is the entire environment the jailer gets.
var execEnv = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}

// Main runs one helper invocation. callerUID is the real caller (sudo's
// SUDO_UID). It returns the process exit code.
func Main(cfg Config, args []string, callerUID int, d Deps, stdout, stderr io.Writer) int {
	if err := run(cfg, args, callerUID, d, stdout); err != nil {
		fmt.Fprintln(stderr, "hcc-helper:", err)
		return 1
	}
	return 0
}

func run(cfg Config, args []string, callerUID int, d Deps, stdout io.Writer) error {
	if callerUID != cfg.RunnerUID {
		return fmt.Errorf("caller uid %d is not the runner (uid %d)", callerUID, cfg.RunnerUID)
	}
	if err := cfg.Jailer.Validate(); err != nil {
		return err
	}
	if !cleanAbs(cfg.StagingBase) || !cleanAbs(cfg.CgroupRoot) {
		return errors.New("staging base and cgroup root must be clean absolute paths")
	}
	if cfg.TrustAnchor == "" {
		cfg.TrustAnchor = "/"
	}
	if cfg.MaxRootFSBytes <= 0 {
		cfg.MaxRootFSBytes = DefaultMaxRootFSBytes
	}
	if d.FreeBytes == nil {
		d.FreeBytes = freeBytes
	}
	if d.LinkExists == nil {
		d.LinkExists = func(name string) bool {
			_, err := os.Lstat(filepath.Join("/sys/class/net", name))
			return err == nil
		}
	}
	if len(args) == 0 {
		return errors.New("expected start, kill, remove, net-up or net-down")
	}
	op, rest := args[0], args[1:]
	if op == "net-up" || op == "net-down" {
		return runNet(cfg, d, op, rest, stdout)
	}
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
	base := cfg.Jailer.ChrootBase
	jail := firecracker.JailDir(base, *id)
	cgroup := firecracker.CgroupDir(cfg.CgroupRoot, *id)
	// Fail closed: the chroot base must exist and be root's all the way
	// up before anything below it is created, used or deleted.
	if op == "start" || op == "remove" {
		if err := checkRootOwnedDirs(base, cfg.RootUID, cfg.TrustAnchor); err != nil {
			return fmt.Errorf("chroot base: %w", err)
		}
	}

	switch op {
	case "start":
		argv, err := cfg.Jailer.Argv(firecracker.StartRequest{ID: *id, Slot: *slot, CPUs: *cpus, MemoryMiB: *memory})
		if err != nil {
			return err
		}
		if err := buildJail(cfg, d, base, *id); err != nil {
			return err
		}
		return d.Exec(argv[0], argv, execEnv)
	case "kill":
		if _, err := os.Lstat(cgroup); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return writeCgroupKill(filepath.Join(cgroup, "cgroup.kill"))
	case "remove":
		// The base is root's (checked above); so must be firecracker/ if it
		// exists. Then nobody but root can redirect rm.
		parent := filepath.Join(base, "firecracker")
		if _, err := os.Lstat(parent); err == nil {
			if err := checkRootOwnedDirs(parent, cfg.RootUID, cfg.TrustAnchor); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var errs []error
		if _, err := os.Lstat(jail); err == nil {
			if err := d.Run(ctx, nil, "rm", "-rf", "--one-file-system", "--", jail); err != nil {
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

// stagedFiles are the only files copied into a jail.
var stagedFiles = []string{"vmlinux", "rootfs.ext4", "config.json"}

func (cfg Config) maxBytes(name string) int64 {
	switch name {
	case "vmlinux":
		return MaxKernelBytes
	case "config.json":
		return MaxConfigBytes
	default:
		return cfg.MaxRootFSBytes
	}
}

// MinFreeBytes must remain free on the jail filesystem after a jail is
// built, counted as space available to unprivileged users, so root's
// reserved blocks are never touched.
const MinFreeBytes = 1 << 30

// buildJail creates base/firecracker/<id>/root as root and copies the
// staged files into it, read-only for the VM. On any failure the partial
// jail is removed.
func buildJail(cfg Config, d Deps, base, id string) (err error) {
	parent := filepath.Join(base, "firecracker")
	// 0711: the runner may check whether its jail still exists, but can
	// neither list nor change anything.
	if err := os.Mkdir(parent, 0o711); err != nil && !errors.Is(err, os.ErrExist) { //nolint:gosec // traverse-only for the runner
		return err
	}
	if err := checkRootOwnedDirs(parent, cfg.RootUID, cfg.TrustAnchor); err != nil {
		return err
	}
	// Serialize jail building across helper processes, so the count and
	// space checks below cannot be raced by concurrent starts. The lock is
	// held until the copies are done (and dropped on exec at the latest:
	// O_CLOEXEC).
	lock, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(lock)
	if err := unix.Flock(lock, unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock jails: %w", err)
	}
	// At most one jail per VM identity may exist; leftovers count, so a
	// runner cannot pile up jails by starting many IDs.
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	if len(entries) >= cfg.Jailer.Identities {
		return fmt.Errorf("%d jails exist already (limit %d); remove finished ones first", len(entries), cfg.Jailer.Identities)
	}

	// Open and check every staged file before writing anything.
	staging, err := openNoFollow(filepath.Join(cfg.StagingBase, id), true)
	if err != nil {
		return fmt.Errorf("staging: %w", err)
	}
	defer staging.Close()
	if err := checkOwner(staging, cfg.RunnerUID); err != nil {
		return err
	}
	files := make(map[string]*stagedFile, len(stagedFiles))
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	var total int64
	for _, name := range stagedFiles {
		f, err := openStaged(staging, name, cfg.RunnerUID, cfg.maxBytes(name))
		if err != nil {
			return err
		}
		files[name] = f
		total += f.size
	}
	free, err := d.FreeBytes(base)
	if err != nil {
		return err
	}
	if total < 0 || uint64(total)+MinFreeBytes > free {
		return fmt.Errorf("not enough space for a %d-byte jail (%d bytes free, %d must stay free)", total, free, MinFreeBytes)
	}

	jail := filepath.Join(parent, id)
	// Exclusive: a jail is never reused.
	if err := os.Mkdir(jail, 0o700); err != nil {
		return fmt.Errorf("create jail: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(jail) // ours, root-owned: safe to remove
		}
	}()
	root := filepath.Join(jail, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		return err
	}
	// The jail root becomes the VM's "/": its UID must traverse it.
	if err := os.Chmod(root, 0o711); err != nil { //nolint:gosec // traversal by the jailed UID is required
		return err
	}
	for _, name := range stagedFiles {
		if err := files[name].copyTo(filepath.Join(root, name)); err != nil {
			return err
		}
	}
	return nil
}

// stagedFile is a checked, open staged file.
type stagedFile struct {
	*os.File
	size, limit int64
}

// openStaged opens one staged file relative to the staging directory
// without following symlinks. Only regular, singly linked files owned by
// the runner and at most limit bytes long are accepted, so root never
// copies something the runner could not read itself, nor more than it
// was configured to.
func openStaged(staging *os.File, name string, runnerUID int, limit int64) (*stagedFile, error) {
	fd, err := unix.Openat(int(staging.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("staged %s: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = f.Close()
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || int(st.Uid) != runnerUID || st.Nlink != 1 {
		_ = f.Close()
		return nil, fmt.Errorf("staged %s must be a regular, singly linked file owned by the runner", name)
	}
	if st.Size > limit {
		_ = f.Close()
		return nil, fmt.Errorf("staged %s is %d bytes; the limit is %d", name, st.Size, limit)
	}
	return &stagedFile{File: f, size: st.Size, limit: limit}, nil
}

// copyTo copies the file into the jail, read-only and readable by the VM.
func (f *stagedFile) copyTo(dst string) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600) //nolint:gosec // dst is in the jail we just created
	if err != nil {
		return err
	}
	// Cap the copy itself: the runner could grow the file after the size
	// check (but not beyond what it was checked against).
	n, err := io.Copy(out, io.LimitReader(f, f.size+1))
	if err == nil && n > f.size {
		err = fmt.Errorf("%s grew after it was checked", f.Name())
	}
	if err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %s: %w", f.Name(), err)
	}
	if err := out.Chmod(0o444); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// freeBytes is the space on path's filesystem available to unprivileged
// users (excluding root's reserved blocks).
func freeBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	bsize := toInt64(st.Bsize)
	if bsize <= 0 {
		return 0, fmt.Errorf("%s: invalid block size %d", path, bsize)
	}
	return st.Bavail * uint64(bsize), nil
}

// openNoFollow opens path one component at a time from "/", refusing to
// follow a symlink anywhere, and returns the final file.
func openNoFollow(path string, dir bool) (*os.File, error) {
	if !cleanAbs(path) {
		return nil, fmt.Errorf("path %q must be clean and absolute", path)
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, part := range parts {
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if i < len(parts)-1 || dir {
			flags |= unix.O_DIRECTORY
		}
		next, err := unix.Openat(fd, part, flags, 0)
		_ = unix.Close(fd)
		if err != nil {
			if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
				return nil, fmt.Errorf("%s: a component is a symlink or not a directory; refusing", path)
			}
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), path), nil
}

func checkOwner(f *os.File, uid int) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	if int(st.Uid) != uid {
		return fmt.Errorf("%s must be owned by uid %d", f.Name(), uid)
	}
	return nil
}

// checkRootOwnedDirs fails unless dir and every directory above it, up to
// and including anchor, are real directories owned by rootUID and not
// writable by group or others: then nobody but root can rename, replace
// or add anything on that path.
func checkRootOwnedDirs(dir string, rootUID int, anchor string) error {
	if !cleanAbs(dir) || !cleanAbs(anchor) {
		return fmt.Errorf("path %q must be clean and absolute", dir)
	}
	if rel, err := filepath.Rel(anchor, dir); err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("%s is outside %s", dir, anchor)
	}
	for p := dir; ; p = filepath.Dir(p) {
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return fmt.Errorf("%s must be a directory (not a symlink)", p)
		}
		if int(st.Uid) != rootUID || st.Mode&0o022 != 0 {
			return fmt.Errorf("%s must be owned by root and writable only by root", p)
		}
		if p == anchor || p == filepath.Dir(p) {
			return nil
		}
	}
}

// writeCgroupKill writes "1" to an existing cgroup.kill. It never creates
// the file: if it is missing, the directory is not a cgroup, and root has
// no business writing there.
func writeCgroupKill(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|unix.O_NOFOLLOW, 0) //nolint:gosec // path built from the configured cgroup root and a validated id
	if err != nil {
		return err
	}
	if _, err := f.WriteString("1\n"); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func cleanAbs(p string) bool { return filepath.IsAbs(p) && filepath.Clean(p) == p }

// toInt64 converts Statfs_t.Bsize, which is int64 on Linux and uint32 on
// macOS.
func toInt64[T int64 | uint32](v T) int64 { return int64(v) }

// runNet creates or deletes slot N's link. Names, addresses and the tap
// owner (the slot's VM identity) all come from the configuration.
func runNet(cfg Config, d Deps, op string, args []string, stdout io.Writer) error {
	if cfg.Network == nil {
		return errors.New("networking is not configured")
	}
	if err := cfg.Network.Validate(cfg.Jailer.Identities); err != nil {
		return err
	}
	if !cleanAbs(cfg.IPTool) || !cleanAbs(cfg.NFTTool) {
		return errors.New("ip and nft tools must be absolute paths")
	}
	if cfg.SysctlRoot == "" {
		cfg.SysctlRoot = "/proc/sys"
	}
	fs := flag.NewFlagSet(op, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	slot := fs.Int("slot", -1, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if *slot < 0 || *slot >= cfg.Jailer.Identities {
		return fmt.Errorf("slot %d outside 0..%d", *slot, cfg.Jailer.Identities-1)
	}
	link := cfg.Network.Link(*slot)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ip := func(args ...string) error { return d.Run(ctx, nil, append([]string{cfg.IPTool}, args...)...) }

	if op == "net-down" {
		if d.LinkExists(link.Tap) {
			return ip("link", "del", "dev", link.Tap)
		}
		return nil
	}

	// The guard table first: never bring up a link without it.
	if err := d.Run(ctx, strings.NewReader(cfg.Network.Ruleset()), cfg.NFTTool, "-f", "-"); err != nil {
		return fmt.Errorf("firewall: %w", err)
	}
	if d.LinkExists(link.Tap) { // left over from a crash
		if err := ip("link", "del", "dev", link.Tap); err != nil {
			return err
		}
	}
	uid := strconv.Itoa(cfg.Jailer.UID + *slot)
	gid := strconv.Itoa(cfg.Jailer.GID + *slot)
	steps := [][]string{
		// Owned by the slot's VM identity, so only that VMM can attach.
		{"tuntap", "add", "dev", link.Tap, "mode", "tap", "user", uid, "group", gid},
		{"addr", "add", netip.PrefixFrom(link.Host, 30).String(), "dev", link.Tap},
		{"link", "set", "dev", link.Tap, "up"},
	}
	for _, step := range steps {
		if err := ip(step...); err != nil {
			_ = ip("link", "del", "dev", link.Tap)
			return err
		}
	}
	// No IPv6 on sandbox links (the guard drops it anyway).
	if p := filepath.Join(cfg.SysctlRoot, "net", "ipv6", "conf", link.Tap, "disable_ipv6"); fileExists(p) {
		if err := os.WriteFile(p, []byte("1\n"), 0o644); err != nil { //nolint:gosec // sysctl file
			_ = ip("link", "del", "dev", link.Tap)
			return err
		}
	}
	return json.NewEncoder(stdout).Encode(link)
}

func fileExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
