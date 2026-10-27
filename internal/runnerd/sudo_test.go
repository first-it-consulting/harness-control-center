//go:build unix

package runnerd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHost puts ssh, sudo and hcc on PATH: ssh runs the remote command
// here, sudo wants the password "pw" when needPassword is set, and hcc
// records its arguments and stdin.
func fakeHost(t *testing.T, needPassword bool) (dir string) {
	t.Helper()
	dir = t.TempDir()
	write := func(name, script string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o700); err != nil { //nolint:gosec // test programs must be executable
			t.Fatal(err)
		}
	}
	write("ssh", `printf '%s\n' "$@" >"$FAKE/ssh-args"
while [ "$1" != "--" ]; do shift; done
shift 2
exec sh -c "$1"
`)
	need := "0"
	if needPassword {
		need = "1"
	}
	write("sudo", `case "$1" in
-S) IFS= read -r p; [ "$p" = pw ] && touch "$FAKE/validated" && exit 0; exit 1 ;;
-n) shift 2
    if [ `+need+` = 1 ] && [ ! -e "$FAKE/validated" ]; then echo "sudo: a password is required" >&2; exit 1; fi
    exec "$@" ;;
esac
exit 2
`)
	write("hcc", `printf '%s\n' "$@" >"$FAKE/hcc-args"; cat >"$FAKE/hcc-stdin"; echo done
`)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE", dir)
	return dir
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func TestSudoSSHWithoutAPassword(t *testing.T) {
	dir := fakeHost(t, false)
	out, err := SudoSSH(context.Background(), "server", nil, []string{"credentials", "set", "--config", "/etc/hcc/runner.yaml"}, []byte(`{"secret":"sk"}`))
	if err != nil || strings.TrimSpace(string(out)) != "done" {
		t.Fatalf("%q %v", out, err)
	}
	if got := read(t, filepath.Join(dir, "hcc-args")); got != "credentials\nset\n--config\n/etc/hcc/runner.yaml\n" {
		t.Errorf("hcc args %q", got)
	}
	if got := read(t, filepath.Join(dir, "hcc-stdin")); got != `{"secret":"sk"}` {
		t.Errorf("hcc stdin %q", got)
	}
}

func TestSudoSSHAsksForThePassword(t *testing.T) {
	fakeHost(t, true)
	if _, err := SudoSSH(context.Background(), "server", nil, []string{"credentials", "list"}, nil); !errors.Is(err, ErrSudoPassword) {
		t.Fatalf("got %v", err)
	}
}

func TestSudoSSHWithThePassword(t *testing.T) {
	dir := fakeHost(t, true)
	input := `{"name":"x","secret":"sk-1"}` + "\n" + "second line"
	if _, err := SudoSSH(context.Background(), "server", []byte("pw"), []string{"credentials", "set"}, []byte(input)); err != nil {
		t.Fatal(err)
	}
	// hcc gets exactly the input, not the password; the password is on
	// no command line.
	if got := read(t, filepath.Join(dir, "hcc-stdin")); got != input {
		t.Errorf("hcc stdin %q", got)
	}
	if args := read(t, filepath.Join(dir, "ssh-args")); strings.Contains(args, "pw\n") || strings.Contains(args, "sk-1") {
		t.Errorf("a secret on the command line: %q", args)
	}
}

func TestSudoSSHWrongPassword(t *testing.T) {
	dir := fakeHost(t, true)
	_, err := SudoSSH(context.Background(), "server", []byte("wrong"), []string{"credentials", "set"}, []byte(`{"secret":"sk-1"}`))
	if !errors.Is(err, ErrSudoRefused) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "hcc-stdin")); err == nil {
		t.Error("hcc ran after sudo refused the password")
	}
}

func TestSudoSSHRefusesUnsafeInput(t *testing.T) {
	fakeHost(t, false)
	for _, c := range []struct {
		host string
		args []string
		pw   []byte
	}{
		{"-oProxyCommand=x", []string{"credentials"}, nil},
		{"server", []string{"credentials;", "id"}, nil},
		{"server", []string{"credentials", "$(id)"}, nil},
		{"server", []string{"it's"}, nil},
		{"server", []string{"credentials"}, []byte("pw\nsecond")},
		{"server", []string{"credentials"}, []byte{}},
	} {
		if _, err := SudoSSH(context.Background(), c.host, c.pw, c.args, nil); err == nil {
			t.Errorf("%q %q accepted", c.host, c.args)
		}
	}
}
