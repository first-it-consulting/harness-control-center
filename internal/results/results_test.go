package results

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

type entry struct {
	name     string
	typ      byte
	body     string
	mode     int64
	linkname string
}

func archive(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.linkname, Uid: 1000, Gid: 1000, Uname: "agent"}
		if e.typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if h.Mode == 0 {
			h.Mode = 0o600
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			_, _ = io.WriteString(tw, e.body)
		}
	}
	_ = tw.Close()
	return buf.Bytes()
}

func read(t *testing.T, data []byte) map[string]*tar.Header {
	t.Helper()
	out := map[string]*tar.Header{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = h
	}
}

func TestSanitizeKeepsSafeContent(t *testing.T) {
	src := archive(t,
		entry{name: "./", typ: tar.TypeDir},
		entry{name: "./src/", typ: tar.TypeDir},
		entry{name: "./src/main.go", typ: tar.TypeReg, body: "package main\n"},
		entry{name: "./fib.sh", typ: tar.TypeReg, body: "echo 1\n", mode: 0o4755}, // setuid stripped
	)
	// Followed by the drive's zeros, as on the real output drive.
	src = append(src, make([]byte, 64<<10)...)
	var out bytes.Buffer
	sum, err := Sanitize(&out, bytes.NewReader(src), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Files != 2 || sum.Bytes != int64(len("package main\n")+len("echo 1\n")) || len(sum.Skipped) != 0 {
		t.Errorf("summary = %+v", sum)
	}
	got := read(t, out.Bytes())
	if h := got["fib.sh"]; h == nil || h.Mode != 0o755 || h.Uid != 0 || h.Uname != "" {
		t.Errorf("fib.sh header = %+v (want 0755, root, no names)", h)
	}
	if h := got["src/main.go"]; h == nil || h.Mode != 0o644 {
		t.Errorf("src/main.go header = %+v", h)
	}
	if got["src/"] == nil {
		t.Error("directory dropped")
	}
}

func TestSanitizeDropsDangerousEntries(t *testing.T) {
	src := archive(t,
		entry{name: "ok.txt", typ: tar.TypeReg, body: "ok"},
		entry{name: "../escape.txt", typ: tar.TypeReg, body: "x"},
		entry{name: "a/../../escape2.txt", typ: tar.TypeReg, body: "x"},
		entry{name: "/etc/passwd", typ: tar.TypeReg, body: "x"},
		entry{name: "link", typ: tar.TypeSymlink, linkname: "/etc/shadow"},
		entry{name: "hard", typ: tar.TypeLink, linkname: "ok.txt"},
		entry{name: "dev", typ: tar.TypeChar},
		entry{name: "fifo", typ: tar.TypeFifo},
		entry{name: "bad\x01name", typ: tar.TypeReg, body: "x"},
		entry{name: "win\\path", typ: tar.TypeReg, body: "x"},
	)
	var out bytes.Buffer
	sum, err := Sanitize(&out, bytes.NewReader(src), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	got := read(t, out.Bytes())
	if len(got) != 1 || got["ok.txt"] == nil {
		t.Errorf("kept %v, want only ok.txt", got)
	}
	if len(sum.Skipped) != 9 {
		t.Errorf("skipped = %v", sum.Skipped)
	}
}

func TestSanitizeLimits(t *testing.T) {
	big := archive(t, entry{name: "a", typ: tar.TypeReg, body: strings.Repeat("x", 100)})
	if _, err := Sanitize(io.Discard, bytes.NewReader(big), Limits{MaxBytes: 50}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("size limit: %v", err)
	}
	many := archive(t, entry{name: "a", typ: tar.TypeReg}, entry{name: "b", typ: tar.TypeReg}, entry{name: "c", typ: tar.TypeReg})
	if _, err := Sanitize(io.Discard, bytes.NewReader(many), Limits{MaxFiles: 2}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("file limit: %v", err)
	}
}

func TestSanitizeNoArchive(t *testing.T) {
	var out bytes.Buffer
	for name, src := range map[string][]byte{
		"zeros":   make([]byte, 1<<20),
		"empty":   nil,
		"garbage": bytes.Repeat([]byte("not a tar archive "), 100),
	} {
		sum, err := Sanitize(&out, bytes.NewReader(src), Limits{})
		if err != nil || sum.Files != 0 || out.Len() != 0 {
			t.Errorf("%s: %+v %v (%d bytes out)", name, sum, err, out.Len())
		}
	}
}

func TestSanitizeTruncatedArchive(t *testing.T) {
	src := archive(t, entry{name: "a", typ: tar.TypeReg, body: strings.Repeat("x", 4096)})
	if _, err := Sanitize(io.Discard, bytes.NewReader(src[:1024]), Limits{}); err == nil {
		t.Error("truncated archive accepted")
	}
}

// Sanitize stops at the end of the archive instead of reading the whole
// (possibly huge) drive.
func TestSanitizeStopsAtTheEnd(t *testing.T) {
	src := archive(t, entry{name: "a", typ: tar.TypeReg, body: "x"})
	r := &countingReader{r: io.MultiReader(bytes.NewReader(src), zeros{})}
	if _, err := Sanitize(io.Discard, r, Limits{}); err != nil {
		t.Fatal(err)
	}
	if r.n > int64(len(src))+64<<10 {
		t.Errorf("read %d bytes for a %d-byte archive", r.n, len(src))
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func FuzzSanitize(f *testing.F) {
	f.Add([]byte{})
	f.Add(make([]byte, 1024))
	var seed bytes.Buffer
	tw := tar.NewWriter(&seed)
	_ = tw.WriteHeader(&tar.Header{Name: "a/b", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	f.Add(seed.Bytes())
	f.Fuzz(func(t *testing.T, data []byte) {
		var out bytes.Buffer
		if _, err := Sanitize(&out, bytes.NewReader(data), Limits{MaxBytes: 1 << 20, MaxFiles: 100}); err != nil {
			return
		}
		// Whatever passes must be a clean archive.
		tr := tar.NewReader(&out)
		for {
			h, err := tr.Next()
			if err != nil {
				return
			}
			if (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir) || strings.HasPrefix(h.Name, "/") || strings.Contains(h.Name, "..") || h.Uid != 0 || h.Mode&^0o755 != 0 {
				t.Fatalf("unsafe entry passed: %+v", h)
			}
		}
	})
}
