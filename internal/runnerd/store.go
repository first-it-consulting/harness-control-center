package runnerd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/runner"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
)

// maxJobFileBytes bounds a stored job record when it is read back.
const maxJobFileBytes = 1 << 20

// FileStore keeps each job in <stateDir>/jobs/<id>/job.json, next to its
// console log and results, written atomically (temporary file, then
// rename) and readable by the runner user only.
type FileStore struct {
	stateDir string
	log      *slog.Logger
}

var _ runner.Store = (*FileStore)(nil)

// NewFileStore returns a store below stateDir.
func NewFileStore(stateDir string, log *slog.Logger) *FileStore {
	return &FileStore{stateDir: stateDir, log: log}
}

func (s *FileStore) jobDir(id string) (string, error) {
	if err := sandbox.ValidateID(id); err != nil {
		return "", err
	}
	return filepath.Join(s.stateDir, "jobs", id), nil
}

// Save implements runner.Store.
func (s *FileStore) Save(j runner.Job) error {
	dir, err := s.jobDir(j.ID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".job-*.json")
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
		err = os.Rename(tmp, filepath.Join(dir, "job.json"))
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// Load implements runner.Store. Unreadable records are skipped (and
// logged), so one damaged file cannot keep the runner from starting.
func (s *FileStore) Load() ([]runner.Job, error) {
	entries, err := os.ReadDir(filepath.Join(s.stateDir, "jobs"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var jobs []runner.Job
	for _, e := range entries {
		if !e.IsDir() || sandbox.ValidateID(e.Name()) != nil {
			continue
		}
		j, err := s.read(e.Name())
		if errors.Is(err, os.ErrNotExist) {
			continue // from before jobs were stored; see PruneOrphans
		}
		if err != nil {
			s.log.Warn("skipping unreadable job record", "job", e.Name(), "err", err)
			continue
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

func (s *FileStore) read(id string) (runner.Job, error) {
	var j runner.Job
	dir, err := s.jobDir(id)
	if err != nil {
		return j, err
	}
	f, err := os.Open(filepath.Join(dir, "job.json")) //nolint:gosec // path built from a validated id
	if err != nil {
		return j, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxJobFileBytes+1))
	if err != nil {
		return j, err
	}
	if len(data) > maxJobFileBytes {
		return j, errors.New("job record too large")
	}
	if err := json.Unmarshal(data, &j); err != nil {
		return j, err
	}
	if j.ID != id {
		return j, fmt.Errorf("record is for job %q", j.ID)
	}
	return j, nil
}

// Delete implements runner.Store: the job's whole directory goes.
func (s *FileStore) Delete(id string) error {
	dir, err := s.jobDir(id)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// PruneOrphans deletes job directories without a record (left by
// versions that did not store jobs, or by a crash before the first
// save) once they have not changed for maxAge.
func (s *FileStore) PruneOrphans(maxAge time.Duration, now time.Time) []string {
	entries, err := os.ReadDir(filepath.Join(s.stateDir, "jobs"))
	if err != nil {
		return nil
	}
	var pruned []string
	for _, e := range entries {
		dir, err := s.jobDir(e.Name())
		if err != nil || !e.IsDir() {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, "job.json")); !errors.Is(err, os.ErrNotExist) {
			continue
		}
		fi, err := e.Info()
		if err != nil || now.Sub(fi.ModTime()) <= maxAge {
			continue
		}
		if err := os.RemoveAll(dir); err == nil {
			pruned = append(pruned, e.Name())
		}
	}
	return pruned
}
