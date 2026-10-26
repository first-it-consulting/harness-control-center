// Package tools reads a runner's tool catalog: the packages an image can
// have installed before its agent starts (ADR-0010).
//
// The catalog is built on the runner at install time, as root, from
// signed Alpine packages. Its package list ("packages.txt") names, per
// offered package, its version and the package files of it and of every
// dependency the image does not have yet:
//
//	# name version file...
//	python3 3.12.8-r1 python3-3.12.8-r1.apk libffi-3.4.6-r0.apk
//
// The files themselves are on a read-only drive that only the guest's
// init reads; apk verifies every file's signature there.
package tools

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

// Limits on a catalog's package list.
const (
	MaxListBytes       = 1 << 20
	MaxPackages        = 1024
	MaxFilesPerPackage = 512
	// MaxInstall bounds the files one job installs.
	MaxInstall = 2048
)

var (
	nameRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9._+-]{0,127}$`)
	versionRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	// fileRe: a plain file name in the catalog's top directory, nothing
	// an installer or a shell could read as an option or a path.
	fileRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,250}\.apk$`)
)

// Package is one offered package.
type Package struct {
	Name    string
	Version string
	// Files are the package files to install: the package's own and those
	// of the dependencies the image lacks.
	Files []string
}

// Catalog is an image's offered packages, by name.
type Catalog struct {
	pkgs map[string]Package
}

// Parse reads a package list. Anything malformed is an error: the list
// is root's, so a bad one means a broken install, not a reason to guess.
func Parse(r io.Reader) (*Catalog, error) {
	c := &Catalog{pkgs: map[string]Package{}}
	sc := bufio.NewScanner(io.LimitReader(r, MaxListBytes+1))
	sc.Buffer(make([]byte, 0, 64<<10), MaxListBytes+1)
	n, read := 0, 0
	for sc.Scan() {
		n++
		read += len(sc.Bytes()) + 1
		if read > MaxListBytes {
			return nil, fmt.Errorf("tools: package list exceeds %d bytes", MaxListBytes)
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 3 {
			return nil, fmt.Errorf("tools: line %d: expected a name, a version and at least one file", n)
		}
		p := Package{Name: f[0], Version: f[1], Files: f[2:]}
		switch {
		case !nameRe.MatchString(p.Name):
			return nil, fmt.Errorf("tools: line %d: invalid package name %q", n, p.Name)
		case !versionRe.MatchString(p.Version):
			return nil, fmt.Errorf("tools: line %d: invalid version %q", n, p.Version)
		case len(p.Files) > MaxFilesPerPackage:
			return nil, fmt.Errorf("tools: line %d: more than %d files", n, MaxFilesPerPackage)
		}
		if _, dup := c.pkgs[p.Name]; dup {
			return nil, fmt.Errorf("tools: line %d: package %s listed twice", n, p.Name)
		}
		for _, file := range p.Files {
			if !fileRe.MatchString(file) {
				return nil, fmt.Errorf("tools: line %d: invalid file name %q", n, file)
			}
		}
		if len(c.pkgs) == MaxPackages {
			return nil, fmt.Errorf("tools: more than %d packages", MaxPackages)
		}
		c.pkgs[p.Name] = p
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, fmt.Errorf("tools: package list exceeds %d bytes", MaxListBytes)
		}
		return nil, fmt.Errorf("tools: %w", err)
	}
	return c, nil
}

// Packages lists the offered packages as "name@version", sorted.
func (c *Catalog) Packages() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.pkgs))
	for _, p := range c.pkgs {
		out = append(out, p.Name+"@"+p.Version)
	}
	slices.Sort(out)
	return out
}

// Resolve returns the packages that grants (spec.permissions.tools.install
// entries: "name" or "name@version") name, and the files to install for
// them, each once, sorted. A grant the catalog does not offer, or offers
// only in another version, is an error: the job would run without a tool
// the user saw it get.
func (c *Catalog) Resolve(grants []string) (pkgs, files []string, err error) {
	var errs []error
	seen := map[string]bool{}
	for _, g := range grants {
		if !spec.ValidPackage(g) {
			errs = append(errs, fmt.Errorf("invalid package reference %q", g))
			continue
		}
		name, version, pinned := strings.Cut(g, "@")
		var p Package
		ok := false
		if c != nil {
			p, ok = c.pkgs[name]
		}
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("tool %s is not offered by this runner's image", name))
			continue
		case pinned && version != p.Version:
			errs = append(errs, fmt.Errorf("tool %s is offered in version %s, not %s", name, p.Version, version))
			continue
		}
		pkgs = append(pkgs, p.Name)
		for _, f := range p.Files {
			if !seen[f] {
				seen[f] = true
				files = append(files, f)
			}
		}
	}
	if len(files) > MaxInstall {
		errs = append(errs, fmt.Errorf("these tools need %d package files; the limit is %d", len(files), MaxInstall))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, nil, err
	}
	slices.Sort(pkgs)
	pkgs = slices.Compact(pkgs)
	slices.Sort(files)
	return pkgs, files, nil
}
