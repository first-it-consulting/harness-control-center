//go:build unix

// Command hcc-helper performs the runner's root-only operations. It is
// installed root-owned and is the single command the runner user may run
// through sudo. See internal/helper.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"

	"github.com/first-it-consulting/harness-control-center/internal/helper"
	"github.com/first-it-consulting/harness-control-center/internal/runnerd"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox/firecracker"
)

// configPath is fixed: a caller able to choose the configuration would
// choose which binaries run as root.
const configPath = "/etc/hcc/runner.yaml"

func main() {
	cfg, caller, err := setup()
	if err != nil {
		fmt.Fprintln(os.Stderr, "hcc-helper:", err)
		os.Exit(1)
	}
	os.Exit(helper.Main(cfg, os.Args[1:], caller, helper.Deps{
		Exec: syscall.Exec,
		Run: func(ctx context.Context, stdin io.Reader, argv ...string) error {
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv built by the helper
			cmd.Stdin = stdin
			cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("%s: %w: %s", argv[0], err, out)
			}
			return nil
		},
	}, os.Stdout, os.Stderr))
}

func setup() (helper.Config, int, error) {
	if os.Geteuid() != 0 {
		return helper.Config{}, 0, fmt.Errorf("must run as root (through sudo)")
	}
	// sudo records the real caller; without it, refuse.
	caller, err := strconv.Atoi(os.Getenv("SUDO_UID"))
	if err != nil {
		return helper.Config{}, 0, fmt.Errorf("must be called through sudo")
	}
	c, err := runnerd.LoadConfig(configPath)
	if err != nil {
		return helper.Config{}, 0, err
	}
	if err := c.CheckFiles(); err != nil {
		return helper.Config{}, 0, err
	}
	u, err := user.Lookup(c.RunnerUser)
	if err != nil {
		return helper.Config{}, 0, fmt.Errorf("runner user: %w", err)
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil || uid == 0 {
		return helper.Config{}, 0, fmt.Errorf("runner user %q must be a non-root user", c.RunnerUser)
	}
	netCfg, err := c.NetworkConfig()
	if err != nil {
		return helper.Config{}, 0, err
	}
	images := make(map[string]helper.Image, len(c.Images))
	for name, img := range c.Images {
		hi := helper.Image{Kernel: img.Kernel, RootFS: img.RootFS}
		if img.Tools != nil {
			hi.Tools = img.Tools.Drive
		}
		images[name] = hi
	}
	return helper.Config{
		Jailer: firecracker.JailerConfig{
			JailerPath:      c.Firecracker.Jailer,
			FirecrackerPath: c.Firecracker.Firecracker,
			ChrootBase:      c.Firecracker.ChrootBase,
			UID:             c.Firecracker.UID,
			GID:             c.Firecracker.GID,
			Identities:      c.VMSlots(),
			ForbiddenIDs:    []int{0, uid, gid},
		},
		StagingBase: c.Firecracker.StagingDir,
		CgroupRoot:  firecracker.DefaultCgroupRoot,
		RunnerUID:   uid,
		RootUID:     0,
		Images:      images,
		Network:     netCfg,
		IPTool:      "/usr/sbin/ip",
		NFTTool:     "/usr/sbin/nft",
	}, caller, nil
}
