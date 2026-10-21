package cli

import (
	"io"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"

	"github.com/AkaraChen/open-agent-runtime/internal/vm"
)

// warmCompression reports whether warm uploads are zstd-compressed:
// OAR_WARM_COMPRESS=1 or 0 forces it; by default only a QEMU port forward
// uses it. Same-runner A/B, both orders: QEMU warms 14-34% faster with it;
// VZ about 25% slower (its guest decompresses slower than the channel
// transfers, on the CI hosts measured), so VZ does not compress.
func warmCompression(g *globalOpts) bool {
	switch os.Getenv("OAR_WARM_COMPRESS") {
	case "1":
		return true
	case "0":
		return false
	}
	st, err := vm.ReadState(vmDir(g))
	if err != nil {
		return false
	}
	return st.Transport == "tcp"
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

// warmFlush reports whether a warm ends by writing the guest cache back:
// OAR_WARM_FLUSH=1 or 0 forces it. VM guests flush: with hundreds of MB
// still to write back, the guest's first runc create waited 377-409 ms
// for ext4's journal commit and the next view build slowed 2-3x (kvm,
// same-runner A/B; 43 ms after the flush). The native backend does not:
// there the first start was unaffected while the flush of the host
// filesystem made warms 4x slower.
func warmFlush(g *globalOpts) bool {
	switch os.Getenv("OAR_WARM_FLUSH") {
	case "1":
		return true
	case "0":
		return false
	}
	st, err := vm.ReadState(vmDir(g))
	if err != nil {
		return true
	}
	return st.Transport != "inprocess"
}

// warmWriteback reports whether the agent syncs the guest cache in the
// background while a warm uploads, so the final flush has only the tail
// left: OAR_WARM_WRITEBACK=1 or 0 forces it. Same-runner A/B: kvm cold warm
// 3.8 -> 2.7 s and QEMU 49 -> 36 s (the final flush 1.45 s -> 21 ms and
// 2.1 s -> 0.14 s), but VZ 37 -> 43 s and native (which does not flush)
// 1.6 -> 2.3 s, so only QEMU-based guests use it.
func warmWriteback(g *globalOpts) bool {
	switch os.Getenv("OAR_WARM_WRITEBACK") {
	case "1":
		return true
	case "0":
		return false
	}
	st, err := vm.ReadState(vmDir(g))
	if err != nil {
		return false
	}
	return st.Transport == "vsock" || st.Transport == "tcp"
}
