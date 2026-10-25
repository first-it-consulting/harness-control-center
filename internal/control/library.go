package control

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/first-it-consulting/harness-control-center/internal/policy"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

// MaxHarnesses bounds the library.
const MaxHarnesses = 1000

// ErrNoHarness is returned for a harness that is not in the library.
var ErrNoHarness = errors.New("no such harness")

// Library is the user's saved harnesses: <dir>/<name>.yaml, one valid
// harness document each, named after its metadata.name.
type Library struct {
	dir string
}

// NewLibrary returns the library in dir.
func NewLibrary(dir string) *Library { return &Library{dir: dir} }

// Entry summarizes one saved harness.
type Entry struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Runtime     string `json:"runtime,omitempty"`
	Target      string `json:"target,omitempty"`
	Risk        string `json:"risk,omitempty"`
	// Error says why a file in the library is not a valid harness (it was
	// edited outside the Control Center).
	Error string `json:"error,omitempty"`
}

func (l *Library) path(name string) (string, error) {
	if !nameRe.MatchString(name) {
		return "", fmt.Errorf("invalid harness name %q", name)
	}
	return filepath.Join(l.dir, name+".yaml"), nil
}

// List returns every saved harness, by name.
func (l *Library) List() ([]Entry, error) {
	files, err := os.ReadDir(l.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, f := range files {
		name, ok := strings.CutSuffix(f.Name(), ".yaml")
		if !ok || !f.Type().IsRegular() || !nameRe.MatchString(name) {
			continue
		}
		e := Entry{Name: name}
		data, err := l.Get(name)
		var h *spec.Harness
		if err == nil {
			h, err = spec.Parse(data)
		}
		if err == nil {
			e.Description, e.Runtime, e.Target = h.Metadata.Description, h.Spec.Runtime, h.Spec.Target
			if eng, perr := policy.New(h); perr == nil {
				risk, _ := eng.Risk()
				e.Risk = risk.String()
			}
		} else {
			e.Error = err.Error()
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Get returns a saved harness document.
func (l *Library) Get(name string) ([]byte, error) {
	p, err := l.path(name)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p) //nolint:gosec // path built from a validated name
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoHarness
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	return io.ReadAll(io.LimitReader(f, spec.MaxDocumentBytes+1))
}

// Put saves a harness document under its metadata.name. Only valid
// harnesses are saved, exactly as written (comments included).
func (l *Library) Put(data []byte) (string, error) {
	h, err := spec.Parse(data)
	if err != nil {
		return "", err
	}
	p, err := l.path(h.Metadata.Name)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		if entries, _ := os.ReadDir(l.dir); len(entries) >= MaxHarnesses {
			return "", fmt.Errorf("the library holds at most %d harnesses", MaxHarnesses)
		}
	}
	return h.Metadata.Name, writeFileAtomic(p, data)
}

// Delete removes a saved harness.
func (l *Library) Delete(name string) error {
	p, err := l.path(name)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNoHarness
	}
	return err
}
