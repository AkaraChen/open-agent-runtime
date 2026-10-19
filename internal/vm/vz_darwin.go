//go:build darwin && cgo

package vm

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/Code-Hex/vz/v3"
)

// vzBackend runs the guest with Apple's Virtualization.framework. The
// binary must be signed with the com.apple.security.virtualization
// entitlement (see hack/sign-darwin.sh).
type vzBackend struct{}

func init() { Register(vzBackend{}) }

func (vzBackend) Name() string          { return "vz" }
func (vzBackend) NeedsGuestImage() bool { return true }
func (vzBackend) NeedsRoot() bool       { return false }

// Available is optimistic: hosts without hypervisor support fail in
// Validate/Start with a descriptive error.
func (vzBackend) Available() error { return nil }

type vzMachine struct {
	vm   *vz.VirtualMachine
	done chan struct{}
	once sync.Once
}

func (vzBackend) Boot(ctx context.Context, cfg MachineConfig) (Machine, error) {
	if cfg.Arch != "" && cfg.Arch != runtime.GOARCH {
		return nil, fmt.Errorf("vz can only run %s guests on this Mac", runtime.GOARCH)
	}
	console := "hvc0"
	boot, err := vz.NewLinuxBootLoader(cfg.Kernel,
		vz.WithCommandLine("console="+console+" panic=-1 quiet loglevel=3"),
		vz.WithInitrd(cfg.Initrd))
	if err != nil {
		return nil, fmt.Errorf("boot loader: %w", err)
	}
	vmCfg, err := vz.NewVirtualMachineConfiguration(boot, uint(cfg.CPUs), uint64(cfg.MemoryMiB)<<20)
	if err != nil {
		return nil, err
	}
	serial, err := vz.NewFileSerialPortAttachment(cfg.ConsoleLog, false)
	if err != nil {
		return nil, fmt.Errorf("serial: %w", err)
	}
	consoleCfg, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serial)
	if err != nil {
		return nil, err
	}
	vmCfg.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{consoleCfg})

	disk := filepath.Join(cfg.Dir, "data.img")
	if err := EnsureDisk(disk, cfg.DiskGiB); err != nil {
		return nil, err
	}
	diskAtt, err := vz.NewDiskImageStorageDeviceAttachment(disk, false)
	if err != nil {
		return nil, fmt.Errorf("disk: %w", err)
	}
	blk, err := vz.NewVirtioBlockDeviceConfiguration(diskAtt)
	if err != nil {
		return nil, err
	}
	vmCfg.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{blk})

	nat, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return nil, err
	}
	netCfg, err := vz.NewVirtioNetworkDeviceConfiguration(nat)
	if err != nil {
		return nil, err
	}
	if mac, err := vz.NewRandomLocallyAdministeredMACAddress(); err == nil {
		netCfg.SetMACAddress(mac)
	}
	vmCfg.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{netCfg})

	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, err
	}
	vmCfg.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})

	sock, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, err
	}
	vmCfg.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{sock})

	if ok, err := vmCfg.Validate(); !ok || err != nil {
		return nil, fmt.Errorf("invalid VM configuration: %v", err)
	}
	machine, err := vz.NewVirtualMachine(vmCfg)
	if err != nil {
		return nil, err
	}
	if err := machine.Start(); err != nil {
		return nil, fmt.Errorf("start VM: %w", err)
	}
	m := &vzMachine{vm: machine, done: make(chan struct{})}
	go func() {
		for st := range machine.StateChangedNotify() {
			if st == vz.VirtualMachineStateStopped || st == vz.VirtualMachineStateError {
				m.once.Do(func() { close(m.done) })
				return
			}
		}
	}()
	return m, nil
}

func (m *vzMachine) DialAgent(ctx context.Context) (net.Conn, error) {
	devs := m.vm.SocketDevices()
	if len(devs) == 0 {
		return nil, fmt.Errorf("no vsock device")
	}
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := devs[0].Connect(1024)
		if err != nil {
			ch <- res{nil, err}
			return
		}
		ch <- res{c, nil}
	}()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.c != nil {
				r.c.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

func (m *vzMachine) Done() <-chan struct{} { return m.done }

func (m *vzMachine) Kill() error {
	if m.vm.CanStop() {
		if err := m.vm.Stop(); err != nil {
			return err
		}
	}
	<-m.done
	return nil
}
