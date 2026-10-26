// Package runnerd is the runner daemon: it exposes a runner.Runner over
// HTTP on a Unix socket that only the runner's user can open (ADR-0003),
// and provides the matching client.
package runnerd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/first-it-consulting/harness-control-center/internal/budget"
	"github.com/first-it-consulting/harness-control-center/internal/runner"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox/firecracker"
	"github.com/first-it-consulting/harness-control-center/internal/tools"
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
	// Network enables network access for harnesses that are granted it;
	// without it, such harnesses are rejected.
	Network *NetworkSection `yaml:"network"`
	// Credentials is the credential broker's store (see
	// internal/credentials). Root-owned, readable by the runner's group
	// only; re-read for every job.
	Credentials string `yaml:"credentials"`
	// Pricing gives models a price in USD per million tokens, so that
	// budget.maxCostUSD can be enforced for them.
	Pricing map[string]budget.Price `yaml:"pricing"`
	// MaxDiskMiB bounds a harness's spec.sandbox.diskMiB: each job's disk
	// is reserved on the jail filesystem, and its results are stored in
	// stateDir.
	MaxDiskMiB int `yaml:"maxDiskMiB"`
	// Retention bounds the finished jobs kept in stateDir (record,
	// console log and results each).
	Retention Retention `yaml:"retention"`
	// MinScheduleInterval bounds how often a cron schedule may run (a Go
	// duration, default "15m", at least "1m").
	MinScheduleInterval string `yaml:"minScheduleInterval"`
	// ApprovalTimeout bounds how long a gated request waits for a human
	// (a Go duration, default "30m", between "1m" and "24h").
	ApprovalTimeout string `yaml:"approvalTimeout"`
	// ContactTimeout pauses every schedule when nobody contacted the
	// runner for that long (a Go duration between "1h" and "8760h",
	// default "72h"; "off" never pauses).
	ContactTimeout string `yaml:"contactTimeout"`
	// MaxPluginVMs bounds the plugin sandboxes running at once
	// (ADR-0013; default 2, 0..64). They have VM identities of their own,
	// so plugins never take a job's slot.
	MaxPluginVMs *int `yaml:"maxPluginVMs"`
}

// PluginVMs is MaxPluginVMs with its default.
func (c *Config) PluginVMs() int {
	if c.MaxPluginVMs == nil {
		return 2
	}
	return *c.MaxPluginVMs
}

// VMSlots is the number of VM identities (and sandbox links): one per
// job that may run at once and one per plugin sandbox.
func (c *Config) VMSlots() int { return c.MaxConcurrent + c.PluginVMs() }

// ContactWait is ContactTimeout parsed (valid after LoadConfig; 0 for
// "off").
func (c *Config) ContactWait() time.Duration {
	d, _ := time.ParseDuration(c.ContactTimeout)
	return d
}

// ApprovalWait is ApprovalTimeout parsed (valid after LoadConfig).
func (c *Config) ApprovalWait() time.Duration {
	d, _ := time.ParseDuration(c.ApprovalTimeout)
	return d
}

// ScheduleInterval is MinScheduleInterval parsed (valid after
// LoadConfig).
func (c *Config) ScheduleInterval() time.Duration {
	d, _ := time.ParseDuration(c.MinScheduleInterval)
	return d
}

// Retention defaults.
const (
	DefaultRetentionJobs = 1000
	DefaultRetentionAge  = 30 * 24 * time.Hour
)

// Retention says how many finished jobs to keep, and for how long.
type Retention struct {
	MaxJobs int `yaml:"maxJobs"`
	// MaxAge is a Go duration such as "720h".
	MaxAge string `yaml:"maxAge"`
}

// Age is MaxAge parsed (valid after LoadConfig).
func (r Retention) Age() time.Duration {
	d, _ := time.ParseDuration(r.MaxAge)
	return d
}

// NetworkSection configures sandbox links and the egress proxy.
type NetworkSection struct {
	// Subnet is an IPv4 range reserved for sandbox links (one /30 each).
	Subnet    string `yaml:"subnet"`
	ProxyPort int    `yaml:"proxyPort"`
}

// NetworkConfig returns the sandbox network configuration, or nil when
// networking is not configured.
func (c *Config) NetworkConfig() (*firecracker.NetworkConfig, error) {
	if c.Network == nil {
		return nil, nil
	}
	subnet, err := netip.ParsePrefix(c.Network.Subnet)
	if err != nil {
		return nil, fmt.Errorf("runnerd: network.subnet: %w", err)
	}
	nc := &firecracker.NetworkConfig{Subnet: subnet, ProxyPort: c.Network.ProxyPort}
	if nc.ProxyPort == 0 {
		nc.ProxyPort = 3128
	}
	if err := nc.Validate(c.VMSlots()); err != nil {
		return nil, fmt.Errorf("runnerd: %w", err)
	}
	return nc, nil
}

// FirecrackerConfig configures the Firecracker sandbox provider.
type FirecrackerConfig struct {
	// Helper is the root-owned hcc-helper the runner calls through sudo.
	Helper      string `yaml:"helper"`
	Jailer      string `yaml:"jailer"`
	Firecracker string `yaml:"firecracker"`
	// ChrootBase holds the jails. It must be owned by root and writable
	// only by root, up to "/": hcc-helper creates every jail itself.
	ChrootBase string `yaml:"chrootBase"`
	// StagingDir is the runner's own directory where it prepares VM files
	// for hcc-helper to copy into a jail.
	StagingDir string `yaml:"stagingDir"`
	ImageRoot  string `yaml:"imageRoot"`
	// UID and GID are the first of maxConcurrent IDs; each running VM
	// gets its own.
	UID int `yaml:"uid"`
	GID int `yaml:"gid"`
}

// Catalogs loads the images' tool catalogs, by image name.
func (c *Config) Catalogs() (map[string]*tools.Catalog, error) {
	out := map[string]*tools.Catalog{}
	for name, img := range c.Images {
		if img.Tools == nil {
			continue
		}
		f, err := os.Open(img.Tools.Packages)
		if err != nil {
			return nil, fmt.Errorf("runnerd: image %s: %w", name, err)
		}
		cat, err := tools.Parse(f)
		_ = f.Close() // read-only
		if err != nil {
			return nil, fmt.Errorf("runnerd: image %s: %w", name, err)
		}
		out[name] = cat
	}
	return out, nil
}

// ImageConfig is one bootable image.
type ImageConfig struct {
	Kernel string `yaml:"kernel"`
	RootFS string `yaml:"rootfs"`
	// Tools, if set, is the image's tool catalog (ADR-0010).
	Tools *ToolsConfig `yaml:"tools"`
}

// ToolsConfig is an image's tool catalog, built by install-runner.sh.
type ToolsConfig struct {
	// Drive is the read-only ext4 image holding the package files.
	Drive string `yaml:"drive"`
	// Packages is the package list (see internal/tools).
	Packages string `yaml:"packages"`
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
	if c.MaxDiskMiB == 0 {
		c.MaxDiskMiB = runner.DefaultMaxDiskMiB
	}
	if c.Retention.MaxJobs == 0 {
		c.Retention.MaxJobs = DefaultRetentionJobs
	}
	if c.Retention.MaxAge == "" {
		c.Retention.MaxAge = DefaultRetentionAge.String()
	}
	if c.ApprovalTimeout == "" {
		c.ApprovalTimeout = runner.DefaultApprovalTimeout.String()
	}
	if c.MinScheduleInterval == "" {
		c.MinScheduleInterval = runner.DefaultMinScheduleInterval.String()
	}
	if c.ContactTimeout == "" {
		c.ContactTimeout = runner.DefaultContactTimeout.String()
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
	if within(c.Firecracker.StagingDir, c.Firecracker.ChrootBase) || within(c.StateDir, c.Firecracker.ChrootBase) {
		add("firecracker.chrootBase must not be inside a runner-owned directory")
	}
	paths := map[string]string{
		"socket": c.Socket, "stateDir": c.StateDir,
		"firecracker.jailer": c.Firecracker.Jailer, "firecracker.firecracker": c.Firecracker.Firecracker,
		"firecracker.chrootBase": c.Firecracker.ChrootBase, "firecracker.imageRoot": c.Firecracker.ImageRoot,
		"firecracker.helper": c.Firecracker.Helper, "firecracker.stagingDir": c.Firecracker.StagingDir,
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
		files := map[string]string{"kernel": img.Kernel, "rootfs": img.RootFS}
		if img.Tools != nil {
			files["tools.drive"], files["tools.packages"] = img.Tools.Drive, img.Tools.Packages
		}
		for what, p := range files {
			if !cleanAbs(p) || !within(c.Firecracker.ImageRoot, p) {
				add("images.%s.%s must be an absolute path inside firecracker.imageRoot, got %q", name, what, p)
			}
		}
	}
	if c.Retention.MaxJobs < 1 || c.Retention.MaxJobs > 1_000_000 {
		add("retention.maxJobs must be between 1 and 1000000")
	}
	if d, err := time.ParseDuration(c.Retention.MaxAge); err != nil || d < time.Hour || d > 10*365*24*time.Hour {
		add("retention.maxAge must be a duration between 1h and 87600h, got %q", c.Retention.MaxAge)
	}
	if d, err := time.ParseDuration(c.ApprovalTimeout); err != nil || d < time.Minute || d > 24*time.Hour {
		add("approvalTimeout must be a duration between 1m and 24h, got %q", c.ApprovalTimeout)
	}
	if n := c.PluginVMs(); n < 0 || n > 64 {
		add("maxPluginVMs must be between 0 and 64")
	}
	if d, err := time.ParseDuration(c.ContactTimeout); c.ContactTimeout != "off" && (err != nil || d < time.Hour || d > 8760*time.Hour) {
		add("contactTimeout must be \"off\" or a duration between 1h and 8760h, got %q", c.ContactTimeout)
	}
	if d, err := time.ParseDuration(c.MinScheduleInterval); err != nil || d < time.Minute || d > 24*time.Hour {
		add("minScheduleInterval must be a duration between 1m and 24h, got %q", c.MinScheduleInterval)
	}
	if c.MaxDiskMiB < 1 || c.MaxDiskMiB > sandbox.MaxDiskMiB {
		add("maxDiskMiB must be between 1 and %d", sandbox.MaxDiskMiB)
	}
	if c.Firecracker.UID <= 0 || c.Firecracker.GID <= 0 {
		add("firecracker.uid and firecracker.gid must be non-root")
	}
	if _, err := c.NetworkConfig(); err != nil {
		errs = append(errs, err)
	}
	for model, p := range c.Pricing {
		if p.InputPerMTok < 0 || p.OutputPerMTok < 0 || p.InputPerMTok > 1e4 || p.OutputPerMTok > 1e4 {
			add("pricing for %q must be between 0 and 10000 USD per million tokens", model)
		}
	}
	if c.Credentials != "" {
		if !cleanAbs(c.Credentials) {
			add("credentials must be a clean absolute path, got %q", c.Credentials)
		}
		if c.Network == nil {
			add("credentials need a network section (they travel through the egress proxy)")
		}
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
	for _, p := range []string{c.Firecracker.Helper, c.Firecracker.Jailer, c.Firecracker.Firecracker} {
		if err := checkRootOwned(p); err != nil {
			errs = append(errs, err)
		}
	}
	// What an image offers as tools is root's decision.
	for _, img := range c.Images {
		if img.Tools != nil {
			for _, p := range []string{img.Tools.Drive, img.Tools.Packages} {
				if err := checkRootOwned(p); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	// The runner must not be able to change where secrets go, and nobody
	// else may read them.
	if c.Credentials != "" {
		if err := checkRootOwned(c.Credentials); err != nil {
			errs = append(errs, err)
		} else if err := checkNotWorldReadable(c.Credentials); err != nil {
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
