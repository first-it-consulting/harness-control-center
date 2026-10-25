// Package runnerd is the runner daemon: it exposes a runner.Runner over
// HTTP on a Unix socket that only the runner's user can open (ADR-0003),
// and provides the matching client.
package runnerd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// DefaultSocket is where the daemon listens unless configured otherwise.
const DefaultSocket = "/run/hcc/runner.sock"

// Config is the daemon configuration file (runner.yaml).
type Config struct {
	// Target is this runner's name as used in spec.target.
	Target        string `yaml:"target"`
	MaxConcurrent int    `yaml:"maxConcurrent"`
	// RunnerUser is the system user the daemon runs as; it is the only
	// user hcc-helper accepts calls from.
	RunnerUser string `yaml:"runnerUser"`
	Socket     string `yaml:"socket"`
	// SocketGroup, if set, may also use the socket (mode 0660): operators
	// in this group can drive the runner without being the runner user.
	SocketGroup string `yaml:"socketGroup"`
	// StateDir holds per-job console logs.
	StateDir    string                 `yaml:"stateDir"`
	Firecracker FirecrackerConfig      `yaml:"firecracker"`
	Images      map[string]ImageConfig `yaml:"images"`
}

// FirecrackerConfig configures the Firecracker sandbox provider.
type FirecrackerConfig struct {
	// Helper is the root-owned hcc-helper the runner calls through sudo.
	// Without it, the runner falls back to running the jailer through
	// sudo directly (development only: that needs broad sudo rights).
	Helper      string `yaml:"helper"`
	Jailer      string `yaml:"jailer"`
	Firecracker string `yaml:"firecracker"`
	ChrootBase  string `yaml:"chrootBase"`
	ImageRoot   string `yaml:"imageRoot"`
	// UID and GID are the first of maxConcurrent IDs; each running VM
	// gets its own.
	UID int `yaml:"uid"`
	GID int `yaml:"gid"`
}

// ImageConfig is one bootable image.
type ImageConfig struct {
	Kernel string `yaml:"kernel"`
	RootFS string `yaml:"rootfs"`
}

var (
	targetRe    = regexp.MustCompile(`^(local|server:[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)$`)
	unixNameRe  = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	imageNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,127}$`)
)

// LoadConfig reads, defaults and validates a config file. Like harness
// files, unknown fields are errors.
func LoadConfig(path string) (*Config, error) {
	f, err := os.Open(path) //nolint:gosec // the operator chooses the config file
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("runnerd: %s: %w", path, err)
	}
	if c.Target == "" {
		c.Target = "local"
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = 1
	}
	if c.Socket == "" {
		c.Socket = DefaultSocket
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks the configuration, but not the files it names; see
// CheckFiles.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if !targetRe.MatchString(c.Target) {
		add("target must be \"local\" or \"server:<name>\", got %q", c.Target)
	}
	if c.MaxConcurrent < 1 || c.MaxConcurrent > 64 {
		add("maxConcurrent must be between 1 and 64")
	}
	if !unixNameRe.MatchString(c.RunnerUser) || c.RunnerUser == "root" {
		add("runnerUser must name a non-root user, got %q", c.RunnerUser)
	}
	if c.SocketGroup != "" && (!unixNameRe.MatchString(c.SocketGroup) || c.SocketGroup == "root") {
		add("socketGroup must name a non-root group, got %q", c.SocketGroup)
	}
	if c.Firecracker.Helper != "" && !cleanAbs(c.Firecracker.Helper) {
		add("firecracker.helper must be a clean absolute path, got %q", c.Firecracker.Helper)
	}
	paths := map[string]string{
		"socket": c.Socket, "stateDir": c.StateDir,
		"firecracker.jailer": c.Firecracker.Jailer, "firecracker.firecracker": c.Firecracker.Firecracker,
		"firecracker.chrootBase": c.Firecracker.ChrootBase, "firecracker.imageRoot": c.Firecracker.ImageRoot,
	}
	for name, p := range paths {
		if !cleanAbs(p) {
			add("%s must be a clean absolute path, got %q", name, p)
		}
	}
	if len(c.Images) == 0 {
		add("at least one image is required")
	}
	for name, img := range c.Images {
		if !imageNameRe.MatchString(name) {
			add("invalid image name %q", name)
		}
		for what, p := range map[string]string{"kernel": img.Kernel, "rootfs": img.RootFS} {
			if !cleanAbs(p) || !within(c.Firecracker.ImageRoot, p) {
				add("images.%s.%s must be an absolute path inside firecracker.imageRoot, got %q", name, what, p)
			}
		}
	}
	if c.Firecracker.UID <= 0 || c.Firecracker.GID <= 0 {
		add("firecracker.uid and firecracker.gid must be non-root")
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("runnerd: invalid config: %w", err)
	}
	return nil
}

// CheckFiles verifies the binaries that run as root (through sudo).
// Whoever can modify one can become root, so each must be owned by root
// and writable by nobody else.
func (c *Config) CheckFiles() error {
	var errs []error
	bins := []string{c.Firecracker.Jailer, c.Firecracker.Firecracker}
	if c.Firecracker.Helper != "" {
		bins = append(bins, c.Firecracker.Helper)
	}
	for _, p := range bins {
		if err := checkRootOwned(p); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("runnerd: %w", err)
	}
	return nil
}

func cleanAbs(p string) bool { return filepath.IsAbs(p) && filepath.Clean(p) == p }

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
