package cli

import (
	"io"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"

	"github.com/AkaraChen/open-agent-runtime/internal/vm"
)

// warmTuning is how a warm is done for a control channel. Every choice
// was decided by same-runner A/B benchmarks (docs/benchmarks/12-warm):
//
//   - Flush: VM guests write the cache back before the warm returns; with
//     hundreds of MB pending, the guest's first runc create waited ~400 ms
//     for ext4's journal commit. The native backend skips it (its first
//     start was unaffected and the host-wide flush made warms 4x slower).
//   - Writeback: QEMU guests sync in the background while uploading, so
//     the final flush is only the tail (kvm 3.8 -> 2.7 s, macOS QEMU
//     49 -> 36 s); it made VZ and native slower.
//   - Compress: zstd over a QEMU port forward (14-34% faster); VZ's guest
//     decompresses slower than its channel transfers (~25% slower).
type warmTuning struct{ Flush, Writeback, Compress bool }

func tuningFor(transport string) warmTuning {
	switch transport {
	case "inprocess": // native
		return warmTuning{}
	case "vsock": // QEMU/KVM
		return warmTuning{Flush: true, Writeback: true}
	case "tcp": // QEMU through a port forward (macOS)
		return warmTuning{Flush: true, Writeback: true, Compress: true}
	default: // VZ, and anything unknown: the safe choice for a guest disk
		return warmTuning{Flush: true}
	}
}

// warmTuningFor reads the running VM's control channel.
func warmTuningFor(g *globalOpts) warmTuning {
	st, err := vm.ReadState(vmDir(g))
	if err != nil {
		return tuningFor("")
	}
	return tuningFor(st.Transport)
}

// zstdArtifact returns the zstd-compressed copy of the artifact at p,
// creating it next to the artifact on first use (later warms reuse it).
func zstdArtifact(p string) (string, error) {
	z := p + ".zst"
	src, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(z); err == nil && st.Size() > 0 && !st.ModTime().Before(src.ModTime()) {
		return z, nil
	}
	in, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(p), filepath.Base(p)+".zst.tmp-")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	// the fastest level: it already halves typical store paths, and the
	// host compresses several artifacts at once
	enc, err := zstd.NewWriter(tmp, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1))
	if err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := io.Copy(enc, in); err != nil {
		enc.Close()
		tmp.Close()
		return "", err
	}
	if err := enc.Close(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return z, os.Rename(tmp.Name(), z)
}
