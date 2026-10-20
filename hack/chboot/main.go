// chboot boots the oar guest with Cloud Hypervisor and reports how long it
// takes until the guest's init starts and until the agent answers over
// hybrid vsock, for comparison with the QEMU backends (roadmap P1-2).
//
//	go run ./hack/chboot -ch cloud-hypervisor -kernel guest/out/amd64/vmlinux \
//	    -initrd guest/out/amd64/initramfs.cpio.lz4
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/AkaraChen/open-agent-runtime/internal/proto"
	"github.com/AkaraChen/open-agent-runtime/internal/vm"
)

func main() {
	ch := flag.String("ch", "cloud-hypervisor", "cloud-hypervisor binary")
	kernel := flag.String("kernel", "guest/out/amd64/vmlinux", "PVH kernel (vmlinux)")
	initrd := flag.String("initrd", "guest/out/amd64/initramfs.cpio.lz4", "initramfs")
	runs := flag.Int("runs", 3, "boots per disk state")
	memory := flag.Int("memory", 4096, "MiB")
	flag.Parse()
	dir, err := os.MkdirTemp("", "chboot-")
	if err != nil {
		fatal(err)
	}
	defer os.RemoveAll(dir)
	disk := filepath.Join(dir, "data.img")
	for i := 0; i < *runs*2; i++ {
		state := "warm"
		if i%2 == 0 {
			state = "cold"
			os.Remove(disk)
		}
		if err := vm.EnsureDisk(disk, 16); err != nil {
			fatal(err)
		}
		r, err := boot(*ch, *kernel, *initrd, disk, dir, *memory)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("cloud-hypervisor %s: init %v, agent %v, guest uptime at agent %.0f ms, kernel %.0f ms, RSS %d MiB\n",
			state, r.init.Round(time.Millisecond), r.agent.Round(time.Millisecond), r.uptime*1000, r.kernel*1000, r.rss>>20)
	}
}

type result struct {
	init, agent    time.Duration
	uptime, kernel float64
	rss            int64
}

func boot(ch, kernel, initrd, disk, dir string, memory int) (*result, error) {
	sock := filepath.Join(dir, "vsock.sock")
	console := filepath.Join(dir, "console.log")
	os.Remove(sock)
	os.Remove(console)
	start := time.Now()
	cmd := exec.Command(ch,
		"--kernel", kernel, "--initramfs", initrd,
		"--cmdline", "console=ttyS0 panic=-1 quiet loglevel=3",
		"--cpus", "boot=2", "--memory", fmt.Sprintf("size=%dM", memory),
		"--disk", "path="+disk+",image_type=raw",
		"--vsock", "cid=3,socket="+sock,
		"--serial", "file="+console, "--console", "off")
	out, _ := os.Create(filepath.Join(dir, "ch.log"))
	defer out.Close()
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	defer func() { cmd.Process.Kill(); <-exited }()
	r := &result{}
	client := &proto.Client{Dial: func(ctx context.Context) (net.Conn, error) { return dialHybrid(ctx, sock, 1024) }}
	deadline := time.Now().Add(60 * time.Second)
	for {
		if r.init == 0 {
			if b, _ := os.ReadFile(console); strings.Contains(string(b), "[oar-init] init started") {
				r.init = time.Since(start)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		var info proto.InfoResult
		err := client.Call(ctx, proto.OpInfo, nil, &info)
		cancel()
		if err == nil {
			r.agent = time.Since(start)
			break
		}
		select {
		case <-exited:
			b, _ := os.ReadFile(filepath.Join(dir, "ch.log"))
			return nil, fmt.Errorf("cloud-hypervisor exited:\n%s", b)
		default:
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(filepath.Join(dir, "ch.log"))
			c, _ := os.ReadFile(console)
			return nil, fmt.Errorf("agent did not answer: %v\n%s\n%s", err, b, c)
		}
		time.Sleep(5 * time.Millisecond)
	}
	var st proto.StatsResult
	if err := client.Call(context.Background(), proto.OpStats, nil, &st); err == nil {
		r.uptime, r.kernel = st.Boot["agentReady"], st.Boot["kernel"]
	}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", cmd.Process.Pid)); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			var kb int64
			if _, err := fmt.Sscanf(l, "VmRSS: %d kB", &kb); err == nil {
				r.rss = kb << 10
			}
		}
	}
	client.Call(context.Background(), proto.OpShutdown, nil, nil)
	return r, nil
}

// dialHybrid opens a stream to a guest vsock port through Cloud
// Hypervisor's unix socket ("CONNECT <port>" / "OK <n>").
func dialHybrid(ctx context.Context, sock string, port int) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	fmt.Fprintf(c, "CONNECT %d\n", port)
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "OK ") {
		c.Close()
		return nil, fmt.Errorf("hybrid vsock: %q %v", line, err)
	}
	c.SetDeadline(time.Time{})
	return c, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
