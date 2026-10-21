//go:build linux

package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/AkaraChen/open-agent-runtime/internal/proto"
)

func storeTar(t *testing.T, name string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range []*tar.Header{
		{Name: "nix/store/" + name + "/", Typeflag: tar.TypeDir, Mode: 0o555},
		{Name: "nix/store/" + name + "/f", Typeflag: tar.TypeReg, Mode: 0o444, Size: 5},
	} {
		h.ModTime = time.Unix(1, 0)
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte("hello"))
		}
	}
	tw.Close()
	return buf.Bytes()
}

func TestWarmPutZstd(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (overlay xattr on store paths)")
	}
	dir := t.TempDir()
	s, err := New(Config{Root: dir, Mode: "native", Runc: Runc{Root: dir + "/runc"}})
	if err != nil {
		t.Fatal(err)
	}
	name := "0123456789abcdfghijklmnpqrsvwxyz-hello"
	raw := storeTar(t, name)
	sum := sha256.Sum256(raw)
	var z bytes.Buffer
	enc, _ := zstd.NewWriter(&z)
	enc.Write(raw)
	enc.Close()
	put := func(rawSize int64) error {
		args, _ := json.Marshal(proto.WarmPutArgs{
			Item:     proto.WarmItem{Kind: proto.KindStore, Key: "sha256:" + hex.EncodeToString(sum[:]), Name: name},
			Encoding: "zstd", RawSize: rawSize,
		})
		_, err := s.Handle(context.Background(), &proto.Request{Op: proto.OpWarmPut, Args: args, BodySize: int64(z.Len())}, bytes.NewReader(z.Bytes()), nil)
		return err
	}
	if err := put(int64(len(raw)) + 1); err == nil {
		t.Fatal("a wrong raw size was accepted")
	}
	if err := put(int64(len(raw))); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(s.layout.StorePath(name), "f"))
	if err != nil || string(got) != "hello" {
		t.Fatalf("extracted %q, %v", got, err)
	}
}
