//go:build unix

package control

import (
	"bytes"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// grantedHarness is okHarness granted the credential "llm" for its host.
var grantedHarness = strings.Replace(okHarness, "  prompt: hi\n", `  prompt: hi
  permissions:
    credentials: [llm]
    network: {egress: [{host: llm.example}]}
`, 1)

func TestCredentialStoreAndCopies(t *testing.T) {
	ts := newTestServer(t)
	var logs bytes.Buffer
	ts.s.log = slog.New(slog.NewTextHandler(&logs, nil))
	sock := startRunner(t)
	if code := ts.do(http.MethodPost, "/api/runners", RunnerRef{Name: "mac", Socket: sock}, nil); code != http.StatusCreated {
		t.Fatalf("add runner: %d", code)
	}
	const secret = "sk-store-0123456789abcdef"
	body := map[string]any{"hosts": []string{"llm.example"}, "scheme": "https", "port": 443, "header": "Authorization", "prefix": "Bearer ", "secret": secret}
	if rec := ts.rawJSON(http.MethodPut, "/api/credentials/llm", body); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	// A new credential needs a secret.
	noSecret := map[string]any{"hosts": []string{"x.example"}, "scheme": "https", "port": 443, "header": "x-api-key"}
	if rec := ts.rawJSON(http.MethodPut, "/api/credentials/other", noSecret); rec.Code != http.StatusBadRequest {
		t.Errorf("new without a secret: %d", rec.Code)
	}
	store, secrets, _ := ts.s.storePaths()
	for path, want := range map[string]os.FileMode{filepath.Dir(store): 0o700, store: 0o600} {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: %v %v, want %v", path, fi.Mode(), err, want)
		}
	}
	if files, _ := os.ReadDir(secrets); len(files) != 1 {
		t.Errorf("secret files: %v", files)
	}

	list := func() []CredentialView {
		t.Helper()
		rec := ts.rawJSON(http.MethodGet, "/api/credentials", nil)
		if strings.Contains(rec.Body.String(), secret) || strings.Contains(rec.Body.String(), "secrets/") {
			t.Fatalf("the list shows the secret: %s", rec.Body.String())
		}
		var out struct{ Credentials []CredentialView }
		ts.do(http.MethodGet, "/api/credentials", nil, &out)
		return out.Credentials
	}
	if l := list(); len(l) != 1 || l[0].Name != "llm" || len(l[0].Runners) != 0 {
		t.Fatalf("list: %+v", l)
	}

	// Running a harness granted it copies it to the runner first. (This
	// test runner has no network, so it then refuses the job itself.)
	runnerStore := filepath.Join(filepath.Dir(sock), "credentials.yaml")
	var out map[string]any
	if code := ts.do(http.MethodPost, "/api/runs", runRequest{Runner: "mac", YAML: grantedHarness, Confirmed: true}, &out); code == http.StatusForbidden {
		t.Fatalf("run: %d %v", code, out)
	}
	first, _ := os.ReadFile(runnerStore)
	if !strings.Contains(string(first), "llm.example") {
		t.Fatalf("not copied:\n%s", first)
	}
	if l := list(); l[0].Runners["mac"] != "current" {
		t.Errorf("after the copy: %+v", l[0].Runners)
	}
	// Unchanged: not copied again (a copy writes a new secret file).
	ts.do(http.MethodPost, "/api/runs", runRequest{Runner: "mac", YAML: grantedHarness, Confirmed: true}, nil)
	if again, _ := os.ReadFile(runnerStore); string(again) != string(first) {
		t.Errorf("copied again although unchanged")
	}
	// A new secret makes the runner's copy outdated, and the next run
	// copies it.
	body["secret"] = "sk-store-new-secret-456"
	ts.rawJSON(http.MethodPut, "/api/credentials/llm", body)
	if l := list(); l[0].Runners["mac"] != "outdated" {
		t.Errorf("after a change: %+v", l[0].Runners)
	}
	ts.do(http.MethodPost, "/api/runs", runRequest{Runner: "mac", YAML: grantedHarness, Confirmed: true}, nil)
	if l := list(); l[0].Runners["mac"] != "current" {
		t.Errorf("after the next run: %+v", l[0].Runners)
	}
	// Copying by hand; unknown runners.
	if code := ts.do(http.MethodPost, "/api/credentials/llm/copy", copyRequest{Runner: "mac"}, nil); code != http.StatusOK {
		t.Errorf("copy: %d", code)
	}
	if code := ts.do(http.MethodPost, "/api/credentials/llm/copy", copyRequest{Runner: "nope"}, nil); code != http.StatusNotFound {
		t.Errorf("copy to an unknown runner: %d", code)
	}

	// A harness granted a credential the store does not hold is left to
	// the runner (which refuses it if it has none).
	other := strings.ReplaceAll(grantedHarness, "llm", "elsewhere")
	if code := ts.do(http.MethodPost, "/api/runs", runRequest{Runner: "mac", YAML: other, Confirmed: true}, nil); code == http.StatusCreated {
		t.Error("the runner accepted a credential nobody has")
	}

	if code := ts.do(http.MethodDelete, "/api/credentials/llm", nil, nil); code != http.StatusOK {
		t.Errorf("DELETE: %d", code)
	}
	if l := list(); len(l) != 0 {
		t.Errorf("after delete: %+v", l)
	}
	for _, s := range []string{secret, "sk-store-new-secret-456"} {
		if strings.Contains(logs.String(), s) {
			t.Fatalf("a secret is in the log:\n%s", logs.String())
		}
	}
	if !strings.Contains(logs.String(), "credential copied to runner") {
		t.Errorf("copies are not logged:\n%s", logs.String())
	}
}

// Over ssh, a copy runs "hcc credentials set" with sudo on the host, the
// secret on stdin; sudo's password is asked for when needed.
func TestCredentialCopyOverSSH(t *testing.T) {
	dir := t.TempDir()
	script := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o700); err != nil { //nolint:gosec // test programs must be executable
			t.Fatal(err)
		}
	}
	script("ssh", "while [ \"$1\" != -- ]; do shift; done\nshift 2\nexec sh -c \"$1\"\n")
	script("sudo", `case "$1" in
-S) IFS= read -r p; [ "$p" = pw ] && touch "$FAKE/validated" && exit 0; exit 1 ;;
-n) shift 2; [ -e "$FAKE/validated" ] || { echo "sudo: a password is required" >&2; exit 1; }; exec "$@" ;;
esac
exit 2
`)
	script("hcc", "printf '%s\\n' \"$@\" >\"$FAKE/args\"; cat >\"$FAKE/stdin\"\n")
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE", dir)

	ts := newTestServer(t)
	ts.do(http.MethodPost, "/api/runners", RunnerRef{Name: "srv", SSH: "server"}, nil)
	const secret = "ghp-0123456789abcdef"
	ts.rawJSON(http.MethodPut, "/api/credentials/github", map[string]any{"hosts": []string{"api.github.com"}, "scheme": "https", "port": 443, "header": "Authorization", "prefix": "Bearer ", "secret": secret})

	var out map[string]any
	if code := ts.do(http.MethodPost, "/api/credentials/github/copy", copyRequest{Runner: "srv"}, &out); code != http.StatusForbidden || out["needPassword"] != true {
		t.Fatalf("without a password: %d %v", code, out)
	}
	pw := "pw"
	if code := ts.do(http.MethodPost, "/api/credentials/github/copy", copyRequest{Runner: "srv", SudoPassword: &pw}, &out); code != http.StatusOK {
		t.Fatalf("with the password: %d %v", code, out)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if got := strings.Join(strings.Fields(string(args)), " "); got != "credentials set --config /etc/hcc/runner.yaml" {
		t.Errorf("args = %q", got)
	}
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	if !strings.Contains(string(stdin), secret) || !strings.Contains(string(stdin), `"name":"github"`) {
		t.Errorf("stdin = %s", stdin)
	}
	if strings.Contains(string(args), secret) {
		t.Error("the secret is on the command line")
	}
}
