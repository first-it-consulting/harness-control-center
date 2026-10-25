package main

import (
	"strings"
	"testing"
)

func TestWriteSafe(t *testing.T) {
	tests := map[string]struct{ in, want string }{
		"plain":            {"hello\tworld\n", "hello\tworld\n"},
		"crlf":             {"line1\r\nline2\r\n", "line1\nline2\n"},
		"lone cr":          {"secret\rhidden", "secret^Mhidden"},
		"trailing cr":      {"x\r", "x^M"},
		"ansi color":       {"\x1b[31mred\x1b[0m", "^[[31mred^[[0m"},
		"osc52 clipboard":  {"\x1b]52;c;cm0gLXJmIH4=\x07", "^[]52;c;cm0gLXJmIH4=^G"},
		"title change":     {"\x1b]0;pwned\x1b\\", "^[]0;pwned^[\\"},
		"bell and del":     {"\x07\x7f", "^G^?"},
		"nul":              {"a\x00b", "a^@b"},
		"c1 csi":           {"\u009b31m", `\u009b31m`},
		"bidi override":    {"abc\u202edcba", `abc\u202edcba`},
		"invalid utf8":     {"a\xffb", `a\xffb`},
		"utf8 kept":        {"Grüße 🚀", "Grüße 🚀"},
		"replacement char": {"\ufffd", "\ufffd"},
	}
	for name, tt := range tests {
		var out strings.Builder
		if err := writeSafe(&out, strings.NewReader(tt.in)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if out.String() != tt.want {
			t.Errorf("%s: got %q, want %q", name, out.String(), tt.want)
		}
	}
}

func TestWriteSafeLeavesNoRawEscapes(t *testing.T) {
	var b strings.Builder
	for i := range 0x100 {
		b.WriteByte(byte(i))
	}
	var out strings.Builder
	if err := writeSafe(&out, strings.NewReader(b.String())); err != nil {
		t.Fatal(err)
	}
	for _, r := range out.String() {
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			t.Fatalf("raw control character %U survived", r)
		}
	}
}

func TestSafeStringIsOneLine(t *testing.T) {
	got := safeString("bad\njob-2  hello  succeeded\tx\x1b[2J")
	if strings.ContainsAny(got, "\n\t\x1b") || got != `bad\njob-2  hello  succeeded\tx^[[2J` {
		t.Errorf("safeString = %q", got)
	}
}
