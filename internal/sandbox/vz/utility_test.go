package vz

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func utility(run func(m *fakeMachine), got *MachineConfig) Utility {
	return Utility{
		Kernel:    "/img/Image",
		Disks:     []Disk{{Path: "/img/base.ext4", ReadOnly: true}, {Path: "/tmp/out.ext4"}},
		BootArgs:  []string{"hcc.mode=test"},
		CPUs:      1,
		MemoryMiB: 512,
		Timeout:   time.Second,
		NewMachine: func(mc MachineConfig) (Machine, error) {
			*got = mc
			return &fakeMachine{mc: mc, run: run, done: make(chan struct{})}, nil
		},
	}
}

func TestUtilityRuns(t *testing.T) {
	var mc MachineConfig
	var console bytes.Buffer
	u := utility(func(m *fakeMachine) {
		_, _ = m.mc.Console.Write([]byte("HCC-OK\n"))
		m.exit(nil)
	}, &mc)
	u.Console = &console
	if err := RunUtility(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(mc.CmdLine, "hcc.shutdown=poweroff hcc.mode=test") || !strings.Contains(mc.CmdLine, " ro ") {
		t.Errorf("command line %q", mc.CmdLine)
	}
	if mc.Vsock || len(mc.Disks) != 2 || console.String() != "HCC-OK\n" {
		t.Errorf("machine %+v, console %q", mc, console.String())
	}
}

func TestUtilityTimesOut(t *testing.T) {
	var mc MachineConfig
	u := utility(func(*fakeMachine) {}, &mc) // never shuts down
	u.Timeout = 50 * time.Millisecond
	if err := RunUtility(context.Background(), u); err == nil || !strings.Contains(err.Error(), "in time") {
		t.Fatalf("got %v", err)
	}
}

func TestUtilityCancelled(t *testing.T) {
	var mc MachineConfig
	ctx, cancel := context.WithCancel(context.Background())
	u := utility(func(*fakeMachine) { cancel() }, &mc)
	if err := RunUtility(ctx, u); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestUtilityVMError(t *testing.T) {
	var mc MachineConfig
	u := utility(func(m *fakeMachine) { m.exit(errors.New("boom")) }, &mc)
	if err := RunUtility(context.Background(), u); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("got %v", err)
	}
}

func TestUtilityNeedsReadOnlyRootAndTimeout(t *testing.T) {
	var mc MachineConfig
	u := utility(func(m *fakeMachine) { m.exit(nil) }, &mc)
	u.Disks[0].ReadOnly = false
	if err := RunUtility(context.Background(), u); err == nil {
		t.Error("writable root accepted")
	}
	u = utility(func(m *fakeMachine) { m.exit(nil) }, &mc)
	u.Timeout = 0
	if err := RunUtility(context.Background(), u); err == nil {
		t.Error("no timeout accepted")
	}
}
