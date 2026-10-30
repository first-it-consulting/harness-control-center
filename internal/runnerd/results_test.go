package runnerd

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeFree makes freeSpace report what free returns, for the rest of the
// test.
func fakeFree(t *testing.T, free func() (uint64, error)) {
	t.Helper()
	old := freeSpace
	freeSpace = func(string) (uint64, error) { return free() }
	t.Cleanup(func() { freeSpace = old })
}

func oneFile(t *testing.T, size int) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "out.txt", Typeflag: tar.TypeReg, Size: int64(size), Mode: 0o600}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(bytes.Repeat([]byte("x"), size)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

// A nearly full runner still stores small results: the space is not
// reserved for the job's whole disk up front.
func TestResultsFuncStoresSmallResultsOnANearlyFullDisk(t *testing.T) {
	fakeFree(t, func() (uint64, error) { return minFreeAfterResults + 10<<20, nil })
	state := t.TempDir()
	sum, err := ResultsFunc(state)("job-1", oneFile(t, 100), 4<<30)
	if err != nil || sum.Files != 1 {
		t.Fatalf("Summary = %+v, %v", sum, err)
	}
	if _, err := os.Stat(filepath.Join(state, "jobs", "job-1", "results.tar")); err != nil {
		t.Error(err)
	}
}

func TestResultsFuncKeepsTheHeadroom(t *testing.T) {
	for name, free := range map[string]uint64{
		"already too full":   minFreeAfterResults - 1,
		"full while writing": minFreeAfterResults + 1000,
	} {
		t.Run(name, func(t *testing.T) {
			fakeFree(t, func() (uint64, error) { return free, nil })
			state := t.TempDir()
			_, err := ResultsFunc(state)("job-1", oneFile(t, 4000), 4<<30)
			if err == nil || !strings.Contains(err.Error(), "not enough space") {
				t.Fatalf("err = %v", err)
			}
			if entries, _ := os.ReadDir(filepath.Join(state, "jobs", "job-1")); len(entries) != 0 {
				t.Errorf("left behind: %v", entries)
			}
		})
	}
	t.Run("statfs fails", func(t *testing.T) {
		fakeFree(t, func() (uint64, error) { return 0, errors.New("statfs: boom") })
		if _, err := ResultsFunc(t.TempDir())("job-1", oneFile(t, 10), 1<<20); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Errorf("err = %v", err)
		}
	})
}

// The guard checks again once it has written spaceCheckEvery bytes, and
// stops when the space runs out on the way.
func TestSpaceGuardChecksAgain(t *testing.T) {
	var checks int
	free := uint64(minFreeAfterResults + 2*spaceCheckEvery)
	fakeFree(t, func() (uint64, error) { checks++; return free, nil })
	var out bytes.Buffer
	g := &spaceGuard{w: &out, dir: t.TempDir()}
	chunk := make([]byte, 1<<20)
	for range spaceCheckEvery >> 20 {
		if _, err := g.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if checks != 1 {
		t.Errorf("checks after %d bytes = %d", out.Len(), checks)
	}
	free = minFreeAfterResults // someone else filled the disk
	if _, err := g.Write(chunk); err == nil || checks != 2 {
		t.Errorf("write past the headroom: %v (checks %d)", err, checks)
	}
	if out.Len() != spaceCheckEvery {
		t.Errorf("wrote %d bytes", out.Len())
	}
}
