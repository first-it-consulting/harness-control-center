// Package control is the Control Center: a local web UI and JSON API
// that manages harnesses and drives runners (ADR-0007). It holds no
// secrets and starts nothing by itself: every job is started by the user,
// after seeing what the harness may do, and every runner enforces its own
// limits whatever the Control Center asks.
package control

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"go.yaml.in/yaml/v3"

	"github.com/first-it-consulting/harness-control-center/internal/runnerd"
)

// nameRe is the syntax of runner and harness names (as metadata.name).
var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// MaxRunners bounds the configured runners.
const MaxRunners = 64

// RunnerRef says how to reach one runner: its local socket, or a host
// reached through the user's ssh.
type RunnerRef struct {
	Name   string `yaml:"name" json:"name"`
	SSH    string `yaml:"ssh,omitempty" json:"ssh,omitempty"`
	Socket string `yaml:"socket,omitempty" json:"socket,omitempty"`
	// RemoteSocket is the runner's socket on the ssh host (default
	// runnerd.DefaultSocket).
	RemoteSocket string `yaml:"remoteSocket,omitempty" json:"remoteSocket,omitempty"`
}

// Validate checks r, including that a client could be built for it.
func (r RunnerRef) Validate() error {
	if !nameRe.MatchString(r.Name) {
		return fmt.Errorf("runner name %q must be 1-63 lowercase letters, digits or '-'", r.Name)
	}
	if (r.SSH == "") == (r.Socket == "") {
		return fmt.Errorf("runner %s: set exactly one of ssh and socket", r.Name)
	}
	if r.Socket != "" && (!filepath.IsAbs(r.Socket) || filepath.Clean(r.Socket) != r.Socket) {
		return fmt.Errorf("runner %s: socket must be a clean absolute path", r.Name)
	}
	if r.Socket != "" && r.RemoteSocket != "" {
		return fmt.Errorf("runner %s: remoteSocket is only for ssh", r.Name)
	}
	_, err := r.client()
	return err
}

func (r RunnerRef) client() (*runnerd.Client, error) {
	if r.SSH != "" {
		remote := r.RemoteSocket
		if remote == "" {
			remote = runnerd.DefaultSocket
		}
		return runnerd.NewSSHClient(r.SSH, remote)
	}
	return runnerd.NewUnixClient(r.Socket), nil
}

// Config is the Control Center's configuration file (control.yaml).
type Config struct {
	Runners []RunnerRef `yaml:"runners"`
}

// Validate checks every runner and that names are unique.
func (c *Config) Validate() error {
	if len(c.Runners) > MaxRunners {
		return fmt.Errorf("at most %d runners", MaxRunners)
	}
	var errs []error
	seen := map[string]bool{}
	for _, r := range c.Runners {
		if err := r.Validate(); err != nil {
			errs = append(errs, err)
		}
		if seen[r.Name] {
			errs = append(errs, fmt.Errorf("runner %s is listed twice", r.Name))
		}
		seen[r.Name] = true
	}
	return errors.Join(errs...)
}

// Runner returns the runner called name.
func (c *Config) Runner(name string) (RunnerRef, bool) {
	i := slices.IndexFunc(c.Runners, func(r RunnerRef) bool { return r.Name == name })
	if i < 0 {
		return RunnerRef{}, false
	}
	return c.Runners[i], true
}

// LocalRunnerName is what the Control Center calls the runner on this Mac.
const LocalRunnerName = "local"

// WithLocalRunner returns c with the local runner (on socket) added,
// unless a runner already uses that socket. It fails if the name is taken
// by another runner. added says whether c changed.
func (c *Config) WithLocalRunner(socket string) (next *Config, added bool, err error) {
	for _, r := range c.Runners {
		if r.Socket == socket {
			return c, false, nil
		}
	}
	if _, taken := c.Runner(LocalRunnerName); taken {
		return c, false, fmt.Errorf("a runner called %s exists already; rename it to add this Mac's runner", LocalRunnerName)
	}
	next = &Config{Runners: append(append([]RunnerRef(nil), c.Runners...), RunnerRef{Name: LocalRunnerName, Socket: socket})}
	if err := next.Validate(); err != nil {
		return c, false, err
	}
	return next, true, nil
}

// DefaultDir is where the Control Center keeps its files:
// <user config dir>/hcc (e.g. ~/.config/hcc).
func DefaultDir() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "hcc"), nil
}

// LoadConfig reads a config file; a missing one is an empty config.
// Unknown fields are errors, as in harness files.
func LoadConfig(path string) (*Config, error) {
	f, err := os.Open(path) //nolint:gosec // the user's own config file
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return nil, err
	}
	var c Config
	if len(bytes.TrimSpace(data)) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// SaveConfig validates c and writes it atomically, readable by the user
// only.
func SaveConfig(path string, c *Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

// writeFileAtomic writes data to path through a temporary file in the
// same directory (created owner-only), so readers never see a partial
// file.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}
