package local

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func gz(t *testing.T, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func sha256Check(data []byte) Check {
	s := sha256.Sum256(data)
	return Check("sha256:" + hex.EncodeToString(s[:]))
}

func TestFetchUnpacksSparsely(t *testing.T) {
	// An "image": data, a long run of zeros, data, zeros at the end.
	img := make([]byte, 1<<20)
	copy(img, "start")
	copy(img[600<<10:], "middle")
	packed := gz(t, img)
	dest := filepath.Join(t.TempDir(), "img", "rootfs.ext4")
	var last int64
	if err := fetchTo(bytes.NewReader(packed), sha256Check(packed), dest, true, func(n int64) { last = n }); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, img) {
		t.Fatal("unpacked file differs")
	}
	if last != int64(len(packed)) {
		t.Errorf("progress ended at %d, want %d", last, len(packed))
	}
	if fi, _ := os.Stat(dest); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
}

func TestFetchRefusesAMismatch(t *testing.T) {
	packed := gz(t, []byte("the real image"))
	evil := gz(t, []byte("another image"))
	dest := filepath.Join(t.TempDir(), "rootfs.ext4")
	err := fetchTo(bytes.NewReader(evil), sha256Check(packed), dest, true, nil)
	if err == nil || !strings.Contains(err.Error(), "CHECKSUM MISMATCH") {
		t.Fatalf("got %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(dest)); len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
}

func TestFetchCountsTrailingBytes(t *testing.T) {
	// Bytes after the gzip stream are part of the file, so they change
	// the checksum: a valid image with something appended is refused.
	packed := gz(t, []byte("image"))
	dest := filepath.Join(t.TempDir(), "x")
	if err := fetchTo(bytes.NewReader(append(append([]byte{}, packed...), "junk"...)), sha256Check(packed), dest, true, nil); err == nil {
		t.Fatal("accepted")
	}
}

func TestFetchNPMIntegrity(t *testing.T) {
	data := []byte("a package")
	s := sha512.Sum512(data)
	good := Check("sha512-" + base64.StdEncoding.EncodeToString(s[:]))
	dir := t.TempDir()
	if err := fetchTo(bytes.NewReader(data), good, filepath.Join(dir, "ok.tgz"), false, nil); err != nil {
		t.Fatal(err)
	}
	if err := fetchTo(bytes.NewReader([]byte("another")), good, filepath.Join(dir, "bad.tgz"), false, nil); err == nil {
		t.Fatal("mismatch accepted")
	}
}

func TestChecksRejectMalformed(t *testing.T) {
	for _, c := range []Check{"", "md5:abc", "sha256:xyz", "sha256:abcd", "sha512-!!", "sha512-YWJj"} {
		if _, _, err := c.hasher(); err == nil {
			t.Errorf("%q accepted", c)
		}
	}
}

func TestDownloadsAreHTTPSOnly(t *testing.T) {
	if _, err := get(context.Background(), nil, "http://example.com/x"); err == nil {
		t.Error("http accepted")
	}
	redirect := httpsOnly().CheckRedirect
	if err := redirect(&http.Request{URL: &url.URL{Scheme: "http", Host: "example.com"}}, nil); err == nil {
		t.Error("redirect to http accepted")
	}
	if err := redirect(&http.Request{URL: &url.URL{Scheme: "https", Host: "example.com"}}, nil); err != nil {
		t.Error(err)
	}
}

func TestDownloadFailsOnHTTPError(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	src := Source{BaseURL: srv.URL, Client: srv.Client()}
	if _, err := src.open(context.Background(), "vz-kernel-arm64.gz"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("got %v", err)
	}
}

func TestReleaseSource(t *testing.T) {
	if got := ReleaseSource("1.2.3").BaseURL; got != "https://github.com/first-it-consulting/harness-control-center/releases/download/v1.2.3" {
		t.Error(got)
	}
}

// The pin here must be the one the image build uses.
func TestClaudePinMatchesBuildInputs(t *testing.T) {
	data, err := os.ReadFile("../../hack/vz/inputs.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(data), "\r\n", "\n") // a Windows checkout
	for _, want := range []string{`CC_VERSION=` + ClaudeCodeVersion + "\n", `CC_INTEGRITY="` + claudeCodeIntegrity + `"`} {
		if !strings.Contains(s, want) {
			t.Errorf("hack/vz/inputs.sh lacks %s", want)
		}
	}
}
