package plugins

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Pack makes a bundle of a plugin directory: its plain files and
// directories. Anything else (a symlink, a device) is an error rather
// than silently left out, so what is reviewed is what is installed.
func Pack(dir string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	var total int64
	files := 0
	epoch := time.Unix(0, 0)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		name := filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if files++; files > MaxBundleFiles {
			return fmt.Errorf("more than %d files", MaxBundleFiles)
		}
		switch {
		case d.IsDir():
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o755, ModTime: epoch})
		case info.Mode().IsRegular():
			if total += info.Size(); total > MaxBundleBytes {
				return fmt.Errorf("more than %d bytes", MaxBundleBytes)
			}
			mode := int64(0o644)
			if info.Mode()&0o111 != 0 {
				mode = 0o755
			}
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: info.Size(), Mode: mode, ModTime: epoch}); err != nil {
				return err
			}
			f, err := os.Open(p) //nolint:gosec // a file of the directory the user names
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.CopyN(tw, f, info.Size())
			return err
		default:
			return fmt.Errorf("%s is not a plain file or directory", name)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("plugins: %w", err)
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
