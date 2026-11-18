package runnerd

import (
	"strings"
	"testing"
)

func TestSSHArgs(t *testing.T) {
	got := strings.Join(SSHArgs("-T", "--", "host", "cmd"), " ")
	if got != "-o BatchMode=yes -o ConnectTimeout=15 -T -- host cmd" {
		t.Errorf("ssh args: %s", got)
	}
	// Callers' arguments never change the shared options.
	a := SSHArgs("x")
	a[1] = "changed"
	if b := SSHArgs(); b[1] != "BatchMode=yes" {
		t.Errorf("shared options changed: %v", b)
	}
}
