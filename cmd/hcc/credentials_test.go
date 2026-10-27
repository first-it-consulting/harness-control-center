//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// credentialRunner writes a runner config naming an empty store.
func credentialRunner(t *testing.T) (config string) {
	t.Helper()
	dir := t.TempDir()
	store := filepath.Join(dir, "credentials.yaml")
	if err := os.WriteFile(store, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config = filepath.Join(dir, "runner.yaml")
	cfg := "target: local\nmaxConcurrent: 1\nrunnerUser: me\nsocket: " + dir + "/runner.sock\nstateDir: " + dir + "/state\nprovider: vz\n" +
		"vz: {stagingDir: " + dir + "/staging, imageRoot: " + dir + "/images}\n" +
		"images: {base: {kernel: " + dir + "/images/Image, rootfs: " + dir + "/images/base.ext4}}\n" +
		"network: {proxyPort: 3128}\ncredentials: " + store + "\n"
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return config
}

func TestCredentialsCommand(t *testing.T) {
	config := credentialRunner(t)
	req := `{"name":"omlx","hosts":["mac.fritz.box"],"scheme":"http","port":4000,"header":"Authorization","prefix":"Bearer ","secret":"sk-test-secret"}`
	if code, out, errOut := runCLIWithInput(req, "credentials", "set", "--config", config); code != exitOK || !strings.Contains(out, "Saved credential omlx") {
		t.Fatalf("set: %d %q %q", code, out, errOut)
	}
	code, out, _ := runCLI("credentials", "list", "--json", "--config", config)
	if code != exitOK || !strings.Contains(out, `"name":"omlx"`) || strings.Contains(out, "sk-test-secret") {
		t.Fatalf("list: %d %q", code, out)
	}
	// Changing where it goes keeps the secret.
	moved := `{"name":"omlx","hosts":["other.fritz.box"],"scheme":"http","port":4000,"header":"Authorization","prefix":"Bearer "}`
	if code, _, errOut := runCLIWithInput(moved, "credentials", "set", "--config", config); code != exitOK {
		t.Fatalf("move: %d %q", code, errOut)
	}
	if code, out, _ := runCLI("credentials", "list", "--config", config); code != exitOK || !strings.Contains(out, "other.fritz.box") {
		t.Errorf("after move: %q", out)
	}
	if code, _, _ := runCLI("credentials", "remove", "--config", config, "omlx"); code != exitOK {
		t.Errorf("remove: %d", code)
	}
}

func TestCredentialsCommandRefuses(t *testing.T) {
	config := credentialRunner(t)
	for name, req := range map[string]string{ //nolint:gosec // test requests, no real secret
		"unknown field":   `{"name":"x","hosts":["a.example"],"header":"x-api-key","secret":"s","secretFile":"/etc/shadow"}`,
		"not JSON":        `name: x`,
		"an IP as host":   `{"name":"x","hosts":["127.0.0.1"],"header":"x-api-key","secret":"s"}`,
		"no secret (new)": `{"name":"x","hosts":["a.example"],"header":"x-api-key"}`,
	} {
		if code, _, _ := runCLIWithInput(req, "credentials", "set", "--config", config); code != exitInvalid {
			t.Errorf("%s: exit %d", name, code)
		}
	}
	// A secret never goes on the command line.
	if code, _, _ := runCLI("credentials", "set", "--config", config, "omlx", "sk-secret"); code != exitUsage {
		t.Errorf("arguments accepted: %d", code)
	}
	// A runner without a store.
	noStore := filepath.Join(t.TempDir(), "runner.yaml")
	data, _ := os.ReadFile(config)
	cfg := strings.Split(string(data), "credentials:")[0]
	if err := os.WriteFile(noStore, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runCLI("credentials", "list", "--config", noStore); code != exitInvalid || !strings.Contains(errOut, "no credential store") {
		t.Errorf("no store: %d %q", code, errOut)
	}
}
