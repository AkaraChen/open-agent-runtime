//go:build linux

package agent

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/AkaraChen/open-agent-runtime/internal/proto"
)

// benchTar writes a tar shaped like a rootfs: many small files and a few
// large ones (about 200 MB of low-entropy content).
func benchTar(tb testing.TB, p string) (size int64, key string) {
	f, err := os.Create(p)
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	tw := tar.NewWriter(io.MultiWriter(f, h))
	rng := rand.New(rand.NewSource(1))
	buf := make([]byte, 8<<20)
	for i := range buf {
		buf[i] = byte(rng.Intn(16))
	}
	add := func(name string, n int) {
		hdr := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(n), ModTime: time.Unix(1, 0)}
		if err := tw.WriteHeader(hdr); err != nil {
			tb.Fatal(err)
		}
		if _, err := tw.Write(buf[:n]); err != nil {
			tb.Fatal(err)
		}
	}
	for d := 0; d < 200; d++ {
		for i := 0; i < 100; i++ {
			add(fmt.Sprintf("d%d/f%d", d, i), 200+rng.Intn(12000))
		}
	}
	for i := 0; i < 8; i++ {
		add(fmt.Sprintf("big%d", i), 8<<20)
	}
	if err := tw.Close(); err != nil {
		tb.Fatal(err)
	}
	st, _ := f.Stat()
	return st.Size(), "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func zstdFile(tb testing.TB, src, dst string) int64 {
	in, err := os.Open(src)
	if err != nil {
		tb.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		tb.Fatal(err)
	}
	defer out.Close()
	enc, _ := zstd.NewWriter(out, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if _, err := io.Copy(enc, in); err != nil {
		tb.Fatal(err)
	}
	enc.Close()
	st, _ := out.Stat()
	return st.Size()
}

// BenchmarkWarmPut times receiving one rootfs-like artifact the three ways
// an agent gets it: as a plain stream, as a zstd stream, and by path
// (native). OAR_BENCH_DIR should be on a real disk (default: a temp dir).
func BenchmarkWarmPut(b *testing.B) {
	dir := os.Getenv("OAR_BENCH_DIR")
	if dir == "" {
		dir = b.TempDir()
	}
	src := filepath.Join(dir, "a.tar")
	size, key := benchTar(b, src)
	zsrc := src + ".zst"
	zsize := zstdFile(b, src, zsrc)
	for _, mode := range []string{"stream", "zstd", "inplace"} {
		b.Run(mode, func(b *testing.B) {
			b.SetBytes(size)
			var cpu time.Duration
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				root := filepath.Join(dir, "root")
				os.RemoveAll(root)
				s, err := New(Config{Root: root, Mode: "native", Runc: Runc{Root: filepath.Join(dir, "runc")}})
				if err != nil {
					b.Fatal(err)
				}
				args := proto.WarmPutArgs{Item: proto.WarmItem{Kind: proto.KindBase, Name: "bench", Key: key}}
				req := proto.Request{Op: proto.OpWarmPut}
				var body io.Reader
				open := func(p string) *os.File {
					f, err := os.Open(p)
					if err != nil {
						b.Fatal(err)
					}
					b.Cleanup(func() { f.Close() })
					return f
				}
				switch mode {
				case "stream":
					body, req.BodySize = open(src), size
				case "zstd":
					args.Encoding, args.RawSize = "zstd", size
					body, req.BodySize = open(zsrc), zsize
				case "inplace":
					args.Path = src
				}
				req.Args, _ = json.Marshal(args)
				b.StartTimer()
				c0 := cpuTime()
				if _, err := s.Handle(context.Background(), &req, body, nil); err != nil {
					b.Fatal(err)
				}
				cpu += cpuTime() - c0
			}
			b.ReportMetric(float64(cpu.Milliseconds())/float64(b.N), "cpu-ms/op")
		})
	}
}

// cpuTime is the process's user+system CPU time (all goroutines).
func cpuTime() time.Duration {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
