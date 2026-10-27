package local

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"

	"go.yaml.in/yaml/v3"

	"github.com/first-it-consulting/harness-control-center/internal/runnerd"
)

// Target is the local runner's name in spec.target.
const Target = "local"

// Layout is where the local runner keeps everything: one owner-only
// directory of the user's (~/Library/Application Support/hcc/local).
type Layout struct{ Dir string }

// DefaultLayout is the user's own layout.
func DefaultLayout() (Layout, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return Layout{}, err
	}
	return Layout{Dir: filepath.Join(d, "hcc", "local")}, nil
}

func (l Layout) path(p ...string) string { return filepath.Join(append([]string{l.Dir}, p...)...) }

// Config is the runner's runner.yaml.
func (l Layout) Config() string { return l.path("runner.yaml") }

// Socket is the runner's socket.
func (l Layout) Socket() string { return l.path("runner.sock") }

// Log is the runner's log, written by launchd.
func (l Layout) Log() string { return l.path("runner.log") }

func (l Layout) images() string       { return l.path("images") }
func (l Layout) kernel() string       { return l.path("images", "kernel", "Image") }
func (l Layout) baseRootFS() string   { return l.path("images", "base", "rootfs.ext4") }
func (l Layout) claudeRootFS() string { return l.path("images", "claude-code", "rootfs.ext4") }
func (l Layout) tools() string        { return l.path("images", "claude-code", "tools.ext4") }
func (l Layout) toolsList() string    { return l.path("images", "claude-code", "tools.txt") }
func (l Layout) downloads() string    { return l.path("downloads") }
func (l Layout) installed() string    { return l.path("images", "installed.yaml") }
func (l Layout) credentials() string  { return l.path("credentials.yaml") }

// localConfig is the part of runner.yaml (runnerd.Config) the local
// runner sets.
type localConfig struct {
	Target        string                `yaml:"target"`
	MaxConcurrent int                   `yaml:"maxConcurrent"`
	RunnerUser    string                `yaml:"runnerUser"`
	Socket        string                `yaml:"socket"`
	StateDir      string                `yaml:"stateDir"`
	Provider      string                `yaml:"provider"`
	VZ            runnerd.VZConfig      `yaml:"vz"`
	Images        map[string]localImage `yaml:"images"`
	Network       localNetwork          `yaml:"network"`
	Credentials   string                `yaml:"credentials"`
}

type localNetwork struct {
	ProxyPort int `yaml:"proxyPort"`
}

type localImage struct {
	Kernel string               `yaml:"kernel"`
	RootFS string               `yaml:"rootfs"`
	Tools  *runnerd.ToolsConfig `yaml:"tools,omitempty"`
}

// runnerConfig is the local runner's runner.yaml: provider vz, this
// user, everything inside the layout, and an empty credential store the
// Control Center fills (ADR-0017).
func (l Layout) runnerConfig() (*localConfig, error) {
	u, err := user.Current()
	if err != nil {
		return nil, err
	}
	images := map[string]localImage{
		"base":        {Kernel: l.kernel(), RootFS: l.baseRootFS()},
		"claude-code": {Kernel: l.kernel(), RootFS: l.claudeRootFS()},
	}
	if _, err := os.Stat(l.toolsList()); err == nil {
		cc := images["claude-code"]
		cc.Tools = &runnerd.ToolsConfig{Drive: l.tools(), Packages: l.toolsList()}
		images["claude-code"] = cc
	}
	return &localConfig{
		Target:        Target,
		MaxConcurrent: 2,
		RunnerUser:    u.Username,
		Socket:        l.Socket(),
		StateDir:      l.path("state"),
		Provider:      "vz",
		VZ:            runnerd.VZConfig{StagingDir: l.path("staging"), ImageRoot: l.images()},
		Images:        images,
		Network:       localNetwork{ProxyPort: 3128},
		Credentials:   l.credentials(),
	}, nil
}

const configHeader = `# The local runner on this Mac (ADR-0016): VMs of Apple's Virtualization
# framework, run by this user; nothing runs as root. Written by
# "hcc local up"; change it in the Control Center.
`

// writeConfig writes runner.yaml unless one exists (the user's settings
// win), then checks it as the runner will.
func (l Layout) writeConfig() (created bool, err error) {
	if _, err := os.Stat(l.Config()); errors.Is(err, os.ErrNotExist) {
		c, err := l.runnerConfig()
		if err != nil {
			return false, err
		}
		data, err := yaml.Marshal(c)
		if err != nil {
			return false, err
		}
		if err := writeExclusive(l.Config(), append([]byte(configHeader), data...)); err != nil {
			return false, err
		}
		if err := writeExclusive(l.credentials(), []byte("{}\n")); err != nil && !errors.Is(err, os.ErrExist) {
			return false, err
		}
		created = true
	} else if err != nil {
		return false, err
	}
	c, err := runnerd.LoadConfig(l.Config())
	if err != nil {
		return created, fmt.Errorf("%s: %w", l.Config(), err)
	}
	if err := c.CheckFiles(); err != nil {
		return created, fmt.Errorf("%s: %w", l.Config(), err)
	}
	return created, nil
}

func writeExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // our own directory
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if err := errors.Join(werr, f.Close()); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}
