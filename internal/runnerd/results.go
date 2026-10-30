package runnerd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/first-it-consulting/harness-control-center/internal/results"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
)

// minFreeAfterResults must stay free on the state directory's filesystem
// after a job's results are stored.
const minFreeAfterResults = 1 << 30

// spaceCheckEvery is how much is written between two free-space checks.
const spaceCheckEvery = 64 << 20

// freeSpace is freeBytes; tests replace it.
var freeSpace = freeBytes

func resultsPath(stateDir, id string) (string, error) {
	if err := sandbox.ValidateID(id); err != nil {
		return "", err
	}
	return filepath.Join(stateDir, "jobs", id, "results.tar"), nil
}

// OpenResultsFunc returns a runner.Config.ReadResults that opens a job's
// stored results archive.
func OpenResultsFunc(stateDir string) func(id string) (io.ReadCloser, error) {
	return func(id string) (io.ReadCloser, error) {
		p, err := resultsPath(stateDir, id)
		if err != nil {
			return nil, err
		}
		return os.Open(p) //nolint:gosec // path built from a validated id
	}
}

// ResultsFunc returns a runner.Config.Results that sanitizes a job's disk
// (internal/results) into <stateDir>/jobs/<id>/results.tar. Only a
// complete, clean archive is kept: on any error, or if it holds no files,
// nothing is stored.
//
// Jobs store one at a time. The free space is checked while the archive
// is written, not reserved for the whole limit up front: most results are
// far smaller than the job's disk, so a nearly full runner still stores
// them, and the archive stops before minFreeAfterResults is used up.
func ResultsFunc(stateDir string) func(id string, disk io.Reader, maxBytes int64) (results.Summary, error) {
	var mu sync.Mutex
	return func(id string, disk io.Reader, maxBytes int64) (results.Summary, error) {
		mu.Lock()
		defer mu.Unlock()
		var sum results.Summary
		p, err := resultsPath(stateDir, id)
		if err != nil {
			return sum, err
		}
		dir := filepath.Dir(p)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return sum, err
		}
		g := &spaceGuard{dir: dir}
		if err := g.check(0); err != nil {
			return sum, err
		}
		tmp := p + ".partial"
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // path built from a validated id
		if err != nil {
			return sum, err
		}
		g.w = f
		sum, err = results.Sanitize(g, disk, results.Limits{MaxBytes: maxBytes})
		err = errors.Join(err, f.Close())
		if err != nil || sum.Files == 0 {
			return sum, errors.Join(err, os.Remove(tmp))
		}
		return sum, os.Rename(tmp, p)
	}
}

// spaceGuard writes to w while the filesystem of dir keeps
// minFreeAfterResults free, checking again every spaceCheckEvery bytes.
type spaceGuard struct {
	w    io.Writer
	dir  string
	left int64 // bytes that may be written before the next check
}

func (g *spaceGuard) Write(p []byte) (int, error) {
	if int64(len(p)) > g.left {
		if err := g.check(int64(len(p))); err != nil {
			return 0, err
		}
	}
	g.left -= int64(len(p))
	return g.w.Write(p)
}

// check fails unless n bytes more fit with minFreeAfterResults to spare.
func (g *spaceGuard) check(n int64) error {
	free, err := freeSpace(g.dir)
	if err != nil {
		return err
	}
	if free < minFreeAfterResults+uint64(n) { //nolint:gosec // n is a length
		return fmt.Errorf("not enough space to store the results: %d bytes free, %d must stay free", free, minFreeAfterResults)
	}
	g.left = min(int64(free-minFreeAfterResults), spaceCheckEvery) //nolint:gosec // free > minFreeAfterResults
	return nil
}
