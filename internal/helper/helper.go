//go:build unix

// Package helper is hcc-helper, the only code that runs as root. The
// runner calls it through sudo with a single sudoers rule; it performs
// exactly these operations and trusts nothing it is given:
//
//	start    --id ID --image NAME --slot N --cpus C --memory M --disk D
//	                                                 build the jail, exec the jailer
//	collect  --id ID                                 stream the jail's disk to stdout
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
//
// The guest's writable disk is a sparse file in the jail, owned by the
// slot's identity. Its full size is reserved against the free space when
// a jail is built. collect only copies its bytes out; root never parses
// or mounts what a guest wrote.
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
	// Images are the bootable images by name. Their files (and every
	// directory above them) must be root-owned and writable only by root.
	Images map[string]Image
	// Network enables net-up/net-down; nil refuses them.
	Network *firecracker.NetworkConfig
	// IPTool and NFTTool are the absolute paths of ip(8) and nft(8).
	IPTool, NFTTool string
	// SysctlRoot is /proc/sys, except in tests.
	SysctlRoot string
}

// Image is one bootable image (root-side configuration).
type Image struct {
	Kernel, RootFS string
	// Tools is the image's tool catalog drive (optional, ADR-0010).
	Tools string
}

// Size caps for staged files. Root copies them, so without a cap a
// compromised runner could fill the root filesystem (including the blocks
// reserved for root).
const (
	MaxConfigBytes = 1 << 20
	MaxTaskBytes   = sandbox.MaxTaskBytes + 512 // padded to whole sectors
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
	// Chown changes a file's owner (default: fchown).
	Chown func(f *os.File, uid, gid int) error
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
	if d.FreeBytes == nil {
		d.FreeBytes = freeBytes
	}
	if d.Chown == nil {
		d.Chown = func(f *os.File, uid, gid int) error { return f.Chown(uid, gid) }
	}
	if d.LinkExists == nil {
		d.LinkExists = func(name string) bool {
			_, err := os.Lstat(filepath.Join("/sys/class/net", name))
			return err == nil
		}
	}
	if len(args) == 0 {
		return errors.New("expected start, collect, kill, remove, net-up or net-down")
	}
	op, rest := args[0], args[1:]
	if op == "net-up" || op == "net-down" {
		return runNet(cfg, d, op, rest, stdout)
	}
	fs := flag.NewFlagSet(op, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	id := fs.String("id", "", "")
	var slot, cpus, memory, disk *int
	var image *string
	var tools *bool
	if op == "start" {
		tools = fs.Bool("tools", false, "")
		image = fs.String("image", "", "")
		slot = fs.Int("slot", -1, "")
		cpus = fs.Int("cpus", 0, "")
		memory = fs.Int("memory", 0, "")
		disk = fs.Int("disk", 0, "")
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
	if op == "start" || op == "remove" || op == "collect" {
		if err := checkRootOwnedDirs(base, cfg.RootUID, cfg.TrustAnchor); err != nil {
			return fmt.Errorf("chroot base: %w", err)
		}
	}

	switch op {
	case "start":
		req := firecracker.StartRequest{ID: *id, Slot: *slot, CPUs: *cpus, MemoryMiB: *memory, DiskMiB: *disk, Tools: *tools}
		argv, err := cfg.Jailer.Argv(req)
		if err != nil {
			return err
		}
		img, ok := cfg.Images[*image]
		if !ok {
			return fmt.Errorf("unknown image %q", *image)
		}
		if req.Tools && img.Tools == "" {
			return fmt.Errorf("image %q has no tool catalog", *image)
		}
		if err := buildJail(cfg, d, base, req, img); err != nil {
			return err
		}
		return d.Exec(argv[0], argv, execEnv)
	case "collect":
		return collectDisk(cfg, jail, stdout)
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

// stagedFiles are the only files copied from the runner into a jail:
// name, size limit, and whether it is optional.
var stagedFiles = []struct {
	name     string
	limit    int64
	optional bool
}{
	{"config.json", MaxConfigBytes, false},
	{"task.img", MaxTaskBytes, true},
}

// MinFreeBytes must remain free on the jail filesystem after a jail is
// built, counted as space available to unprivileged users, so root's
// reserved blocks are never touched.
const MinFreeBytes = 1 << 30

// diskName is the guest's writable disk in the jail root.
const diskName = "disk.img"

// buildJail creates base/firecracker/<id>/root as root, links (or
// copies) the image into it and copies the staged files, all read-only
// for the VM, and creates the VM's disk. On any failure the partial jail
// is removed.
func buildJail(cfg Config, d Deps, base string, req firecracker.StartRequest, img Image) (err error) {
	id := req.ID
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
	for _, sf := range stagedFiles {
		f, err := openStaged(staging, sf.name, cfg.RunnerUID, sf.limit)
		if errors.Is(err, unix.ENOENT) && sf.optional {
			continue
		}
		if err != nil {
			return err
		}
		files[sf.name] = f
		total += f.size
	}
	// The image must be root's all the way up: the runner chooses only
	// its name.
	images := map[string]string{"vmlinux": img.Kernel, "rootfs.ext4": img.RootFS}
	if req.Tools {
		images["tools.ext4"] = img.Tools
	}
	for _, p := range images {
		if err := checkRootOwnedFile(p, cfg.RootUID, cfg.TrustAnchor); err != nil {
			return fmt.Errorf("image: %w", err)
		}
	}
	// Sparse disks grow as guests write: count what every existing jail's
	// disk may still take, and this one's in full.
	diskBytes := int64(req.DiskMiB) << 20
	total += diskBytes + reservedDiskBytes(parent, entries)
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
	for name, f := range files {
		if err := f.copyTo(filepath.Join(root, name)); err != nil {
			return err
		}
	}
	for dst, src := range images {
		if err := linkOrCopy(d, base, src, filepath.Join(root, dst)); err != nil {
			return fmt.Errorf("image %s: %w", dst, err)
		}
	}
	if diskBytes > 0 {
		if err := createDisk(d, filepath.Join(root, diskName), diskBytes, cfg.Jailer.UID+req.Slot, cfg.Jailer.GID+req.Slot); err != nil {
			return fmt.Errorf("disk: %w", err)
		}
	}
	return nil
}

// createDisk creates the VM's sparse disk, owner-only for the slot's
// identity (the VMM opens it as that user; the root-owned jail root keeps
// it from being replaced or linked).
func createDisk(d Deps, path string, size int64, uid, gid int) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600) //nolint:gosec // path is in the jail we just created
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		return err
	}
	if err := d.Chown(f, uid, gid); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// reservedDiskBytes is what the existing jails' sparse disks may still
// allocate.
func reservedDiskBytes(parent string, jails []os.DirEntry) int64 {
	var n int64
	for _, j := range jails {
		var st unix.Stat_t
		if unix.Lstat(filepath.Join(parent, j.Name(), "root", diskName), &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
			continue
		}
		n += max(0, st.Size-st.Blocks*512)
	}
	return n
}

// collectDisk copies a jail's disk to w, up to the end of its last data
// (a guest writes from the start; the zeros after that need no copying).
// The jail directory must be root's all the way up. Its root/ belongs to
// the VM's identity once the jailer ran, so inside it nothing is trusted:
// root/ and the disk are opened without following links, and the disk
// must be a regular, singly linked file within the size limit. Its
// content is not looked at.
func collectDisk(cfg Config, jail string, w io.Writer) error {
	if err := checkRootOwnedDirs(jail, cfg.RootUID, cfg.TrustAnchor); err != nil {
		return err
	}
	dir, err := openNoFollow(filepath.Join(jail, "root"), true)
	if err != nil {
		return err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), diskName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("disk: %w", err)
	}
	f := os.NewFile(uintptr(fd), diskName)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Size < 0 || st.Size > int64(sandbox.MaxDiskMiB)<<20 {
		return errors.New("disk must be a regular, singly linked file within the size limit")
	}
	_, err = io.Copy(w, io.NewSectionReader(f, 0, dataEnd(f, st.Size)))
	return err
}

// dataEnd is where f's last data region ends (size if the filesystem
// cannot tell).
func dataEnd(f *os.File, size int64) int64 {
	var end, off int64
	for off < size {
		data, err := f.Seek(off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break // only holes from off on
		}
		if err != nil {
			return size
		}
		hole, err := f.Seek(data, unix.SEEK_HOLE)
		if err != nil || hole <= data {
			return size
		}
		end, off = hole, hole
	}
	return min(end, size)
}

// linkOrCopy hard-links a root-owned image file into the jail (no copy:
// images can be large, and the VM opens them read-only, as a UID that
// cannot write them). Across filesystems it copies, if space allows.
func linkOrCopy(d Deps, base, src, dst string) error {
	err := os.Link(src, dst)
	if err == nil || !errors.Is(err, unix.EXDEV) {
		return err
	}
	in, err := os.Open(src) //nolint:gosec // root-owned image path from the configuration
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	free, err := d.FreeBytes(base)
	if err != nil {
		return err
	}
	if uint64(fi.Size())+MinFreeBytes > free { //nolint:gosec // file sizes are non-negative
		return fmt.Errorf("not enough space to copy %s (%d bytes free)", src, free)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o444) //nolint:gosec // read-only for the VM
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// checkRootOwnedFile requires a regular file owned by rootUID, writable
// only by it, in a directory chain that is root's (checkRootOwnedDirs).
func checkRootOwnedFile(path string, rootUID int, anchor string) error {
	if !cleanAbs(path) {
		return fmt.Errorf("path %q must be clean and absolute", path)
	}
	if err := checkRootOwnedDirs(filepath.Dir(path), rootUID, anchor); err != nil {
		return err
	}
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || int(st.Uid) != rootUID || st.Mode&0o022 != 0 {
		return fmt.Errorf("%s must be a regular file owned by root and writable only by root", path)
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
