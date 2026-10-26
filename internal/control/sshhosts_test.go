package control

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestSSHHosts(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(filepath.Join(dir, "conf.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("config", `# comment
Include conf.d/*
Include ~/.ssh/extra
Host hcc-dev pve1
  User root
host=lower
HOST	tabbed
Host *.example.com !secret web-? *
Host -oProxyCommand=x
Include config
`)
	write("conf.d/one", "Host from-include\n")
	write("extra", "Host from-home\n")
	got := SSHHosts(filepath.Join(dir, "config"))
	want := []string{"from-home", "from-include", "hcc-dev", "lower", "pve1", "tabbed"}
	if !slices.Equal(got, want) {
		t.Errorf("SSHHosts = %q, want %q", got, want)
	}
	if got := SSHHosts(filepath.Join(dir, "missing")); len(got) != 0 {
		t.Errorf("missing config: %q", got)
	}
}

func TestSSHHostsAPI(t *testing.T) {
	ts := newTestServer(t)
	cfg := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(cfg, []byte("Host b a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ts.s.sshConfig = cfg
	var out struct{ Hosts []string }
	if code := ts.do(http.MethodGet, "/api/ssh-hosts", nil, &out); code != http.StatusOK || !slices.Equal(out.Hosts, []string{"a", "b"}) {
		t.Errorf("GET /api/ssh-hosts: %d %q", code, out.Hosts)
	}
	ts.s.sshConfig = filepath.Join(t.TempDir(), "none")
	if code := ts.do(http.MethodGet, "/api/ssh-hosts", nil, &out); code != http.StatusOK || len(out.Hosts) != 0 {
		t.Errorf("without a config: %d %q", code, out.Hosts)
	}
}
