//go:build darwin && cgo

package vm

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/Code-Hex/vz/v4"
	"github.com/Code-Hex/vz/v4/vmnet"
	"golang.org/x/sys/unix"
)

// vzBackend runs the guest with Apple's Virtualization.framework. The
// binary must be signed with the com.apple.security.virtualization
// entitlement (see hack/sign-darwin.sh).
type vzBackend struct{}

func init() { Register(vzBackend{}) }

func (vzBackend) Name() string          { return "vz" }
func (vzBackend) NeedsGuestImage() bool { return true }
func (vzBackend) NeedsRoot() bool       { return false }

// macOS26 reports whether a product version ("26.1", "15.6.1") is 26 or later.
func macOS26(version string) bool {
	major, _, _ := strings.Cut(version, ".")
	n, err := strconv.Atoi(major)
	return err == nil && n >= 26
}

// Available requires macOS 26 (the vmnet network API) and hypervisor
// support (kern.hv_support), which is absent e.g. inside VMs on M1 hosts
// that cannot nest virtualization.
func (vzBackend) Available() error {
	if v, err := unix.Sysctl("kern.osproductversion"); err != nil || !macOS26(v) {
		return fmt.Errorf("the vz backend needs macOS 26 or later (this is macOS %s): it puts the guest on a vmnet network", v)
	}
	if v, err := unix.SysctlUint32("kern.hv_support"); err != nil || v != 1 {
		return fmt.Errorf("Virtualization.framework is not available: the host has no hypervisor support (kern.hv_support unset; nested virtualization is unsupported here)")
	}
	return nil
}

type vzMachine struct {
	vm      *vz.VirtualMachine
	network *vmnet.Network // kept alive with the VM
	done    chan struct{}
	once    sync.Once
}

func (vzBackend) Boot(ctx context.Context, cfg MachineConfig) (Machine, error) {
	if cfg.Arch != "" && cfg.Arch != runtime.GOARCH {
		return nil, fmt.Errorf("vz can only run %s guests on this Mac", runtime.GOARCH)
	}
	console := "hvc0"
	cmdline := "console=" + console + " " + guestKernelParams
	attachment, network, netSpec, err := vzNetwork()
	if err != nil {
		return nil, err
	}
	if netSpec != "" {
		cmdline += " oar.net=" + netSpec
	}
	if cfg.GuestCmdline != "" {
		cmdline += " " + cfg.GuestCmdline
	}
	boot, err := vz.NewLinuxBootLoader(cfg.Kernel,
		vz.WithCommandLine(cmdline),
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

	disk, err := OpenDataDisk(cfg.Dir, cfg.DiskGiB)
	if err != nil {
		return nil, err
	}
	// The data disk is a rebuildable cache: guest flushes are not passed on
	// (VZ's default "full" mode is F_FULLFSYNC, "fsync" still a disk write
	// per flush).
	diskAtt, err := vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(disk, false, vz.DiskImageCachingModeCached, vz.DiskImageSynchronizationModeNone)
	if err != nil {
		return nil, fmt.Errorf("disk: %w", err)
	}
	blk, err := vz.NewVirtioBlockDeviceConfiguration(diskAtt)
	if err != nil {
		return nil, err
	}
	vmCfg.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{blk})

	netCfg, err := vz.NewVirtioNetworkDeviceConfiguration(attachment)
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
	m := &vzMachine{vm: machine, network: network, done: make(chan struct{})}
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

// vzNetwork returns the guest's network attachment: a vmnet shared (NAT)
// network on a subnet that no host interface uses, with the guest's static
// address for the kernel command line (the API needs macOS 26). VZ's own
// NAT attachment always takes vmnet's default 192.168.64.0/24, and on a host
// that is itself on that subnet (a Mac VM, such as the GitHub macOS
// runners) its bridge takes over the host's gateway and DNS.
func vzNetwork() (vz.NetworkDeviceAttachment, *vmnet.Network, string, error) {
	used, err := hostPrefixes()
	if err != nil {
		return nil, nil, "", fmt.Errorf("host interfaces: %w", err)
	}
	subnet, err := freeGuestSubnet(used)
	if err != nil {
		return nil, nil, "", err
	}
	cfg, err := vmnet.NewNetworkConfiguration(vmnet.ModeShared, vmnet.WithIPv4Subnet(subnet))
	if err != nil {
		return nil, nil, "", fmt.Errorf("vmnet network on %s: %w", subnet, err)
	}
	network, err := vmnet.NewNetwork(cfg)
	if err != nil {
		return nil, nil, "", fmt.Errorf("vmnet network on %s: %w", subnet, err)
	}
	att, err := vz.NewVMNetNetworkDeviceAttachment(network)
	if err != nil {
		return nil, nil, "", fmt.Errorf("vmnet attachment: %w", err)
	}
	return att, network, guestNetSpec(subnet), nil
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
			if r := <-ch; r.err == nil && r.c != nil {
				r.c.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

func (m *vzMachine) Done() <-chan struct{} { return m.done }

// Transport reports the control channel (VZ's vsock: a slow path on the
// hosts measured so far, 20-35 MB/s).
func (m *vzMachine) Transport() string { return "vz-vsock" }

// GuestNetwork reports that the guest's NAT address is reachable from the
// host, so ingress ports are proxied to it directly.
func (m *vzMachine) GuestNetwork() bool { return true }

func (m *vzMachine) Kill() error {
	if m.vm.CanStop() {
		if err := m.vm.Stop(); err != nil {
			return err
		}
	}
	<-m.done
	return nil
}
