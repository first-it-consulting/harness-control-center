// Package plugins installs plugins on a runner and runs them, each in a
// sandbox of its own (ADR-0013).
//
// A plugin is a directory: its manifest (plugin.yaml, see spec.Plugin)
// and its files. Installing re-packs it into a clean archive (plain files
// and directories, safe names; anything else refuses the install), shows
// what it may do and installs exactly the archive whose checksum the
// operator confirmed. Plugins are never upgraded in place: remove, then
// install.
package plugins

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/policy"
	"github.com/first-it-consulting/harness-control-center/internal/results"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

// Limits on a plugin bundle.
const (
	MaxBundleBytes = 32 << 20
	MaxBundleFiles = 4096
	// MaxPlugins bounds the plugins on one runner.
	MaxPlugins = 32
)

// ManifestName is the manifest's name at the bundle's root.
const ManifestName = "plugin.yaml"

// Errors.
var (
	ErrNotFound  = errors.New("plugins: no such plugin")
	ErrMismatch  = errors.New("plugins: the bundle is not the one confirmed (checksum differs); review it again")
	ErrInstalled = errors.New("plugins: a plugin of that name is installed; remove it first")
)

// Preview is what an operator confirms before installing.
type Preview struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	SHA256      string   `json:"sha256"`
	Files       int      `json:"files"`
	Bytes       int64    `json:"bytes"`
	Risk        string   `json:"risk"`
	Why         []string `json:"why,omitempty"`
	Explanation []string `json:"explanation"`
}

// Installed is an installed plugin.
type Installed struct {
	Preview
	Installed time.Time `json:"installed"`
}

// Store keeps installed plugins in a directory of their own (owner-only).
type Store struct {
	dir string
	now func() time.Time
}

// NewStore returns a store in dir (created on first install).
func NewStore(dir string) *Store { return &Store{dir: dir, now: time.Now} }

// Check sanitizes a bundle (a tar archive) and returns the clean archive,
// its manifest and what installing it means.
func Check(bundle io.Reader) ([]byte, *spec.Plugin, Preview, error) {
	var clean bytes.Buffer
	sum, err := results.Sanitize(&clean, io.LimitReader(bundle, MaxBundleBytes+1<<20), results.Limits{MaxBytes: MaxBundleBytes, MaxFiles: MaxBundleFiles})
	if err != nil {
		return nil, nil, Preview{}, fmt.Errorf("plugins: %w", err)
	}
	if len(sum.Skipped) > 0 {
		return nil, nil, Preview{}, fmt.Errorf("plugins: the bundle may only hold plain files and directories with safe names: %s", strings.Join(sum.Skipped, "; "))
	}
	files, err := readTar(clean.Bytes())
	if err != nil {
		return nil, nil, Preview{}, err
	}
	m, ok := files[ManifestName]
	if !ok || m.dir {
		return nil, nil, Preview{}, fmt.Errorf("plugins: no %s at the top of the bundle", ManifestName)
	}
	p, err := spec.ParsePlugin(m.data)
	if err != nil {
		return nil, nil, Preview{}, fmt.Errorf("plugins: %s: %w", ManifestName, err)
	}
	if prog := p.Spec.Command[0]; strings.Contains(prog, "/") {
		f, ok := files[path.Clean(prog)]
		if !ok || f.dir || !f.exec {
			return nil, nil, Preview{}, fmt.Errorf("plugins: the command %s is not an executable file of the bundle", prog)
		}
	}
	h := sha256.Sum256(clean.Bytes())
	risk, why, lines := policy.ExplainPlugin(p)
	return clean.Bytes(), p, Preview{
		Name: p.Metadata.Name, Version: p.Metadata.Version, Type: p.Spec.Type, Description: p.Metadata.Description,
		SHA256: hex.EncodeToString(h[:]), Files: sum.Files, Bytes: sum.Bytes,
		Risk: risk.String(), Why: why, Explanation: lines,
	}, nil
}

// Install installs a bundle whose Preview.SHA256 the operator confirmed.
func (s *Store) Install(bundle io.Reader, confirmed string) (Installed, error) {
	clean, p, pv, err := Check(bundle)
	if err != nil {
		return Installed{}, err
	}
	if confirmed != pv.SHA256 {
		return Installed{}, ErrMismatch
	}
	list, err := s.List()
	if err != nil {
		return Installed{}, err
	}
	if slices.ContainsFunc(list, func(i Installed) bool { return i.Name == p.Metadata.Name }) {
		return Installed{}, ErrInstalled
	}
	if len(list) >= MaxPlugins {
		return Installed{}, fmt.Errorf("plugins: at most %d plugins", MaxPlugins)
	}
	in := Installed{Preview: pv, Installed: s.now().UTC()}
	rec, err := json.Marshal(in)
	if err != nil {
		return Installed{}, err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return Installed{}, err
	}
	// Written to a temporary directory and renamed: a plugin is installed
	// completely or not at all.
	tmp, err := os.MkdirTemp(s.dir, ".install-*")
	if err != nil {
		return Installed{}, err
	}
	defer func() { _ = os.RemoveAll(tmp) }() // gone after the rename
	if err := os.WriteFile(filepath.Join(tmp, "bundle.tar"), clean, 0o600); err != nil {
		return Installed{}, err
	}
	if err := os.WriteFile(filepath.Join(tmp, "plugin.json"), rec, 0o600); err != nil {
		return Installed{}, err
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, p.Metadata.Name)); err != nil {
		return Installed{}, fmt.Errorf("plugins: %w", err)
	}
	return in, nil
}

// List returns the installed plugins, by name.
func (s *Store) List() ([]Installed, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Installed
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		in, _, _, err := s.Get(e.Name())
		if err != nil {
			continue // a broken install is skipped (and can be removed)
		}
		out = append(out, in)
	}
	return out, nil
}

// Get returns an installed plugin, its manifest and its bundle, checked
// against the checksum recorded at install.
func (s *Store) Get(name string) (Installed, *spec.Plugin, []byte, error) {
	dir, err := s.pluginDir(name)
	if err != nil {
		return Installed{}, nil, nil, err
	}
	rec, err := os.ReadFile(filepath.Join(dir, "plugin.json")) //nolint:gosec // validated name
	if errors.Is(err, os.ErrNotExist) {
		return Installed{}, nil, nil, ErrNotFound
	}
	if err != nil {
		return Installed{}, nil, nil, err
	}
	var in Installed
	if err := json.Unmarshal(rec, &in); err != nil {
		return Installed{}, nil, nil, fmt.Errorf("plugins: %s: %w", name, err)
	}
	bundle, err := os.ReadFile(filepath.Join(dir, "bundle.tar")) //nolint:gosec // validated name
	if err != nil {
		return Installed{}, nil, nil, err
	}
	h := sha256.Sum256(bundle)
	if hex.EncodeToString(h[:]) != in.SHA256 {
		return Installed{}, nil, nil, fmt.Errorf("plugins: %s: the bundle changed since it was installed", name)
	}
	_, p, pv, err := Check(bytes.NewReader(bundle))
	if err != nil {
		return Installed{}, nil, nil, err
	}
	// Explanations follow the current code; what was installed does not
	// change.
	in.Risk, in.Why, in.Explanation = pv.Risk, pv.Why, pv.Explanation
	return in, p, bundle, nil
}

// Remove uninstalls a plugin.
func (s *Store) Remove(name string) error {
	dir, err := s.pluginDir(name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "plugin.json")); errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	return os.RemoveAll(dir)
}

func (s *Store) pluginDir(name string) (string, error) {
	if !spec.ValidName(name) {
		return "", fmt.Errorf("plugins: invalid name %q", name)
	}
	return filepath.Join(s.dir, name), nil
}

type tarFile struct {
	data      []byte
	dir, exec bool
}

// readTar reads a (clean) bundle into memory.
func readTar(b []byte) (map[string]tarFile, error) {
	out := map[string]tarFile{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("plugins: %w", err)
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		f := tarFile{dir: hdr.Typeflag == tar.TypeDir, exec: hdr.Mode&0o111 != 0}
		if !f.dir && name == ManifestName {
			if f.data, err = io.ReadAll(io.LimitReader(tr, spec.MaxDocumentBytes+1)); err != nil {
				return nil, err
			}
		}
		out[name] = f
	}
}
