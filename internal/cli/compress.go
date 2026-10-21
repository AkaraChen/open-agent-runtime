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
