package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/first-it-consulting/harness-control-center/internal/runnerd"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox/vz"
)

// Step is one step of the setup, as reported while it runs.
type Step struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// State is "running", "done", "skipped" or "failed".
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
	// Bytes and Total report a download's progress.
	Bytes int64 `json:"bytes,omitempty"`
	Total int64 `json:"total,omitempty"`
}

// Options configure Up.
type Options struct {
	Layout Layout
	// Source of the release's image files (default: this version's
	// GitHub release).
	Source *Source
	// Version is this build's version (version.Version).
	Version string
	// Executable is the hcc binary the service runs (default: this one).
	Executable string
	Service    Service
	// Progress gets every change of a step (optional).
	Progress func(Step)
	// NewMachine defaults to the Virtualization framework.
	NewMachine func(vz.MachineConfig) (vz.Machine, error)

	// For tests.
	supported  func() error
	images     func() ([]File, error)
	claude     *claudePin
	waitRunner func(ctx context.Context, socket string) error
}

// Supported reports whether this computer can run local agents.
func Supported() error {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return errors.New("agents on this computer need a Mac with Apple Silicon; add a Linux server as a runner instead")
	}
	return nil
}

// What each release file becomes, under the layout's image directory.
type target struct {
	path string // relative to Images()
	gz   bool
}

var releaseFiles = map[string]target{
	"vz-kernel-arm64.gz":                 {"kernel/Image", true},
	"vz-base-arm64.ext4.gz":              {"base/rootfs.ext4", true},
	"vz-claude-code-arm64.ext4.gz":       {"claude-code/rootfs.ext4", true},
	"vz-claude-code-tools-arm64.ext4.gz": {"claude-code/tools.ext4", true},
	"vz-claude-code-tools-arm64.txt":     {"claude-code/tools.txt", false},
}

const claudeImageFile = "vz-claude-code-arm64.ext4.gz"

// installed records what is in the image directory, so a second Up only
// fetches what changed.
type installed struct {
	Files      map[string]string `yaml:"files"` // release file -> sha256
	ClaudeCode string            `yaml:"claudeCode,omitempty"`
}

// Up sets up the local runner, step by step, skipping what is already
// done: the VM images, Claude Code, runner.yaml, the launchd agent, and
// waits until the runner answers. It is safe to run again (after an
// upgrade of hcc, it fetches the new release's images).
func Up(ctx context.Context, o Options) error {
	if o.supported == nil {
		o.supported = Supported
	}
	if err := o.supported(); err != nil {
		return err
	}
	if o.claude == nil {
		o.claude = &defaultClaude
	}
	if o.images == nil {
		o.images = ReleaseImages
	}
	if o.waitRunner == nil {
		o.waitRunner = waitRunner
	}
	if o.Executable == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if exe, err = filepath.EvalSymlinks(exe); err != nil {
			return err
		}
		o.Executable = exe
	}
	if o.Source == nil {
		src := ReleaseSource(o.Version)
		o.Source = &src
	}
	report := func(s Step) {
		if o.Progress != nil {
			o.Progress(s)
		}
	}
	step := func(id, title string, fn func(update func(Step)) (string, error)) error {
		report(Step{ID: id, Title: title, State: "running"})
		detail, err := fn(func(s Step) { s.ID, s.Title, s.State = id, title, "running"; report(s) })
		switch {
		case err != nil:
			report(Step{ID: id, Title: title, State: "failed", Detail: err.Error()})
			return fmt.Errorf("%s: %w", title, err)
		case detail == "skip":
			report(Step{ID: id, Title: title, State: "skipped", Detail: "already done"})
		default:
			report(Step{ID: id, Title: title, State: "done", Detail: detail})
		}
		return nil
	}
	l := o.Layout
	for _, d := range []string{l.Dir, l.images()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	if err := step("images", "Download the VM images", func(update func(Step)) (string, error) {
		return o.installImages(ctx, update)
	}); err != nil {
		return err
	}
	if err := step("config", "Write the runner's settings", func(func(Step)) (string, error) {
		created, err := l.writeConfig()
		if err != nil || !created {
			return "skip", err
		}
		return l.Config(), nil
	}); err != nil {
		return err
	}
	return step("service", "Start the runner", func(func(Step)) (string, error) {
		started, err := o.Service.Install(ctx, o.Executable, l, false)
		if err != nil {
			return "", err
		}
		if err := o.waitRunner(ctx, l.Socket()); err != nil {
			return "", fmt.Errorf("%w (its log: %s)", err, l.Log())
		}
		if !started {
			return "skip", nil
		}
		return "it starts again at login", nil
	})
}

// installImages fetches the release's images that are missing or
// changed, and adds Claude Code to the claude-code image. A development
// build (no checksums) uses images built on this Mac.
func (o *Options) installImages(ctx context.Context, update func(Step)) (string, error) {
	l := o.Layout
	files, err := o.images()
	if errors.Is(err, ErrNoImages) {
		for _, p := range []string{l.kernel(), l.baseRootFS(), l.claudeRootFS()} {
			if _, err := os.Stat(p); err != nil {
				return "", fmt.Errorf("%w; build the images with hack/vz/build-images.sh first", ErrNoImages)
			}
		}
		return "skip", nil
	}
	if err != nil {
		return "", err
	}
	have := l.readInstalled()
	next := installed{Files: map[string]string{}, ClaudeCode: have.ClaudeCode}
	fetched := 0
	claudeImage := ""
	for _, f := range files {
		t, ok := releaseFiles[f.Name]
		if !ok {
			continue // not for this version of the setup
		}
		dest := filepath.Join(l.images(), filepath.FromSlash(t.path))
		if have.Files[f.Name] == f.SHA256 && exists(dest) {
			next.Files[f.Name] = f.SHA256
			continue
		}
		if f.Name == claudeImageFile {
			// Completed below, then put in place.
			dest += ".new"
			claudeImage = dest
		}
		if err := o.fetchFile(ctx, f, t, dest, update); err != nil {
			return "", err
		}
		next.Files[f.Name] = f.SHA256
		fetched++
	}
	for name := range releaseFiles {
		if name != "vz-claude-code-tools-arm64.ext4.gz" && name != "vz-claude-code-tools-arm64.txt" && next.Files[name] == "" {
			return "", fmt.Errorf("this release has no %s", name)
		}
	}
	if claudeImage != "" || have.ClaudeCode != o.claude.Version {
		if claudeImage == "" {
			// A new Claude Code for the same image: start over from the
			// release's image, which needs fetching again.
			f := File{Name: claudeImageFile, SHA256: next.Files[claudeImageFile]}
			claudeImage = l.claudeRootFS() + ".new"
			if err := o.fetchFile(ctx, f, releaseFiles[claudeImageFile], claudeImage, update); err != nil {
				return "", err
			}
		}
		if err := o.addClaude(ctx, claudeImage, update); err != nil {
			_ = os.Remove(claudeImage)
			return "", err
		}
		if err := os.Rename(claudeImage, l.claudeRootFS()); err != nil {
			return "", err
		}
		next.ClaudeCode = o.claude.Version
		fetched++
	}
	if err := l.writeInstalled(next); err != nil {
		return "", err
	}
	if fetched == 0 {
		return "skip", nil
	}
	return fmt.Sprintf("version %s, with Claude Code %s", o.Version, o.claude.Version), nil
}

func (o *Options) fetchFile(ctx context.Context, f File, t target, dest string, update func(Step)) error {
	update(Step{Detail: f.Name})
	r, err := o.Source.open(ctx, f.Name)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	if err := fetchTo(r, Check("sha256:"+f.SHA256), dest, t.gz, func(n int64) { update(Step{Detail: f.Name, Bytes: n}) }); err != nil {
		return fmt.Errorf("%s: %w", f.Name, err)
	}
	return nil
}

func (o *Options) addClaude(ctx context.Context, rootfs string, update func(Step)) error {
	l := o.Layout
	if err := os.MkdirAll(l.downloads(), 0o700); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(l.downloads()) }()
	what := "Claude Code " + o.claude.Version
	update(Step{Detail: what})
	pkg, err := fetchClaudeCode(ctx, *o.Source, *o.claude, l.downloads(), func(n int64) {
		update(Step{Detail: what, Bytes: n})
	})
	if err != nil {
		return err
	}
	update(Step{Detail: "adding Claude Code to the image"})
	return installClaude(ctx, o.NewMachine, o.claude.Version, l.kernel(), l.baseRootFS(), pkg, rootfs)
}

func (l Layout) readInstalled() installed {
	var in installed
	data, err := os.ReadFile(l.installed())
	if err == nil {
		_ = yaml.Unmarshal(data, &in) // unreadable: fetch everything again
	}
	return in
}

func (l Layout) writeInstalled(in installed) error {
	data, err := yaml.Marshal(in)
	if err != nil {
		return err
	}
	return writeFileAtomic(l.installed(), data, 0o600)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// waitRunner waits until the runner answers on its socket.
func waitRunner(ctx context.Context, socket string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := runnerd.NewUnixClient(socket)
	for {
		if _, err := c.Info(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("the runner did not answer within 30 seconds")
		case <-time.After(500 * time.Millisecond):
		}
	}
}
