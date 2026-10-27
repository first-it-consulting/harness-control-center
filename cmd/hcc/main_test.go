package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/first-it-consulting/harness-control-center/internal/plugins"
)

func runCLI(args ...string) (code int, stdout, stderr string) {
	return runCLIWithInput("", args...)
}

func runCLIWithInput(input string, args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = run(args, strings.NewReader(input), &out, &errOut)
	return code, out.String(), errOut.String()
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "h.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The shipped examples are documentation; they must always be valid.
func TestExamplesAreValid(t *testing.T) {
	files, err := filepath.Glob("../../examples/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no examples found: %v", err)
	}
	for _, f := range files {
		if code, _, stderr := runCLI("validate", f); code != exitOK {
			t.Errorf("%s: exit %d\n%s", f, code, stderr)
		}
	}
}

// So must the example plugins (as bundles, like an install sees them).
func TestExamplePluginsAreValid(t *testing.T) {
	dirs, err := filepath.Glob("../../examples/plugins/*")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no example plugins found: %v", err)
	}
	for _, d := range dirs {
		b, err := plugins.Pack(d)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := plugins.Check(bytes.NewReader(b)); err != nil {
			t.Errorf("%s: %v", d, err)
		}
	}
}

func TestExplain(t *testing.T) {
	code, out, _ := runCLI("explain", "../../examples/hello-offline.yaml")
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"Risk: low", "no network access"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestValidateListsEveryProblem(t *testing.T) {
	p := writeFile(t, "apiVersion: hcc/v1alpha1\nkind: Harness\nmetadata: {name: x}\nspec: {runtime: claude-code, prompt: hi, target: moon, sandbox: {cpus: 99}}\n")
	code, _, stderr := runCLI("validate", p)
	if code != exitInvalid {
		t.Fatalf("exit %d, want %d", code, exitInvalid)
	}
	if n := strings.Count(stderr, "\n  - "); n != 2 {
		t.Errorf("want 2 listed problems, got %d:\n%s", n, stderr)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{}, {"bogus"}, {"validate"}, {"explain", "a", "b"}, {"local"}, {"local", "bogus"}} {
		if code, _, _ := runCLI(args...); code != exitUsage {
			t.Errorf("run(%q) = %d, want %d", args, code, exitUsage)
		}
	}
}

func TestMissingFile(t *testing.T) {
	if code, _, _ := runCLI("validate", filepath.Join(t.TempDir(), "nope.yaml")); code != exitInvalid {
		t.Errorf("exit %d, want %d", code, exitInvalid)
	}
}

func TestVersionAndHelp(t *testing.T) {
	if code, out, _ := runCLI("version"); code != exitOK || !strings.HasPrefix(out, "hcc dev") {
		t.Errorf("version: %d %q", code, out)
	}
	if code, out, _ := runCLI("help"); code != exitOK || !strings.Contains(out, "explain") {
		t.Errorf("help: %d %q", code, out)
	}
}

func TestLocalImagesInADevelopmentBuild(t *testing.T) {
	code, _, stderr := runCLI("local", "images")
	if code != exitInvalid || !strings.Contains(stderr, "build-images.sh") {
		t.Errorf("exit %d: %q", code, stderr)
	}
}
