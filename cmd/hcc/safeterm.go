package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// writeSafe copies guest console output to a terminal. The output is
// controlled by the agent, so control characters that a terminal would
// act on (escape sequences can write the clipboard, retitle windows or
// redraw lines to hide text) are shown visibly instead, like "cat -v".
// Newlines and tabs pass through; "\r\n" becomes "\n".
func writeSafe(dst io.Writer, src io.Reader) error {
	in := bufio.NewReader(src)
	out := bufio.NewWriter(dst)
	for {
		r, size, err := in.ReadRune()
		if errors.Is(err, io.EOF) {
			return out.Flush()
		}
		if err != nil {
			return err
		}
		switch {
		case r == utf8.RuneError && size == 1:
			// Invalid UTF-8: show the byte. (A real U+FFFD has size 3.)
			_ = in.UnreadRune()
			b, _ := in.ReadByte()
			fmt.Fprintf(out, `\x%02x`, b)
		case r == '\n' || r == '\t':
			_, _ = out.WriteRune(r)
		case r == '\r':
			if next, _, err := in.ReadRune(); err == nil {
				_ = in.UnreadRune()
				if next == '\n' {
					continue
				}
			}
			_, _ = out.WriteString("^M")
		case r < 0x20:
			_, _ = out.WriteString("^" + string(r+'@'))
		case r == 0x7f:
			_, _ = out.WriteString("^?")
		case r >= 0x80 && r <= 0x9f, isBidiControl(r):
			// C1 controls (e.g. CSI U+009B) and bidi overrides, which can
			// reorder what is displayed.
			fmt.Fprintf(out, `\u%04x`, r)
		default:
			_, _ = out.WriteRune(r)
		}
	}
}

func isBidiControl(r rune) bool {
	return (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0x200e || r == 0x200f || r == 0x061c
}
