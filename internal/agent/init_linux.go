//go:build linux

package agent

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// GuestRoot is where the guest keeps its persistent cache.
const GuestRoot = "/var/lib/oar"

// AgentPort is the vsock port the guest agent listens on.
const AgentPort = 1024

func logInit(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[oar-init] "+format+"\n", args...)
}

// RunInit is PID 1 inside the guest VM. It prepares the minimal Linux
// environment, mounts the persistent data disk, supervises the agent and
// reaps orphaned processes (including container inits).
func RunInit() {
	console, err := os.OpenFile("/dev/console", os.O_RDWR, 0)
	if err == nil {
		unix.Dup2(int(console.Fd()), 1)
		unix.Dup2(int(console.Fd()), 2)
	}
	os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	if !switchedRoot() {
		logInit("init started") // timestamped by the host (boot phases)
	}
	// leave the initramfs so that pivot_root works (runc, CRIU restore)
	switchRoot()
	pivot := switchedRoot()
	logInit("booting guest (agent %s)", Version)
	mounts := []struct {
		src, dst, typ string
		flags         uintptr
		data          string
	}{
		{"proc", "/proc", "proc", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, ""},
		{"sysfs", "/sys", "sysfs", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, ""},
		{"devtmpfs", "/dev", "devtmpfs", unix.MS_NOSUID, "mode=0755"},
		{"devpts", "/dev/pts", "devpts", unix.MS_NOSUID | unix.MS_NOEXEC, "gid=5,mode=0620,ptmxmode=0666"},
		{"tmpfs", "/dev/shm", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=1777"},
		{"tmpfs", "/run", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=0755"},
		{"tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=1777"},
		{"cgroup2", "/sys/fs/cgroup", "cgroup2", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, ""},
	}
	for _, m := range mounts {
		os.MkdirAll(m.dst, 0o755)
		if err := unix.Mount(m.src, m.dst, m.typ, m.flags, m.data); err != nil && err != unix.EBUSY {
			logInit("mount %s: %v", m.dst, err)
		}
	}
	enableControllers()
	kernel := uptime()
	if k, sw, ok := switchTimes(); ok {
		kernel = k
		markBootAt("switchRoot", sw)
	}
	markBootAt("kernel", kernel)
	markBoot("mounts")
	unix.Sethostname([]byte("oar-guest"))
	ifUp("lo")
	markBoot("init")
	mountData()
	markBoot("dataDisk")
	if _, err := os.Stat("/sys/class/net/eth0"); err == nil {
		ifUp("eth0")
		if spec := cmdlineParam("oar.net"); spec != "" {
			// a known network (QEMU user networking): configured at once,
			// without DHCP, so the agent is reachable through port
			// forwarding as soon as it listens
			if err := staticNet("eth0", spec); err != nil {
				logInit("static network %s: %v", spec, err)
			}
		} else if p, err := exec.LookPath("udhcpc"); err == nil {
			c := exec.Command(p, "-i", "eth0", "-b", "-q", "-t", "10", "-s", "/usr/share/udhcpc/default.script")
			c.Stdout, c.Stderr = io.Discard, io.Discard
			_ = c.Start()
		}
	}

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, unix.SIGTERM, unix.SIGUSR2, unix.SIGINT, unix.SIGPWR)
	children := make(chan os.Signal, 16)
	signal.Notify(children, unix.SIGCHLD)

	self, _ := os.Executable()
	var agentPid int
	spawn := func() {
		attr := &os.ProcAttr{
			Files: []*os.File{nil, os.Stderr, os.Stderr},
			Env:   append(os.Environ(), "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"),
		}
		argv := []string{"oar-agent", "serve", "--mode", "vm",
			"--listen", fmt.Sprintf("vsock:%d", AgentPort), "--root", GuestRoot}
		if !pivot {
			argv = append(argv, "--no-pivot")
		}
		if port := cmdlineParam("oar.tcp"); port != "" {
			argv = append(argv, "--listen", "tcp:0.0.0.0:"+port)
		}
		p, err := os.StartProcess(self, argv, attr)
		if err != nil {
			logInit("start agent: %v", err)
			return
		}
		agentPid = p.Pid
		logInit("agent started (pid %d)", agentPid)
	}
	spawn()
	for {
		select {
		case <-children:
			for {
				var ws unix.WaitStatus
				pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
				if pid <= 0 || err != nil {
					break
				}
				if pid == agentPid {
					logInit("agent exited (%v), restarting", ws)
					time.Sleep(time.Second)
					spawn()
				}
			}
		case <-shutdown:
			powerOff()
		}
	}
}

// PowerOff asks PID 1 to shut the guest down cleanly.
func PowerOff() { unix.Kill(1, unix.SIGUSR2) }

func powerOff() {
	logInit("shutting down")
	unix.Kill(-1, unix.SIGTERM)
	time.Sleep(2 * time.Second)
	unix.Kill(-1, unix.SIGKILL)
	unix.Sync()
	unix.Unmount(GuestRoot, unix.MNT_DETACH)
	unix.Sync()
	unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
	os.Exit(0)
}

func enableControllers() {
	b, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	if err != nil {
		return
	}
	var ctl []string
	for _, c := range strings.Fields(string(b)) {
		ctl = append(ctl, "+"+c)
	}
	_ = os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control", []byte(strings.Join(ctl, " ")), 0)
}

func ifUp(name string) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	_ = unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

// staticNet configures name from "addr/prefix,gateway,dns".
func staticNet(name, spec string) error {
	parts := strings.Split(spec, ",")
	if len(parts) != 3 {
		return fmt.Errorf("want addr/prefix,gateway,dns")
	}
	ip, ipnet, err := net.ParseCIDR(parts[0])
	if err != nil {
		return err
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	for _, set := range []struct {
		req  uint
		addr net.IP
	}{{unix.SIOCSIFADDR, ip}, {unix.SIOCSIFNETMASK, net.IP(ipnet.Mask)}} {
		ifr, err := unix.NewIfreq(name)
		if err != nil {
			return err
		}
		if err := ifr.SetInet4Addr(set.addr.To4()); err != nil {
			return err
		}
		if err := unix.IoctlIfreq(fd, set.req, ifr); err != nil {
			return err
		}
	}
	if out, err := exec.Command("ip", "route", "add", "default", "via", parts[1], "dev", name).CombinedOutput(); err != nil {
		return fmt.Errorf("default route: %v: %s", err, out)
	}
	return os.WriteFile("/etc/resolv.conf", []byte("nameserver "+parts[2]+"\n"), 0o644)
}

// mountData mounts the persistent data disk (/dev/vda), formatting it on
// first boot. Without a disk the cache lives in memory.
func mountData() {
	os.MkdirAll(GuestRoot, 0o755)
	dev := ""
	for _, d := range []string{"/dev/vda", "/dev/sda"} {
		if _, err := os.Stat(d); err == nil {
			dev = d
			break
		}
	}
	if dev == "" {
		logInit("no data disk; using tmpfs for %s", GuestRoot)
		unix.Mount("tmpfs", GuestRoot, "tmpfs", 0, "mode=0755")
		return
	}
	if !hasExt4(dev) {
		logInit("formatting %s", dev)
		markBoot("formatStart")
		out, err := mkfsData(dev).CombinedOutput()
		if err != nil {
			logInit("mkfs.ext4 failed: %v: %s", err, out)
		}
	}
	if err := unix.Mount(dev, GuestRoot, "ext4", unix.MS_NOATIME, ""); err != nil {
		// The disk only holds a rebuildable cache: repair it, and if that
		// fails start over with a fresh filesystem.
		logInit("mount %s: %v; running e2fsck", dev, err)
		exec.Command("e2fsck", "-p", "-f", dev).Run()
		if err := unix.Mount(dev, GuestRoot, "ext4", unix.MS_NOATIME, ""); err != nil {
			logInit("still unmountable (%v); reformatting the cache disk", err)
			mkfsData(dev).Run()
			if err := unix.Mount(dev, GuestRoot, "ext4", unix.MS_NOATIME, ""); err != nil {
				logInit("mount %s: %v; using tmpfs", dev, err)
				unix.Mount("tmpfs", GuestRoot, "tmpfs", 0, "mode=0755")
				return
			}
		}
	}
	logInit("data disk %s mounted at %s", dev, GuestRoot)
}

// mkfsData formats the cache disk. Metadata is written lazily, the device
// is not discarded (a sparse image has nothing to discard, and discarding
// it through virtio costs more than the rest of mkfs) and no blocks are
// reserved for online growth.
func mkfsData(dev string) *exec.Cmd {
	return exec.Command("mkfs.ext4", "-q", "-F", "-L", "oar-data", "-O", "^resize_inode",
		"-E", "lazy_itable_init=1,lazy_journal_init=1,nodiscard", dev)
}

func hasExt4(dev string) bool {
	f, err := os.Open(dev)
	if err != nil {
		return false
	}
	defer f.Close()
	b := make([]byte, 2)
	if _, err := f.ReadAt(b, 1024+0x38); err != nil {
		return false
	}
	return bytes.Equal(b, []byte{0x53, 0xEF})
}

// cmdlineParam returns the value of key=value on the kernel command line.
func cmdlineParam(key string) string {
	b, _ := os.ReadFile("/proc/cmdline")
	for _, f := range strings.Fields(string(b)) {
		if k, v, ok := strings.Cut(f, "="); ok && k == key {
			return v
		}
	}
	return ""
}
