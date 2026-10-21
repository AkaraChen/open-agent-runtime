//go:build linux

package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AkaraChen/open-agent-runtime/internal/proto"
)

// A tar read in place (native) extracts to the same tree as one streamed.
func TestWarmPutByPath(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	big := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
	for _, h := range []*tar.Header{
		{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "d/empty", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "d/small", Typeflag: tar.TypeReg, Mode: 0o600, Size: 5},
		{Name: "d/big", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(big))},
		{Name: "d/link", Typeflag: tar.TypeSymlink, Linkname: "small"},
		{Name: "d/hard", Typeflag: tar.TypeLink, Linkname: "d/small"},
		{Name: "last", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3},
	} {
		h.ModTime = time.Unix(1, 0)
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		switch h.Name {
		case "d/small":
			tw.Write([]byte("hello"))
		case "d/big":
			tw.Write(big)
		case "last":
			tw.Write([]byte("end"))
		}
	}
	tw.Close()
	dir := t.TempDir()
	src := filepath.Join(dir, "a.tar")
	if err := os.WriteFile(src, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	put := func(name, path string, body []byte) string {
		s, err := New(Config{Root: filepath.Join(dir, name), Mode: "native", Runc: Runc{Root: filepath.Join(dir, "runc")}})
		if err != nil {
			t.Fatal(err)
		}
		args, _ := json.Marshal(proto.WarmPutArgs{Item: proto.WarmItem{Kind: proto.KindBase, Name: "b", Key: "sha256:00"}, Path: path})
		var rd *bytes.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		} else {
			rd = bytes.NewReader(nil)
		}
		if _, err := s.Handle(context.Background(), &proto.Request{Op: proto.OpWarmPut, Args: args, BodySize: int64(len(body))}, rd, nil); err != nil {
			t.Fatal(err)
		}
		p, err := s.itemPath(proto.WarmItem{Kind: proto.KindBase, Name: "b", Key: "sha256:00"})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	a, b := put("stream", "", buf.Bytes()), put("path", src, nil)
	for _, rel := range []string{"d/empty", "d/small", "d/big", "d/hard", "last"} {
		x, err := os.ReadFile(filepath.Join(a, "rootfs", rel))
		if err != nil {
			t.Fatal(err)
		}
		y, err := os.ReadFile(filepath.Join(b, "rootfs", rel))
		if err != nil || !bytes.Equal(x, y) {
			t.Fatalf("%s differs by path: %v", rel, err)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(b, "rootfs", "last")); string(got) != "end" {
		t.Fatalf("last = %q", got)
	}
	if l, err := os.Readlink(filepath.Join(b, "rootfs", "d/link")); err != nil || l != "small" {
		t.Fatalf("symlink %q, %v", l, err)
	}
}

// A truncated tar on disk is an error, not a short file.
func TestWarmPutByPathTruncated(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "f", Typeflag: tar.TypeReg, Mode: 0o644, Size: 100000, ModTime: time.Unix(1, 0)})
	tw.Write(make([]byte, 100000))
	tw.Close()
	dir := t.TempDir()
	src := filepath.Join(dir, "a.tar")
	if err := os.WriteFile(src, buf.Bytes()[:50000], 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Root: filepath.Join(dir, "r"), Mode: "native", Runc: Runc{Root: filepath.Join(dir, "runc")}})
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(proto.WarmPutArgs{Item: proto.WarmItem{Kind: proto.KindBase, Name: "b", Key: "sha256:00"}, Path: src})
	if _, err := s.Handle(context.Background(), &proto.Request{Op: proto.OpWarmPut, Args: args}, bytes.NewReader(nil), nil); err == nil {
		t.Fatal("a truncated tar was accepted")
	}
	// a VM agent refuses paths
	v, _ := New(Config{Root: filepath.Join(dir, "v"), Mode: "vm", Runc: Runc{Root: filepath.Join(dir, "runc")}})
	if _, err := v.Handle(context.Background(), &proto.Request{Op: proto.OpWarmPut, Args: args}, bytes.NewReader(nil), nil); err == nil {
		t.Fatal("a vm agent accepted a host path")
	}
}
