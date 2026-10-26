//go:build !darwin || !cgo

package vz

import "errors"

// newMachine needs Apple's Virtualization framework (macOS, built with
// cgo).
func newMachine(MachineConfig) (Machine, error) {
	return nil, errors.New("this build cannot run VMs: the vz provider needs macOS and a build with cgo")
}
