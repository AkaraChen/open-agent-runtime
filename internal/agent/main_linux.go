//go:build linux

package agent

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Main implements `oar-agent serve` (also reachable as `oar agent serve`).
func Main(args []string) error {
	if len(args) == 0 || args[0] != "serve" {
		return fmt.Errorf("usage: oar-agent serve [--listen vsock:1024|unix:PATH|tcp:ADDR] [--root DIR]")
	}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	var listens listFlag
	fs.Var(&listens, "listen", "listen address (repeatable): vsock:PORT, unix:PATH, tcp:ADDR")
	root := fs.String("root", GuestRoot, "cache and state directory")
	mode := fs.String("mode", "native", "vm, wsl or native")
	runcBin := fs.String("runc", "runc", "runc binary")
	runcRoot := fs.String("runc-root", "/run/oar/runc", "runc state directory")
	noPivot := fs.Bool("no-pivot", false, "pass --no-pivot to runc (initramfs root)")
	sockOwner := fs.String("socket-owner", "", "uid:gid owning the unix socket")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *mode == "wsl" {
		prepareWSL()
	}
	uid, gid := -1, -1
	if *sockOwner != "" {
		var err error
		if uid, gid, err = parseOwner(*sockOwner); err != nil {
			return err
		}
	}
	if len(listens) == 0 {
		listens = listFlag{fmt.Sprintf("vsock:%d", AgentPort)}
	}
	var ls []net.Listener
	for _, spec := range listens {
		l, err := ListenSpec(spec, uid, gid)
		if err != nil {
			return err
		}
		ls = append(ls, l)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), unix.SIGTERM, unix.SIGINT)
	defer cancel()
	cfg := Config{
		Root: *root,
		Mode: *mode,
		Runc: Runc{Binary: *runcBin, Root: *runcRoot, NoPivot: *noPivot},
	}
	if *mode == "vm" {
		cfg.Shutdown = PowerOff
	}
	return Serve(ctx, ls, cfg)
}

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func parseOwner(s string) (int, int, error) {
	var u, g string
	for i := range s {
		if s[i] == ':' {
			u, g = s[:i], s[i+1:]
			break
		}
	}
	if u == "" {
		u, g = s, s
	}
	uid, err := strconv.Atoi(u)
	if err != nil {
		return 0, 0, fmt.Errorf("bad socket owner %q", s)
	}
	gid, err := strconv.Atoi(g)
	if err != nil {
		return 0, 0, fmt.Errorf("bad socket owner %q", s)
	}
	return uid, gid, nil
}

// prepareWSL performs the part of guest init that a WSL2 distro lacks.
func prepareWSL() {
	var st unix.Statfs_t
	if err := unix.Statfs("/sys/fs/cgroup", &st); err != nil || st.Type != unix.CGROUP2_SUPER_MAGIC {
		os.MkdirAll("/sys/fs/cgroup", 0o755)
		if err := unix.Mount("cgroup2", "/sys/fs/cgroup", "cgroup2", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
			fmt.Fprintln(os.Stderr, "mount cgroup2:", err)
		}
	}
	enableControllers()
	os.MkdirAll(GuestRoot, 0o755)
}
