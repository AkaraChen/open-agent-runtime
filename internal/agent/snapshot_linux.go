//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AkaraChen/open-agent-runtime/internal/compose"
	"github.com/AkaraChen/open-agent-runtime/internal/manifest"
	"github.com/AkaraChen/open-agent-runtime/internal/proto"
)

// Application snapshots: the first ready instance of a bundle with
// process.snapshot is checkpointed with CRIU (left running), together with
// its writable layer and private ports. Later instances of that bundle are
// restored from it instead of being started, which skips application
// start-up entirely. Any failure falls back to a normal start.

// criuConf is the CRIU configuration runc passes on checkpoint/restore
// (via the org.criu.config annotation):
//   - ext-mount-map auto: the per-path /nix/store mounts nested in the
//     store view are external bind mounts recreated from the same sources
const criuConf = `ext-mount-map auto
enable-external-masters
enable-external-sharing
`

// criuRestoreConf holds restore-only options, passed via CRIU_CONFIG_FILE:
// the legacy mount engine handles our overlay root on restore.
const criuRestoreConf = `mntns-compat-mode
`

type snapshotMeta struct {
	Digest  string         `json:"digest"`
	Ports   map[string]int `json:"ports"`
	Created time.Time      `json:"created"`
	Bytes   int64          `json:"bytes"`
}

var (
	criuOnce sync.Once
	criuErr  error
)

// criuAvailable reports whether checkpoint/restore can be used here.
func criuAvailable() error {
	criuOnce.Do(func() {
		if _, err := exec.LookPath("criu"); err != nil {
			criuErr = fmt.Errorf("criu is not installed")
		}
	})
	return criuErr
}

func (s *Server) writeCriuConfig() error {
	if err := os.WriteFile(s.layout.CriuConfig(), []byte(criuConf), 0o644); err != nil {
		return err
	}
	return os.WriteFile(s.criuRestoreConfig(), []byte(criuRestoreConf), 0o644)
}

func (s *Server) criuRestoreConfig() string { return filepath.Join(s.cfg.Root, "criu-restore.conf") }

func (s *Server) readSnapshot(digest string) (*snapshotMeta, bool) {
	b, err := os.ReadFile(filepath.Join(s.layout.SnapshotDir(digest), "meta.json"))
	if err != nil {
		return nil, false
	}
	var m snapshotMeta
	if json.Unmarshal(b, &m) != nil {
		return nil, false
	}
	return &m, true
}

// snapshotEnv returns extra environment for snapshot-enabled bundles: Go's
// default MPTCP listeners cannot be checkpointed by CRIU.
func snapshotEnv(pl *manifest.PlatformLock) []string {
	if !pl.Snapshot {
		return nil
	}
	for _, e := range pl.Process.Env {
		if strings.HasPrefix(e, "GODEBUG=") {
			return nil
		}
	}
	return []string{"GODEBUG=multipathtcp=0"}
}

// takeSnapshot checkpoints a ready instance (left running). It is a no-op
// when snapshots are disabled, unavailable, or already taken.
func (s *Server) takeSnapshot(ctx context.Context, in *instance, pl *manifest.PlatformLock, res *proto.RunResult) {
	if !pl.Snapshot {
		return
	}
	if err := criuAvailable(); err != nil {
		res.SnapshotNote = err.Error()
		return
	}
	if _, ok := s.readSnapshot(pl.BundleDigest); ok {
		return
	}
	t := time.Now()
	dst := s.layout.SnapshotDir(pl.BundleDigest)
	tmp, err := os.MkdirTemp(filepath.Dir(dst), "snapshot.tmp-")
	if err != nil {
		res.SnapshotNote = err.Error()
		return
	}
	defer os.RemoveAll(tmp)
	img := filepath.Join(tmp, "img")
	os.MkdirAll(img, 0o700)
	args := []string{"checkpoint", "--image-path", img, "--work-path", img, "--leave-running", in.ID}
	if out, err := s.cfg.Runc.run(ctx, args...); err != nil {
		res.SnapshotNote = fmt.Sprintf("checkpoint failed: %v %s", err, tailFile(filepath.Join(img, "dump.log"), 3))
		_ = out
		return
	}
	// the writable layer: files the application created during start-up
	if out, err := exec.Command("cp", "-a", filepath.Join(in.dir, "upper"), filepath.Join(tmp, "upper")).CombinedOutput(); err != nil {
		res.SnapshotNote = fmt.Sprintf("copy writable layer: %v %s", err, out)
		return
	}
	meta := snapshotMeta{Digest: pl.BundleDigest, Ports: in.Ports, Created: time.Now().UTC(), Bytes: dirSize(tmp)}
	b, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(tmp, "meta.json"), b, 0o644); err != nil {
		res.SnapshotNote = err.Error()
		return
	}
	if err := os.Rename(tmp, dst); err != nil {
		res.SnapshotNote = err.Error()
		return
	}
	res.Timings.Snapshot = time.Since(t)
	res.SnapshotBytes = meta.Bytes
}

// portsFree reports whether every port can be bound right now.
func portsFree(ports map[string]int) bool {
	for _, p := range ports {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			return false
		}
		l.Close()
	}
	return true
}

// restoreInstance creates an instance of the bundle from its snapshot: a
// fresh overlay seeded with the snapshot's writable layer, then
// `runc restore`. The process is running (and was ready) when it returns.
func (s *Server) restoreInstance(ctx context.Context, name string, a proto.RunArgs, b *compose.Bundle) (*instance, error) {
	meta, ok := s.readSnapshot(a.Lock.BundleDigest)
	if !ok {
		return nil, fmt.Errorf("no snapshot")
	}
	if err := criuAvailable(); err != nil {
		return nil, err
	}
	if !portsFree(meta.Ports) {
		return nil, fmt.Errorf("snapshot ports are in use")
	}
	snap := s.layout.SnapshotDir(a.Lock.BundleDigest)
	var env []string
	for _, p := range a.Lock.Ports {
		env = append(env, fmt.Sprintf("%s=%d", p.Env, meta.Ports[p.Name]))
	}
	env = append(env, snapshotEnv(a.Lock)...)
	in, err := s.instantiateSeeded(name, b, env, filepath.Join(snap, "upper"), func(in *instance) {
		in.Ports, in.PublicPorts = meta.Ports, a.Lock.Ports
	})
	if err != nil {
		return nil, err
	}
	work := filepath.Join(in.dir, "criu")
	os.MkdirAll(work, 0o700)
	args := []string{"restore", "--image-path", filepath.Join(snap, "img"), "--work-path", work,
		"--bundle", in.dir, "--detach", "--pid-file", filepath.Join(in.dir, "pid")}
	if s.cfg.Runc.NoPivot {
		args = append(args, "--no-pivot")
	}
	args = append(args, in.ID)
	if err := s.cfg.Runc.withStdioEnv(ctx, filepath.Join(in.dir, "console.log"), args,
		[]string{"CRIU_CONFIG_FILE=" + s.criuRestoreConfig()}); err != nil {
		s.destroy(ctx, in, true)
		return nil, fmt.Errorf("%v %s", err, tailFile(filepath.Join(work, "restore.log"), 3))
	}
	in.Started = true
	return in, nil
}

func dirSize(root string) int64 {
	var n int64
	filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

func tailFile(path string, lines int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var errs []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, "Error") {
			errs = append(errs, strings.TrimSpace(l))
		}
	}
	if len(errs) > lines {
		errs = errs[len(errs)-lines:]
	}
	return strings.Join(errs, " | ")
}
