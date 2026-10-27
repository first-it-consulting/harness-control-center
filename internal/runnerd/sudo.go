package runnerd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/first-it-consulting/harness-control-center/internal/safetext"
)

// Errors of SudoSSH.
var (
	// ErrSudoPassword: sudo on the host needs the user's password.
	ErrSudoPassword = errors.New("sudo on the runner's host needs your password")
	// ErrSudoRefused: sudo did not accept the password.
	ErrSudoRefused = errors.New("sudo on the runner's host did not accept the password")
)

// The words of the hcc command run as root. They go through the remote
// shell, so only shell-inert characters are allowed.
var adminArgRe = regexp.MustCompile(`^[A-Za-z0-9._/=-]{1,128}$`)

// refusedExit is the remote script's exit status when sudo refused the
// password.
const refusedExit = 97

// SudoSSH runs "hcc ARGS..." as root on host, over the user's own ssh
// (ADR-0017), with input on its stdin, and returns its output. Without a
// password it asks sudo not to prompt (ErrSudoPassword if it would).
// With one, the remote shell reads exactly its first line, has sudo
// check it, and only then runs the command, which reads the rest: a
// wrong password never makes sudo read the input as another attempt.
// The password is kept nowhere and never appears on a command line.
func SudoSSH(ctx context.Context, host string, password []byte, args []string, input []byte) ([]byte, error) {
	if !sshHostRe.MatchString(host) {
		return nil, fmt.Errorf("runnerd: invalid ssh host %q", host)
	}
	for _, a := range args {
		if !adminArgRe.MatchString(a) {
			return nil, fmt.Errorf("runnerd: invalid argument %q", a)
		}
	}
	hcc := "sudo -n -- hcc " + strings.Join(args, " ")
	var stdin []byte
	script := "exec " + hcc
	if password != nil {
		if len(password) == 0 || len(password) > 1024 || bytes.ContainsAny(password, "\r\n\x00") {
			return nil, errors.New("runnerd: invalid password")
		}
		// printf is a shell builtin: the password is in no process's
		// arguments. The script has no single quotes (see below).
		script = `IFS= read -r p; printf "%s\n" "$p" | sudo -S -p "" -v 2>/dev/null || exit ` + fmt.Sprint(refusedExit) + `; p=; ` + hcc
		stdin = append(append(stdin, password...), '\n')
	}
	stdin = append(stdin, input...)
	// Our copy of the password (and the input) is wiped when done.
	defer clear(stdin)
	// The user's login shell may not be sh: run the script with sh.
	//nolint:gosec // host and args are validated above; the script is ours; "--" ends ssh's options
	cmd := exec.CommandContext(ctx, "ssh", "-T", "-o", "BatchMode=yes", "--", host, "sh -c '"+script+"'")
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}
	var exit *exec.ExitError
	msg := safetext.Line(strings.TrimSpace(stderr.String()))
	switch {
	case errors.As(err, &exit) && exit.ExitCode() == refusedExit:
		return nil, ErrSudoRefused
	case strings.Contains(msg, "password is required"):
		return nil, ErrSudoPassword
	case msg != "":
		return nil, errors.New(msg)
	}
	return nil, fmt.Errorf("runnerd: ssh %s: %w", host, err)
}
