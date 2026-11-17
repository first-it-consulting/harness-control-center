package runner

import (
	"bytes"
	"strings"
	"testing"
)

func TestMaskWriter(t *testing.T) {
	var out bytes.Buffer
	kube := "apiVersion: v1\nusers:\n- user: {token: tok-0123456789}\n"
	w := newMaskWriter(&out, []string{"sk-secret-value", kube, "abc"})
	// Split across writes, several on a line, a line of a file secret.
	for _, p := range []string{"key=sk-sec", "ret-value and again sk-secret-value\n", "token: {token: tok-0123456789}\n", "short abc stays\n", "end sk-secret-"} {
		if _, err := w.Write([]byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Write([]byte("value")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, leak := range []string{"sk-secret", "tok-0123456789"} {
		if strings.Contains(got, leak) {
			t.Errorf("%q leaked:\n%s", leak, got)
		}
	}
	want := "key=*** and again ***\ntoken: {token: ***}\nshort abc stays\nend ***"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A line longer than the buffer goes out in pieces without leaking a
// secret across a cut.
func TestMaskWriterLongLines(t *testing.T) {
	const secret = "sk-0123456789abcdef"
	for cut := 0; cut < len(secret); cut++ {
		var out bytes.Buffer
		w := newMaskWriter(&out, []string{secret})
		filler := strings.Repeat("x", maxMaskLine-cut)
		_, _ = w.Write([]byte(filler + secret[:cut]))
		_, _ = w.Write([]byte(secret[cut:] + strings.Repeat("y", 10)))
		_ = w.Close()
		if strings.Contains(out.String(), "0123456789") || !strings.Contains(out.String(), Masked) {
			t.Fatalf("cut %d: leaked or not masked (%d bytes)", cut, out.Len())
		}
	}
}

func TestMaskWriterWithoutSecrets(t *testing.T) {
	var out bytes.Buffer
	w := newMaskWriter(&out, nil)
	_, _ = w.Write([]byte("as is"))
	if out.String() != "as is" {
		t.Errorf("%q", out.String())
	}
}
