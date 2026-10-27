package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/safetext"
)

// Runner installation from the web UI. This first implementation uses the
// same reviewed source-tree installer as "make deploy-runner"; it is not a
// second installation mechanism to keep in lockstep.
const (
	runnerInstallTimeout = 45 * time.Minute
	maxInstallLogLines   = 300
)

var runnerTargetRe = regexp.MustCompile(`^server:[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

type runnerInstallState struct {
	Target   string   `json:"target"`
	Running  bool     `json:"running"`
	Finished bool     `json:"finished"`
	Log      []string `json:"log,omitempty"`
	Error    string   `json:"error,omitempty"`
}

type runnerInstaller struct {
	mu     sync.Mutex
	states map[string]*runnerInstallState

	// Replaceable for tests.
	findSource func() (string, error)
	runCommand func(ctx context.Context, dir, name string, args, env []string, stdin []byte, out io.Writer) error
}

func newRunnerInstaller() *runnerInstaller {
	return &runnerInstaller{states: map[string]*runnerInstallState{}, findSource: findRunnerSource, runCommand: runInstallCommand}
}

// start begins installing ref's runner in the background. password, if not
// nil, becomes the installer's copy and is cleared when the install ends.
func (m *runnerInstaller) start(ref RunnerRef, target string, password []byte) error {
	if ref.SSH == "" {
		return errors.New("only a runner reached over ssh can be installed here")
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	if target == "" {
		target = "server:" + ref.Name
	}
	if !runnerTargetRe.MatchString(target) {
		return fmt.Errorf("target must be server:<name>, got %q", target)
	}
	if password != nil && (len(password) == 0 || len(password) > 1024 || bytes.ContainsAny(password, "\r\n\x00")) {
		return errors.New("the sudo password must be one line of at most 1024 bytes")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.states[ref.Name]; st != nil && st.Running {
		return errors.New("the runner installation is running already")
	}
	st := &runnerInstallState{Target: target, Running: true}
	m.states[ref.Name] = st
	go m.run(ref, st, password)
	return nil
}

func (m *runnerInstaller) status(name string) (runnerInstallState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.states[name]
	if !ok {
		return runnerInstallState{}, false
	}
	out := *st
	out.Log = slices.Clone(st.Log)
	return out, true
}

func (m *runnerInstaller) logf(st *runnerInstallState, format string, args ...any) {
	line := safetext.Line(fmt.Sprintf(format, args...))
	if line == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st.Log = append(st.Log, line)
	if len(st.Log) > maxInstallLogLines {
		st.Log = append([]string{"… earlier output omitted …"}, st.Log[len(st.Log)-maxInstallLogLines+1:]...)
	}
}

func (m *runnerInstaller) finish(st *runnerInstallState, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st.Running, st.Finished = false, true
	if err != nil {
		st.Error = safetext.Line(err.Error())
	}
}

func (m *runnerInstaller) run(ref RunnerRef, st *runnerInstallState, password []byte) {
	if password != nil {
		defer clear(password)
	}
	err := m.install(ref, st, password)
	m.finish(st, err)
}

// The Linux build, as "make deploy-runner" does it.
var linuxBuild = []string{"GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0"}

var (
	// What "git describe" may contribute to the version flags.
	versionRe = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`)
	// The remote install directory comes from the host's mktemp; it goes
	// into scp and the remote shell, so it must be shell-inert.
	remoteDirRe = regexp.MustCompile(`^/[A-Za-z0-9._/-]{1,200}/hcc-install\.[A-Za-z0-9]{8,16}$`)
)

func (m *runnerInstaller) install(ref RunnerRef, st *runnerInstallState, password []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), runnerInstallTimeout)
	defer cancel()
	log := installLogWriter{m, st}
	m.logf(st, "Finding the Harness Control Center source checkout…")
	root, err := m.findSource()
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "hcc-runner-install-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	m.logf(st, "Building the Linux/amd64 runner binaries…")
	ver, commit := m.gitOutput(ctx, root, "describe", "--tags", "--always", "--dirty"), m.gitOutput(ctx, root, "rev-parse", "--short", "HEAD")
	const versionPkg = "github.com/first-it-consulting/harness-control-center/internal/version"
	for _, b := range []struct{ out, pkg, ldflags string }{
		{"hcc-linux-amd64", "./cmd/hcc", "-s -w -X " + versionPkg + ".Version=" + ver + " -X " + versionPkg + ".Commit=" + commit},
		{"hcc-helper-linux-amd64", "./cmd/hcc-helper", "-s -w"},
		{"hcc-guest-linux-amd64", "./cmd/hcc-guest", "-s -w"},
	} {
		if err := m.runCommand(ctx, root, "go", []string{"build", "-trimpath", "-ldflags", b.ldflags, "-o", filepath.Join(tmp, b.out), b.pkg}, linuxBuild, nil, log); err != nil {
			return fmt.Errorf("build %s: %w", b.pkg, err)
		}
	}

	m.logf(st, "Uploading the installer to %s…", ref.SSH)
	// A fresh directory of the user's on the host, for this install only.
	var dirOut bytes.Buffer
	if err := m.runCommand(ctx, root, "ssh", []string{"-T", "-o", "BatchMode=yes", "--", ref.SSH, `mktemp -d "$HOME/hcc-install.XXXXXXXXXX"`}, nil, nil, &dirOut); err != nil {
		return fmt.Errorf("prepare %s: %w", ref.SSH, err)
	}
	dir := strings.TrimSpace(dirOut.String())
	if !remoteDirRe.MatchString(dir) {
		return fmt.Errorf("prepare %s: unexpected install directory %q", ref.SSH, safetext.Line(dir))
	}
	files := []string{filepath.Join(tmp, "hcc-linux-amd64"), filepath.Join(tmp, "hcc-helper-linux-amd64"), filepath.Join(tmp, "hcc-guest-linux-amd64"),
		filepath.Join(root, "deploy", "install-runner.sh"), filepath.Join(root, "deploy", "hcc-runner.service"), filepath.Join(root, "deploy", "sudo-askpass"),
		filepath.Join(root, "deploy", "images")}
	args := append([]string{"-q", "-r"}, files...)
	args = append(args, ref.SSH+":"+dir+"/")
	if err := m.runCommand(ctx, root, "scp", args, nil, nil, log); err != nil {
		return fmt.Errorf("upload to %s: %w", ref.SSH, err)
	}

	m.logf(st, "Installing the runner on %s as %s…", ref.SSH, st.Target)
	// The script has no single quotes: it goes into sh -c '...'. dir and
	// the target are checked above to be shell-inert.
	script := `cd ` + dir + ` || exit 1
trap "cd / && rm -rf ` + dir + `" EXIT
`
	if password == nil {
		script += `sudo -n -v 2>/dev/null || { echo "sudo needs your password: enter it and install again" >&2; exit 96; }
`
	} else {
		// sudo remembers a password per terminal (or calling process), so
		// install-runner.sh asks the askpass helper for it instead.
		script += `IFS= read -r p
printf "%s\n" "$p" | sudo -S -p "" -v 2>/dev/null || { echo "sudo did not accept the password" >&2; exit 97; }
HCC_SUDO_PASSWORD=$p; unset p; export HCC_SUDO_PASSWORD
SUDO_ASKPASS=` + dir + `/sudo-askpass; export SUDO_ASKPASS
chmod 0700 "$SUDO_ASKPASS"
`
	}
	script += `./install-runner.sh ./hcc-linux-amd64 ./hcc-helper-linux-amd64 ./hcc-guest-linux-amd64 "$HOME/.cache/hcc-poc" ` + st.Target + `
`
	var stdin []byte
	if password != nil {
		stdin = append(append([]byte(nil), password...), '\n')
		defer clear(stdin)
	}
	if err := m.runCommand(ctx, root, "ssh", []string{"-T", "-o", "BatchMode=yes", "--", ref.SSH, "sh -c '" + script + "'"}, nil, stdin, log); err != nil {
		return fmt.Errorf("install on %s: %w", ref.SSH, err)
	}
	m.logf(st, "Runner installed. A new status check should reach it shortly.")
	return nil
}

// gitOutput is one word of git's output in the checkout, or "unknown".
func (m *runnerInstaller) gitOutput(ctx context.Context, root string, args ...string) string {
	var out bytes.Buffer
	if err := m.runCommand(ctx, root, "git", args, nil, nil, &out); err != nil {
		return "unknown"
	}
	if v := strings.TrimSpace(out.String()); versionRe.MatchString(v) {
		return v
	}
	return "unknown"
}

type runnerInstallRequest struct {
	Target string `json:"target"`
	// Confirmed must be true: installing creates system users and a sudo rule.
	Confirmed bool `json:"confirmed"`
	// SudoPassword is used once on the runner host and is never stored,
	// logged or returned.
	SudoPassword *string `json:"sudoPassword,omitempty"`
}

func (s *Server) startRunnerInstall(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("runner")
	s.mu.Lock()
	ref, ok := s.cfg.Runner(name)
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, errNotFound("no runner called "+name))
		return
	}
	var req runnerInstallRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !req.Confirmed {
		writeError(w, http.StatusBadRequest, errString("confirm that you want to install the runner and make system changes"))
		return
	}
	var password []byte
	if req.SudoPassword != nil {
		password = []byte(*req.SudoPassword)
		*req.SudoPassword = ""
	}
	if err := s.installs.start(ref, req.Target, password); err != nil {
		clear(password)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	st, _ := s.installs.status(name)
	s.log.Info("runner installation started", "runner", name, "target", st.Target)
	writeJSON(w, http.StatusAccepted, st)
}

func (s *Server) runnerInstallState(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("runner")
	if _, ok := s.config().Runner(name); !ok {
		writeError(w, http.StatusNotFound, errNotFound("no runner called "+name))
		return
	}
	st, ok := s.installs.status(name)
	if !ok {
		writeError(w, http.StatusNotFound, errNotFound("no installation for runner "+name))
		return
	}
	writeJSON(w, http.StatusOK, st)
}

type installLogWriter struct {
	m  *runnerInstaller
	st *runnerInstallState
}

func (w installLogWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.ReplaceAll(string(p), "\r\n", "\n"), "\n") {
		w.m.logf(w.st, "%s", line)
	}
	return len(p), nil
}

// findRunnerSource locates the checkout whose installer is uploaded and
// run with sudo on the server, so it is never searched for: it is
// HCC_SOURCE_DIR, or the checkout of the running bin/hcc.
func findRunnerSource() (string, error) {
	if d := os.Getenv("HCC_SOURCE_DIR"); d != "" {
		if hasRunnerSource(d) {
			return filepath.Clean(d), nil
		}
		return "", fmt.Errorf("HCC_SOURCE_DIR %s is not an hcc source checkout", d)
	}
	if exe, err := os.Executable(); err == nil {
		if exe, err = filepath.EvalSymlinks(exe); err == nil && filepath.Base(filepath.Dir(exe)) == "bin" {
			if root := filepath.Dir(filepath.Dir(exe)); hasRunnerSource(root) {
				return root, nil
			}
		}
	}
	return "", errors.New("runner installation from the web UI needs the hcc source checkout: run its bin/hcc, or set HCC_SOURCE_DIR")
}

func hasRunnerSource(dir string) bool {
	for _, p := range []string{"go.mod", "Makefile", filepath.Join("deploy", "install-runner.sh"), filepath.Join("deploy", "hcc-runner.service"), filepath.Join("deploy", "sudo-askpass"), filepath.Join("deploy", "images")} {
		// dir is HCC_SOURCE_DIR or this executable's checkout; only stat'ed.
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil { //nolint:gosec // G703: see above
			return false
		}
	}
	return true
}

func runInstallCommand(ctx context.Context, dir, name string, args, env []string, stdin []byte, out io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed tools and validated arguments; no shell on the local side
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout, cmd.Stderr = out, out
	// Some developer shells export a stale GOROOT; Go finds its own toolchain.
	cmd.Env = slices.DeleteFunc(os.Environ(), func(s string) bool { return strings.HasPrefix(s, "GOROOT=") })
	cmd.Env = append(cmd.Env, env...)
	return cmd.Run()
}
