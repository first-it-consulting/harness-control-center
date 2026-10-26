package plugins

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const manifest = `apiVersion: hcc/v1alpha1
kind: Plugin
metadata: {name: webhook, version: 0.1.0}
spec:
  type: notifier
  command: [./notify.sh]
  events: [job.ended]
  permissions:
    network: {egress: [{host: hooks.example.com}]}
`

func pluginDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil { //nolint:gosec // test directory
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestInstallListRemove(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes")
	}
	b, err := Pack(pluginDir(t, map[string]string{ManifestName: manifest, "notify.sh": "#!/bin/sh\ncat\n", "lib/util.txt": "x"}))
	if err != nil {
		t.Fatal(err)
	}
	_, _, pv, err := Check(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if pv.Name != "webhook" || pv.Type != "notifier" || pv.Files != 3 || len(pv.SHA256) != 64 || len(pv.Explanation) == 0 {
		t.Errorf("preview = %+v", pv)
	}
	s := NewStore(filepath.Join(t.TempDir(), "plugins"))
	if _, err := s.Install(bytes.NewReader(b), strings.Repeat("0", 64)); !errors.Is(err, ErrMismatch) {
		t.Errorf("install with another checksum = %v", err)
	}
	if _, err := s.Install(bytes.NewReader(b), pv.SHA256); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Install(bytes.NewReader(b), pv.SHA256); !errors.Is(err, ErrInstalled) {
		t.Errorf("second install = %v", err)
	}
	list, err := s.List()
	if err != nil || len(list) != 1 || list[0].SHA256 != pv.SHA256 {
		t.Fatalf("list = %v, %v", list, err)
	}
	_, p, bundle, err := s.Get("webhook")
	if err != nil || p.Spec.Command[0] != "./notify.sh" || !bytes.Equal(bundle[:10], mustClean(t, b)[:10]) {
		t.Fatalf("get: %v", err)
	}
	// A bundle changed behind the store's back is refused.
	if err := os.WriteFile(filepath.Join(s.dir, "webhook", "bundle.tar"), append(bundle, 0), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Get("webhook"); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Errorf("tampered bundle: %v", err)
	}
	if err := s.Remove("webhook"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("webhook"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second remove = %v", err)
	}
	if _, _, _, err := s.Get("../x"); err == nil {
		t.Error("a path was accepted as a name")
	}
}

func mustClean(t *testing.T, b []byte) []byte {
	t.Helper()
	clean, _, _, err := Check(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return clean
}

func TestCheckRejects(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes")
	}
	for name, files := range map[string]map[string]string{
		"no manifest":       {"notify.sh": "x"},
		"manifest deeper":   {"sub/" + ManifestName: manifest, "notify.sh": "x"},
		"missing command":   {ManifestName: manifest},
		"not executable":    {ManifestName: manifest, "notify.txt": "x", "notify.sh.txt": "x"},
		"invalid manifest":  {ManifestName: strings.Replace(manifest, "notifier", "rootkit", 1), "notify.sh": "x"},
		"command not exec.": {ManifestName: strings.Replace(manifest, "./notify.sh", "./notify.txt", 1), "notify.txt": "x"},
	} {
		b, err := Pack(pluginDir(t, files))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := Check(bytes.NewReader(b)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPackRefusesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	dir := pluginDir(t, map[string]string{ManifestName: manifest, "notify.sh": "x"})
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "passwd")); err != nil {
		t.Fatal(err)
	}
	if _, err := Pack(dir); err == nil || !strings.Contains(err.Error(), "not a plain file") {
		t.Errorf("Pack = %v", err)
	}
}
