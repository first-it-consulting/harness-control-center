package local

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/safetext"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox/vz"
)

// Claude Code is Anthropic's software, not ours to redistribute, so the
// release images leave it out (ADR-0016) and setup fetches it on the
// user's Mac. It must match hack/vz/inputs.sh (a test checks).
const (
	ClaudeCodeVersion   = "2.1.310"
	claudeCodeIntegrity = "sha512-w0h8eJdDUmnCAtMSthCBC8AeB0nmnBhueVD9XPyPOxC8Sf+4zLy6k0hbAC+StonX80M0f7YVzHaQ9VXtNSxlWQ=="
	claudeCodeURL       = "https://registry.npmjs.org/@anthropic-ai/claude-code-linux-arm64-musl/-/claude-code-linux-arm64-musl-" + ClaudeCodeVersion + ".tgz"
)

// claudePin is one Claude Code build: where to fetch it and its
// expected integrity.
type claudePin struct {
	Version   string
	URL       string
	Integrity Check
}

var defaultClaude = claudePin{Version: ClaudeCodeVersion, URL: claudeCodeURL, Integrity: claudeCodeIntegrity}

// installTimeout bounds the VM that adds Claude Code to the image.
const installTimeout = 5 * time.Minute

// installClaude adds Claude Code (the npm package at pkg, already
// verified) to the claude-code image at rootfs, in a VM booted from the
// base image: macOS cannot write ext4 itself. The VM has no network.
func installClaude(ctx context.Context, newMachine func(vz.MachineConfig) (vz.Machine, error), version, kernel, base, pkg, rootfs string) error {
	st, err := os.Stat(pkg)
	if err != nil {
		return err
	}
	size := st.Size()
	// Drives are read in whole sectors: pad the package with zeros (the
	// guest reads only hcc.size bytes).
	if err := os.Truncate(pkg, (size+511)/512*512); err != nil {
		return err
	}
	pr, pw := io.Pipe()
	var (
		wg     sync.WaitGroup
		result string
		failed string
	)
	wg.Go(func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "HCC-INSTALL-OK"):
				result = safetext.Line(strings.TrimSpace(strings.TrimPrefix(line, "HCC-INSTALL-OK")))
			case strings.HasPrefix(line, "HCC-INSTALL-ERROR"):
				failed = safetext.Line(strings.TrimSpace(strings.TrimPrefix(line, "HCC-INSTALL-ERROR")))
			}
		}
		_, _ = io.Copy(io.Discard, pr)
	})
	err = vz.RunUtility(ctx, vz.Utility{
		Kernel:     kernel,
		Disks:      []vz.Disk{{Path: base, ReadOnly: true}, {Path: pkg, ReadOnly: true}, {Path: rootfs}},
		BootArgs:   []string{"hcc.mode=install-claude", "hcc.size=" + strconv.FormatInt(size, 10)},
		CPUs:       2,
		MemoryMiB:  1024,
		Timeout:    installTimeout,
		Console:    pw,
		NewMachine: newMachine,
	})
	_ = pw.Close()
	wg.Wait()
	switch {
	case err != nil:
		return fmt.Errorf("adding Claude Code to the image: %w", err)
	case failed != "":
		return fmt.Errorf("adding Claude Code to the image: %s", failed)
	case !strings.Contains(result, version):
		return errors.New("adding Claude Code to the image: it did not report its version")
	}
	return nil
}

// fetchClaudeCode downloads Claude Code's npm package to dir/claude-code.tgz.
func fetchClaudeCode(ctx context.Context, src Source, pin claudePin, dir string, progress func(int64)) (string, error) {
	dest := filepath.Join(dir, "claude-code.tgz")
	r, err := get(ctx, src.Client, pin.URL)
	if err != nil {
		return "", err
	}
	defer func() { _ = r.Close() }()
	if err := fetchTo(r, pin.Integrity, dest, false, progress); err != nil {
		return "", fmt.Errorf("fetching Claude Code %s: %w", pin.Version, err)
	}
	return dest, nil
}
