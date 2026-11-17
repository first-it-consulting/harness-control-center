package runner

import (
	"bytes"
	"io"
	"slices"
	"strings"
)

// Masked is what a secret looks like in a job's console (ADR-0020).
const Masked = "***"

// minMasked is the shortest piece of a secret that is masked: shorter
// ones would mask ordinary text and say little about the secret.
const minMasked = 4

// minTokenMasked is the shortest token-like word of a secret file that
// is masked on its own.
const minTokenMasked = 12

// maxMaskLine bounds the console line a maskWriter holds back.
const maxMaskLine = 64 << 10

// maskWriter writes the console with every secret replaced by Masked,
// like CI systems do. It works line by line, so a secret split across
// writes is still found; a secret of several lines (a kubeconfig) is
// masked per line and per token-like word. It is a safety net, like a CI
// system's: an agent that changes a secret before printing it is not
// caught. Close flushes the last, unfinished line.
type maskWriter struct {
	w       io.Writer
	secrets [][]byte // longest first
	buf     []byte
}

// newMaskWriter masks the given secret values in w; with none, it is w.
func newMaskWriter(w io.Writer, values []string) io.WriteCloser {
	seen := map[string]bool{}
	var secrets [][]byte
	add := func(s string) {
		if len(s) >= minMasked && !seen[s] {
			seen[s] = true
			secrets = append(secrets, []byte(s))
		}
	}
	for _, v := range values {
		lines := strings.Split(strings.TrimRight(v, "\n"), "\n")
		for _, line := range lines {
			add(strings.TrimSpace(line))
			if len(lines) < 2 {
				continue
			}
			// In a file (a kubeconfig), the secret parts sit inside lines:
			// token-like words are masked on their own too.
			for _, word := range strings.FieldsFunc(line, func(r rune) bool { return strings.ContainsRune(" \t:\"'{}[],=", r) }) {
				if len(word) >= minTokenMasked && strings.ContainsAny(word, "0123456789") {
					add(word)
				}
			}
		}
	}
	slices.SortFunc(secrets, func(a, b []byte) int { return len(b) - len(a) })
	return &maskWriter{w: w, secrets: secrets}
}

func (m *maskWriter) Write(p []byte) (int, error) {
	if len(m.secrets) == 0 {
		return m.w.Write(p)
	}
	m.buf = append(m.buf, p...)
	for {
		i := bytes.IndexByte(m.buf, '\n')
		if i < 0 {
			break
		}
		if err := m.emit(m.buf[:i+1]); err != nil {
			return 0, err
		}
		m.buf = m.buf[i+1:]
	}
	// A very long line goes out in pieces. Masked first, so that only the
	// start of a secret not complete yet can be at its end: that much is
	// kept back for the next write.
	if len(m.buf) > maxMaskLine {
		masked := m.mask(m.buf)
		keep := len(m.secrets[0]) - 1
		if _, err := m.w.Write(masked[:len(masked)-keep]); err != nil {
			return 0, err
		}
		m.buf = append([]byte(nil), masked[len(masked)-keep:]...)
	}
	return len(p), nil
}

func (m *maskWriter) mask(b []byte) []byte {
	out := b
	for _, s := range m.secrets {
		out = bytes.ReplaceAll(out, s, []byte(Masked))
	}
	return out
}

func (m *maskWriter) emit(b []byte) error {
	_, err := m.w.Write(m.mask(b))
	return err
}

// Close writes what is left of the last line.
func (m *maskWriter) Close() error {
	if len(m.buf) == 0 {
		return nil
	}
	err := m.emit(m.buf)
	m.buf = nil
	return err
}
