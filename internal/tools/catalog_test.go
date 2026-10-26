package tools

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

const list = `# name version file...
python3 3.12.8-r1 python3-3.12.8-r1.apk libffi-3.4.6-r0.apk
py3-pip 24.3.1-r0 py3-pip-24.3.1-r0.apk python3-3.12.8-r1.apk libffi-3.4.6-r0.apk

make 4.4.1-r2 make-4.4.1-r2.apk
`

func TestParseAndResolve(t *testing.T) {
	c, err := Parse(strings.NewReader(list))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.Packages(), []string{"make@4.4.1-r2", "py3-pip@24.3.1-r0", "python3@3.12.8-r1"}; !slices.Equal(got, want) {
		t.Fatalf("Packages = %v, want %v", got, want)
	}
	pkgs, files, err := c.Resolve([]string{"py3-pip", "python3@3.12.8-r1", "python3"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"py3-pip", "python3"}; !slices.Equal(pkgs, want) {
		t.Errorf("pkgs = %v, want %v", pkgs, want)
	}
	if want := []string{"libffi-3.4.6-r0.apk", "py3-pip-24.3.1-r0.apk", "python3-3.12.8-r1.apk"}; !slices.Equal(files, want) {
		t.Errorf("files = %v, want %v (each once, sorted)", files, want)
	}
}

func TestResolveRejects(t *testing.T) {
	c, err := Parse(strings.NewReader(list))
	if err != nil {
		t.Fatal(err)
	}
	for _, grants := range [][]string{
		{"nmap"},              // not offered
		{"python3@3.11"},      // other version
		{"make", "nmap"},      // one bad grant fails all
		{"make;rm -rf /"},     // malformed
		{"make@1;reboot"},     // malformed version
		{"--allow-untrusted"}, // option-like
	} {
		if _, _, err := c.Resolve(grants); err == nil {
			t.Errorf("Resolve(%q) succeeded", grants)
		}
	}
	var none *Catalog // an image without a catalog
	if _, _, err := none.Resolve([]string{"make"}); err == nil {
		t.Error("nil catalog resolved a tool")
	}
	if got := none.Packages(); got != nil {
		t.Errorf("nil catalog offers %v", got)
	}
}

func TestParseRejects(t *testing.T) {
	for name, in := range map[string]string{
		"too few fields":  "make 4.4.1-r2\n",
		"bad name":        "Make 1 make-1.apk\n",
		"bad version":     "make 1;x make-1.apk\n",
		"path in file":    "make 1 ../make-1.apk\n",
		"slash in file":   "make 1 sub/make-1.apk\n",
		"option file":     "make 1 -make.apk\n",
		"not an apk":      "make 1 make-1.tar\n",
		"duplicate":       "make 1 make-1.apk\nmake 2 make-2.apk\n",
		"too long a line": "make 1 " + strings.Repeat("a", MaxListBytes) + ".apk\n",
	} {
		if _, err := Parse(strings.NewReader(in)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestParseLimits(t *testing.T) {
	var b strings.Builder
	for i := range MaxPackages + 1 {
		fmt.Fprintf(&b, "p%d 1 f.apk\n", i)
	}
	if _, err := Parse(strings.NewReader(b.String())); err == nil {
		t.Error("parsed more than MaxPackages packages")
	}
	files := strings.Repeat(" f.apk", MaxFilesPerPackage+1)
	if _, err := Parse(strings.NewReader("make 1" + files + "\n")); err == nil {
		t.Error("parsed more than MaxFilesPerPackage files")
	}
}

func FuzzParse(f *testing.F) {
	f.Add(list)
	f.Add("a 1 a.apk\n")
	f.Fuzz(func(t *testing.T, in string) {
		c, err := Parse(strings.NewReader(in))
		if err != nil {
			return
		}
		for _, p := range c.pkgs {
			for _, file := range p.Files {
				if !fileRe.MatchString(file) || strings.ContainsAny(file, "/ \t\n") {
					t.Fatalf("accepted file name %q", file)
				}
			}
		}
	})
}
