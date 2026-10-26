//go:build darwin && cgo

package vz

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	vzf "github.com/Code-Hex/vz/v3"
)

// machine is a VM of Apple's Virtualization framework. Its only devices
// are the ones configured here: virtio block devices, one virtio console,
// an entropy source and, with network access, one vsock device. No
// network, no shared directories, no graphics, no USB.
type machine struct {
	vm      *vzf.VirtualMachine
	console *os.File // the read end of the console pipe
	done    chan struct{}

	mu  sync.Mutex
	err error
}

func newMachine(mc MachineConfig) (Machine, error) {
	boot, err := vzf.NewLinuxBootLoader(mc.Kernel, vzf.WithCommandLine(mc.CmdLine))
	if err != nil {
		return nil, err
	}
	cfg, err := vzf.NewVirtualMachineConfiguration(boot, uint(mc.CPUs), uint64(mc.MemoryMiB)<<20) //nolint:gosec // validated sizes
	if err != nil {
		return nil, err
	}
	platform, err := vzf.NewGenericPlatformConfiguration()
	if err != nil {
		return nil, err
	}
	cfg.SetPlatformVirtualMachineConfiguration(platform)

	var disks []vzf.StorageDeviceConfiguration
	for _, d := range mc.Disks {
		// Writes reach the file before the VM counts as stopped: the
		// runner reads the results disk right after.
		att, err := vzf.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(d.Path, d.ReadOnly,
			vzf.DiskImageCachingModeAutomatic, vzf.DiskImageSynchronizationModeFull)
		if err != nil {
			return nil, fmt.Errorf("disk %s: %w", d.Path, err)
		}
		dev, err := vzf.NewVirtioBlockDeviceConfiguration(att)
		if err != nil {
			return nil, err
		}
		disks = append(disks, dev)
	}
	cfg.SetStorageDevicesVirtualMachineConfiguration(disks)

	// The console: the guest writes into a pipe the provider reads; it
	// reads from /dev/null (nobody types into a sandbox).
	null, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		_ = null.Close()
		return nil, err
	}
	att, err := vzf.NewFileHandleSerialPortAttachment(null, w)
	if err != nil {
		_ = errors.Join(null.Close(), r.Close(), w.Close())
		return nil, err
	}
	serial, err := vzf.NewVirtioConsoleDeviceSerialPortConfiguration(att)
	if err != nil {
		_ = errors.Join(null.Close(), r.Close(), w.Close())
		return nil, err
	}
	cfg.SetSerialPortsVirtualMachineConfiguration([]*vzf.VirtioConsoleDeviceSerialPortConfiguration{serial})

	entropy, err := vzf.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		_ = errors.Join(null.Close(), r.Close(), w.Close())
		return nil, err
	}
	cfg.SetEntropyDevicesVirtualMachineConfiguration([]*vzf.VirtioEntropyDeviceConfiguration{entropy})
	if mc.Vsock {
		sock, err := vzf.NewVirtioSocketDeviceConfiguration()
		if err != nil {
			_ = errors.Join(null.Close(), r.Close(), w.Close())
			return nil, err
		}
		cfg.SetSocketDevicesVirtualMachineConfiguration([]vzf.SocketDeviceConfiguration{sock})
	}
	if ok, err := cfg.Validate(); !ok || err != nil {
		_ = errors.Join(null.Close(), r.Close(), w.Close())
		return nil, fmt.Errorf("invalid VM configuration: %w", err)
	}
	vm, err := vzf.NewVirtualMachine(cfg)
	if err != nil {
		_ = errors.Join(null.Close(), r.Close(), w.Close())
		return nil, err
	}
	m := &machine{vm: vm, console: r, done: make(chan struct{})}
	// The framework holds its own references to the console's file
	// handles; ours can go once it has them.
	_ = null.Close()
	_ = w.Close()
	consoleDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(mc.Console, r)
		close(consoleDone)
	}()
	go m.watch(consoleDone)
	return m, nil
}

// watch closes done once the VM has stopped (or failed) and its console
// output is read.
func (m *machine) watch(consoleDone <-chan struct{}) {
loop:
	for state := range m.vm.StateChangedNotify() {
		switch state {
		case vzf.VirtualMachineStateStopped:
			break loop
		case vzf.VirtualMachineStateError:
			m.setErr(errors.New("the VM stopped with an error"))
			break loop
		}
	}
	_ = m.console.Close() // ends the console copy once the VM is gone
	<-consoleDone
	close(m.done)
}

func (m *machine) setErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

func (m *machine) Start() error { return m.vm.Start() }

func (m *machine) Done() <-chan struct{} { return m.done }

func (m *machine) Err() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.err
}

func (m *machine) Stop() error {
	if !m.vm.CanStop() {
		return nil // already stopped or stopping
	}
	return m.vm.Stop()
}

func (m *machine) Listen(port uint32) (net.Listener, error) {
	devs := m.vm.SocketDevices()
	if len(devs) != 1 {
		return nil, errors.New("the VM has no vsock device")
	}
	return devs[0].Listen(port)
}
