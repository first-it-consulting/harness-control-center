// Package results turns what a guest hands back into something safe to
// give the user. The guest writes a tar stream onto its output drive; the
// host never mounts anything the agent wrote, it only parses that stream
// here, in userspace, and re-packs what passes into a clean tar:
//
//   - only regular files and directories (no links, devices or FIFOs),
//   - relative, clean names without "..", bounded depth and length,
//   - bounded total size and file count,
//   - normalized metadata (root ownership, 0644/0755, no xattrs).
package results

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
)

// Limits bound what is accepted.
type Limits struct {
	MaxBytes int64 // total file content
	MaxFiles int
}

// DefaultLimits are used for zero fields.
var DefaultLimits = Limits{MaxBytes: 4 << 30, MaxFiles: 100_000}

// Summary describes the clean archive.
type Summary struct {
	Files   int      `json:"files"`
	Bytes   int64    `json:"bytes"`
	Skipped []string `json:"skipped,omitempty"` // entries dropped, with reasons (at most 20)
}

const (
	maxNameLen  = 1024
	maxDepth    = 64
	maxSkipList = 20
)

// ErrTooLarge is returned when the guest's archive exceeds the limits.
var ErrTooLarge = errors.New("results: archive exceeds the limits")

// Sanitize reads a tar stream from src and writes a clean one to dst. It
// stops at the end of the archive (the rest of the drive is never read).
// A guest that wrote no archive yields an empty summary and no output.
func Sanitize(dst io.Writer, src io.Reader, limits Limits) (Summary, error) {
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = DefaultLimits.MaxBytes
	}
	if limits.MaxFiles <= 0 {
		limits.MaxFiles = DefaultLimits.MaxFiles
	}
	var sum Summary
	skip := func(name, why string) {
		if len(sum.Skipped) < maxSkipList {
			sum.Skipped = append(sum.Skipped, fmt.Sprintf("%.100s: %s", name, why))
		}
	}
	tr := tar.NewReader(src)
	var tw *tar.Writer
	defer func() {
		if tw != nil {
			_ = tw.Close()
		}
	}()
	epoch := time.Unix(0, 0)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if sum.Files == 0 && errors.Is(err, tar.ErrHeader) {
				return sum, nil // no archive at all (e.g. an untouched drive)
			}
			return sum, fmt.Errorf("results: malformed archive: %w", err)
		}
		if hdr.Name == "./" || hdr.Name == "." { // the archive root: nothing to keep
			continue
		}
		name, ok := cleanName(hdr.Name)
		if !ok {
			skip(hdr.Name, "unsafe name")
			continue
		}
		var out tar.Header
		switch hdr.Typeflag {
		case tar.TypeDir:
			out = tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o755, ModTime: epoch, Format: tar.FormatPAX}
		case tar.TypeReg:
			if hdr.Size < 0 {
				skip(name, "invalid size")
				continue
			}
			if sum.Files+1 > limits.MaxFiles || sum.Bytes+hdr.Size > limits.MaxBytes {
				return sum, ErrTooLarge
			}
			mode := int64(0o644)
			if hdr.Mode&0o111 != 0 {
				mode = 0o755
			}
			out = tar.Header{Typeflag: tar.TypeReg, Name: name, Size: hdr.Size, Mode: mode, ModTime: epoch, Format: tar.FormatPAX}
		default:
			skip(name, "not a regular file or directory")
			continue
		}
		if tw == nil {
			tw = tar.NewWriter(dst)
		}
		if err := tw.WriteHeader(&out); err != nil {
			return sum, err
		}
		if out.Typeflag == tar.TypeReg {
			n, err := io.Copy(tw, io.LimitReader(tr, out.Size))
			if err != nil {
				return sum, fmt.Errorf("results: %s: %w", name, err)
			}
			if n != out.Size {
				return sum, fmt.Errorf("results: %s: truncated", name)
			}
			sum.Files++
			sum.Bytes += n
		}
	}
	if tw != nil {
		err := tw.Close()
		tw = nil
		return sum, err
	}
	return sum, nil
}

// cleanName accepts relative names that stay inside the archive.
func cleanName(name string) (string, bool) {
	name = strings.TrimPrefix(name, "./")
	if name == "" || name == "." || len(name) > maxNameLen || strings.ContainsAny(name, "\x00\\") {
		return "", false
	}
	if strings.HasPrefix(name, "/") {
		return "", false
	}
	for _, part := range strings.Split(strings.TrimSuffix(name, "/"), "/") {
		if part == ".." {
			return "", false
		}
	}
	clean := path.Clean(name)
	if clean == "." || strings.HasPrefix(clean, "../") || strings.Count(clean, "/") >= maxDepth {
		return "", false
	}
	for _, r := range clean {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	return clean, true
}
