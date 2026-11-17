//go:build unix

package credentials

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

func emptyStore(t *testing.T, mode os.FileMode) (path, secrets string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "credentials.yaml")
	if err := os.WriteFile(path, []byte("{}\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path, filepath.Join(dir, "secrets")
}

func secretFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

var omlx = Entry{Name: "omlx", Hosts: []string{"mac.fritz.box"}, Scheme: "http", Port: 4000, Header: "Authorization", Prefix: "Bearer "}

func TestSetAddsACredential(t *testing.T) {
	path, secrets := emptyStore(t, 0o640)
	if err := Set(path, secrets, omlx, []byte(testSecret+"\n")); err != nil {
		t.Fatal(err)
	}
	creds, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	c := creds["omlx"]
	if c.Value != "Bearer "+testSecret || c.Port != 4000 || c.Scheme != "http" {
		t.Errorf("got %v", c)
	}
	// Secret files copy the store's mode; the directory is searchable by
	// the store's group, nothing for others.
	files := secretFiles(t, secrets)
	if len(files) != 1 {
		t.Fatalf("secret files %v", files)
	}
	if fi, _ := os.Stat(filepath.Join(secrets, files[0])); fi.Mode().Perm() != 0o640 {
		t.Errorf("secret mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(secrets); fi.Mode().Perm() != 0o750 {
		t.Errorf("secrets dir mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Errorf("store mode %v", fi.Mode().Perm())
	}
}

func TestListNeverShowsTheSecret(t *testing.T) {
	path, secrets := emptyStore(t, 0o600)
	if err := Set(path, secrets, omlx, []byte(testSecret)); err != nil {
		t.Fatal(err)
	}
	list, err := List(path)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(list)
	if strings.Contains(string(out), testSecret) || strings.Contains(string(out), "secrets") {
		t.Errorf("List shows the secret or its file: %s", out)
	}
	if len(list) != 1 || list[0].Name != "omlx" || list[0].Prefix != "Bearer " || list[0].Hosts[0] != "mac.fritz.box" {
		t.Errorf("got %+v", list)
	}
}

func TestSetWithoutASecretKeepsIt(t *testing.T) {
	path, secrets := emptyStore(t, 0o600)
	if err := Set(path, secrets, omlx, []byte(testSecret)); err != nil {
		t.Fatal(err)
	}
	moved := omlx
	moved.Hosts = []string{"other.fritz.box"}
	if err := Set(path, secrets, moved, nil); err != nil {
		t.Fatal(err)
	}
	creds, _ := Load(path)
	if c := creds["omlx"]; c.Value != "Bearer "+testSecret || c.Hosts[0] != "other.fritz.box" {
		t.Errorf("got %v", c)
	}
	// A new credential needs a secret.
	fresh := omlx
	fresh.Name = "new"
	if err := Set(path, secrets, fresh, nil); err == nil {
		t.Error("a new credential without a secret was accepted")
	}
}

func TestReplacingASecretRemovesTheOldFile(t *testing.T) {
	path, secrets := emptyStore(t, 0o600)
	if err := Set(path, secrets, omlx, []byte(testSecret)); err != nil {
		t.Fatal(err)
	}
	before := secretFiles(t, secrets)
	if err := Set(path, secrets, omlx, []byte("sk-rotated")); err != nil {
		t.Fatal(err)
	}
	after := secretFiles(t, secrets)
	if len(after) != 1 || after[0] == before[0] {
		t.Errorf("secret files before %v, after %v", before, after)
	}
	creds, _ := Load(path)
	if creds["omlx"].Value != "Bearer sk-rotated" {
		t.Error("not rotated")
	}
}

func TestInvalidChangesChangeNothing(t *testing.T) {
	path, secrets := emptyStore(t, 0o600)
	if err := Set(path, secrets, omlx, []byte(testSecret)); err != nil {
		t.Fatal(err)
	}
	store, _ := os.ReadFile(path)
	files := secretFiles(t, secrets)
	bad := func(name string, e Entry, secret []byte) {
		t.Helper()
		if err := Set(path, secrets, e, secret); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if now, _ := os.ReadFile(path); string(now) != string(store) {
			t.Errorf("%s: the store changed", name)
		}
		if now := secretFiles(t, secrets); strings.Join(now, ",") != strings.Join(files, ",") {
			t.Errorf("%s: secret files %v, want %v", name, now, files)
		}
	}
	e := omlx
	e.Hosts = []string{"10.0.0.1"} // names only
	bad("an IP host", e, []byte("sk-x"))
	e = omlx
	e.Hosts = []string{"*.example.com"}
	bad("a wildcard", e, []byte("sk-x"))
	e = omlx
	e.Header = "Host"
	bad("a reserved header", e, []byte("sk-x"))
	e = omlx
	e.Prefix = "Bearer \r\nX-Evil: 1 "
	bad("a prefix with a newline", e, []byte("sk-x"))
	bad("a secret with a newline", omlx, []byte("sk-x\nX-Evil: 1"))
	bad("an empty secret", omlx, []byte("  \n"))
	bad("a huge secret", omlx, []byte(strings.Repeat("x", MaxSecretBytes+1)))
	e = omlx
	e.Name = "../etc"
	bad("a path as name", e, []byte("sk-x"))
	e = omlx
	e.Scheme = "ftp"
	bad("another scheme", e, []byte("sk-x"))
}

func TestRemove(t *testing.T) {
	path, secrets := emptyStore(t, 0o600)
	if err := Set(path, secrets, omlx, []byte(testSecret)); err != nil {
		t.Fatal(err)
	}
	if err := Remove(path, secrets, "omlx"); err != nil {
		t.Fatal(err)
	}
	if creds, err := Load(path); err != nil || len(creds) != 0 {
		t.Errorf("after remove: %v %v", creds, err)
	}
	if files := secretFiles(t, secrets); len(files) != 0 {
		t.Errorf("secret left: %v", files)
	}
	if err := Remove(path, secrets, "omlx"); err == nil {
		t.Error("removed twice")
	}
}

func TestRemoveLeavesForeignSecretFiles(t *testing.T) {
	s := newStore(t)
	own := s.secret(t, "hand-made", testSecret, 0o600)
	path := s.file(t, "mine:\n  hosts: [api.example.com]\n  header: x-api-key\n  secretFile: "+own+"\n")
	if err := Remove(path, filepath.Join(s.dir, "secrets"), "mine"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(own); err != nil {
		t.Error("a secret file outside the secrets directory was removed")
	}
}

func TestEditRefusesAPublicStore(t *testing.T) {
	path, secrets := emptyStore(t, 0o644)
	if err := Set(path, secrets, omlx, []byte(testSecret)); err == nil {
		t.Error("wrote secrets next to a world-readable store")
	}
}

const kubeconfig = "apiVersion: v1\nkind: Config\nusers:\n- name: me\n  user: {token: abc123}\n"

func TestSecretsForTheAgent(t *testing.T) {
	path, secrets := emptyStore(t, 0o640)
	kube := Entry{Name: "kubeconfig", Kind: KindSecret}
	if err := Set(path, secrets, kube, []byte(kubeconfig)); err != nil {
		t.Fatal(err)
	}
	if err := Set(path, secrets, omlx, []byte(testSecret)); err != nil {
		t.Fatal(err)
	}
	store, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c := store["kubeconfig"]; c.Kind != KindSecret || c.Value != kubeconfig || len(c.Hosts) != 0 {
		t.Errorf("loaded %+v", c)
	}
	list, _ := List(path)
	if len(list) != 2 || list[0].Kind != KindSecret || list[0].Hosts == nil {
		t.Errorf("list = %+v", list)
	}

	// Handed only as a secret, never a web credential (and the other way
	// round).
	h := &spec.Harness{Spec: spec.Spec{Permissions: spec.Permissions{Secrets: []spec.Secret{{Name: "kubeconfig", File: ".kube/config"}}}}}
	got, err := ForAgent(store, h)
	if err != nil || got["kubeconfig"] != kubeconfig {
		t.Errorf("ForAgent = %v, %v", got, err)
	}
	h.Spec.Permissions.Secrets = []spec.Secret{{Name: "omlx", Env: "TOKEN"}}
	if _, err := ForAgent(store, h); err == nil || !strings.Contains(err.Error(), "never handed to an agent") {
		t.Errorf("a web credential was handed out: %v", err)
	}
	h.Spec.Permissions.Secrets = []spec.Secret{{Name: "nope", Env: "TOKEN"}}
	if _, err := ForAgent(store, h); err == nil {
		t.Error("a missing secret")
	}
	h.Spec.Permissions.Secrets = nil
	h.Spec.Permissions.Credentials = []string{"kubeconfig"}
	if _, err := ForHarness(store, h, func(string, int) bool { return true }); err == nil || !strings.Contains(err.Error(), "permissions.secrets") {
		t.Errorf("a secret used as a web credential: %v", err)
	}

	// Changing the kind needs a new secret.
	asSecret := omlx
	asSecret.Kind, asSecret.Hosts, asSecret.Scheme, asSecret.Port, asSecret.Header, asSecret.Prefix = KindSecret, nil, "", 0, "", ""
	if err := Set(path, secrets, asSecret, nil); err == nil || !strings.Contains(err.Error(), "needs a new secret") {
		t.Errorf("kind changed without a new secret: %v", err)
	}
	// A secret has no hosts; too large or not text is refused.
	withHosts := kube
	withHosts.Hosts = []string{"a.example"}
	if err := Set(path, secrets, withHosts, []byte("x")); err == nil {
		t.Error("a secret with hosts")
	}
	if err := Set(path, secrets, kube, make([]byte, MaxAgentSecretBytes+1)); err == nil {
		t.Error("too large")
	}
	if err := Set(path, secrets, kube, []byte("a\x00b")); err == nil {
		t.Error("not text")
	}
}
