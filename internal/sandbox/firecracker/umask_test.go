//go:build unix

package firecracker

import (
	"context"
	"os"
	"syscall"
	"testing"
)

// The runner daemon runs with umask 077 (systemd UMask=0077). The jail
// must still be readable by the jailed UID: modes must not depend on the
// umask. Regression test for "Unable to open or read from the
// configuration file: Permission denied" seen on the first real run.
func TestJailModesIgnoreUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)

	f := newFixture(t, exitsCleanly)
	var rootMode os.FileMode
	f.host.onStart = func(root string) {
		if fi, err := os.Stat(root); err == nil {
			rootMode = fi.Mode().Perm()
		}
	}
	if _, _, err := f.run(context.Background(), t); err != nil {
		t.Fatal(err)
	}
	if rootMode != 0o711 {
		t.Errorf("jail root mode = %v, want 0711", rootMode)
	}
	for name, mode := range f.host.jailSeen {
		if mode != 0o444 {
			t.Errorf("%s mode = %v under umask 077, want 0444", name, mode)
		}
	}
}
