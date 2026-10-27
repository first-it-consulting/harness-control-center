package local

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Limits for downloads: an image file and what it unpacks to.
const (
	maxDownload = 2 << 30
	maxUnpacked = 8 << 30
)

// Source is where image files come from: a release on GitHub, or a local
// directory holding the same files (a release's files fetched by hand, or
// a test). Either way, every file is checked against the checksums this
// build of hcc carries, so the source need not be trusted.
type Source struct {
	// BaseURL is the release's download URL, e.g.
	// https://github.com/OWNER/REPO/releases/download/v1.2.3 (https only).
	BaseURL string
	// Dir, if set, is used instead of BaseURL.
	Dir string
	// Client defaults to a client that refuses anything but https.
	Client *http.Client
}

// ReleaseRepo is where releases are published.
const ReleaseRepo = "first-it-consulting/harness-control-center"

// ReleaseSource is the GitHub release of version (as GoReleaser sets it,
// without the "v").
func ReleaseSource(version string) Source {
	return Source{BaseURL: "https://github.com/" + ReleaseRepo + "/releases/download/v" + version}
}

func httpsOnly() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			TLSHandshakeTimeout:   30 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return errors.New("refusing a redirect away from https")
			}
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

// open returns the named file's contents.
func (s Source) open(ctx context.Context, name string) (io.ReadCloser, error) {
	if s.Dir != "" {
		return os.Open(filepath.Join(s.Dir, name)) //nolint:gosec // name is a checked file name
	}
	return get(ctx, s.Client, s.BaseURL+"/"+name)
}

func get(ctx context.Context, c *http.Client, rawURL string) (io.ReadCloser, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" {
		return nil, fmt.Errorf("refusing to download from %q: https only", rawURL)
	}
	if c == nil {
		c = httpsOnly()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("download %s: %s", u.Redacted(), resp.Status)
	}
	return resp.Body, nil
}

// Check is an expected digest: "sha256:HEX" or an npm integrity value
// ("sha512-BASE64").
type Check string

func (c Check) hasher() (hash.Hash, func([]byte) bool, error) {
	switch {
	case strings.HasPrefix(string(c), "sha256:"):
		want, err := hex.DecodeString(strings.TrimPrefix(string(c), "sha256:"))
		if err != nil || len(want) != sha256.Size {
			return nil, nil, fmt.Errorf("invalid checksum %q", c)
		}
		return sha256.New(), func(got []byte) bool { return bytes.Equal(got, want) }, nil
	case strings.HasPrefix(string(c), "sha512-"):
		want, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(string(c), "sha512-"))
		if err != nil || len(want) != sha512.Size {
			return nil, nil, fmt.Errorf("invalid integrity value %q", c)
		}
		return sha512.New(), func(got []byte) bool { return bytes.Equal(got, want) }, nil
	}
	return nil, nil, fmt.Errorf("unsupported checksum %q", c)
}

// fetchTo copies r to dest, unpacking it if gz is set (sparsely: runs of
// zero bytes become holes), and verifies r against check. dest appears
// only if everything matched; on any error nothing is left behind.
func fetchTo(r io.Reader, check Check, dest string, gz bool, progress func(int64)) (err error) {
	h, ok, err := check.hasher()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	part := dest + ".part"
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // our own directory
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(part)
		}
	}()
	in := &counter{r: io.TeeReader(io.LimitReader(r, maxDownload+1), h), fn: progress}
	var src io.Reader = in
	if gz {
		zr, err := gzip.NewReader(in)
		if err != nil {
			return fmt.Errorf("unpack: %w", err)
		}
		zr.Multistream(false)
		src = io.LimitReader(zr, maxUnpacked+1)
	}
	n, err := writeSparse(f, src)
	if err != nil {
		return err
	}
	if n > maxUnpacked {
		return errors.New("the file unpacks to more than the limit")
	}
	// Whatever follows the gzip stream still counts for the checksum.
	if _, err := io.Copy(io.Discard, in); err != nil {
		return err
	}
	if in.n > maxDownload {
		return errors.New("the download is larger than the limit")
	}
	if !ok(h.Sum(nil)) {
		return errors.New("CHECKSUM MISMATCH: the downloaded file is not the one this hcc release expects")
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return os.Rename(part, dest)
}

type counter struct {
	r  io.Reader
	n  int64
	fn func(int64)
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.fn != nil && n > 0 {
		c.fn(c.n)
	}
	return n, err
}

// writeSparse copies r to f, seeking over all-zero blocks so that they
// take no space (VM images are mostly empty).
func writeSparse(f *os.File, r io.Reader) (int64, error) {
	const block = 64 << 10
	buf := make([]byte, block)
	var off int64
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			if isZero(buf[:n]) {
				if _, serr := f.Seek(int64(n), io.SeekCurrent); serr != nil {
					return off, serr
				}
			} else if _, werr := f.Write(buf[:n]); werr != nil {
				return off, werr
			}
			off += int64(n)
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return off, err
		}
	}
	// A trailing hole needs the file's size set explicitly.
	return off, f.Truncate(off)
}

func isZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
