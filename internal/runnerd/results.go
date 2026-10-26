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
// Jobs store one at a time, so the free-space check holds for each.
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
		// Content plus tar headers for every file, and headroom.
		need := maxBytes + int64(results.DefaultLimits.MaxFiles)*1024 + minFreeAfterResults
		if free, err := freeBytes(dir); err != nil {
			return sum, err
		} else if free < uint64(need) { //nolint:gosec // need is positive
			return sum, fmt.Errorf("not enough space to store up to %d bytes of results (%d bytes free)", maxBytes, free)
		}
		tmp := p + ".partial"
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // path built from a validated id
		if err != nil {
			return sum, err
		}
		sum, err = results.Sanitize(f, disk, results.Limits{MaxBytes: maxBytes})
		err = errors.Join(err, f.Close())
		if err != nil || sum.Files == 0 {
			return sum, errors.Join(err, os.Remove(tmp))
		}
		return sum, os.Rename(tmp, p)
	}
}
