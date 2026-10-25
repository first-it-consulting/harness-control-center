package runnerd

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
)

// MaxConsoleBytes caps each job's console log, so a guest cannot fill the
// host's disk by printing.
const MaxConsoleBytes = 10 << 20

const truncatedMarker = "\n[hcc: console output truncated at 10 MiB]\n"

// consoleLog is a size-capped, concurrency-safe console log file. Writes
// beyond the cap are dropped but reported as successful, so a chatty
// guest cannot make the VMM block or fail.
type consoleLog struct {
	mu        sync.Mutex
	f         *os.File
	written   int64
	truncated bool
}

func consolePath(stateDir, id string) (string, error) {
	if err := sandbox.ValidateID(id); err != nil {
		return "", err
	}
	return filepath.Join(stateDir, "jobs", id, "console.log"), nil
}

func openConsoleLog(stateDir, id string) (*consoleLog, error) {
	p, err := consolePath(stateDir, id)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // path built from a validated id
	if err != nil {
		return nil, fmt.Errorf("runnerd: console log: %w", err)
	}
	return &consoleLog{f: f}, nil
}

func (c *consoleLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.truncated {
		return len(p), nil
	}
	room := MaxConsoleBytes - c.written
	chunk := p
	if int64(len(chunk)) > room {
		chunk = chunk[:room]
	}
	n, err := c.f.Write(chunk)
	c.written += int64(n)
	if err != nil {
		return n, err
	}
	if len(chunk) < len(p) {
		c.truncated = true
		_, _ = c.f.WriteString(truncatedMarker)
	}
	return len(p), nil
}

func (c *consoleLog) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.f.Close()
}
