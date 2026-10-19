package nar

import (
	"archive/tar"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"io"
	"strings"
	"testing"
)

type narWriter struct{ bytes.Buffer }

func (w *narWriter) str(s string) {
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], uint64(len(s)))
	w.Write(n[:])
	w.WriteString(s)
	if p := (8 - len(s)%8) % 8; p > 0 {
		w.Write(make([]byte, p))
	}
}

func (w *narWriter) strs(ss ...string) {
	for _, s := range ss {
		w.str(s)
	}
}

// sample NAR: dir { bin/ { hello (exec) }, lib -> bin, README }
func sampleNAR() []byte {
	w := &narWriter{}
	w.strs("nix-archive-1", "(", "type", "directory")
	w.strs("entry", "(", "name", "README", "node", "(", "type", "regular", "contents", "hi\n", ")", ")")
	w.strs("entry", "(", "name", "bin", "node", "(", "type", "directory")
	w.strs("entry", "(", "name", "hello", "node", "(", "type", "regular", "executable", "", "contents", "#!/bin/sh\n", ")", ")")
	w.strs(")", ")")
	w.strs("entry", "(", "name", "lib", "node", "(", "type", "symlink", "target", "bin", ")", ")")
	w.strs(")")
	return w.Bytes()
}

func TestToTar(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	var entries []Entry
	if err := ToTar(bytes.NewReader(sampleNAR()), tw, "nix/store/abc-x", func(e Entry) { entries = append(entries, e) }); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	tr := tar.NewReader(&buf)
	got := map[string]*tar.Header{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got[h.Name] = h
	}
	if h := got["nix/store/abc-x/bin/hello"]; h == nil || h.Mode != 0o555 || h.Size != 10 {
		t.Fatalf("hello: %+v", h)
	}
	if h := got["nix/store/abc-x/README"]; h == nil || h.Mode != 0o444 {
		t.Fatalf("README: %+v", h)
	}
	if h := got["nix/store/abc-x/lib"]; h == nil || h.Typeflag != tar.TypeSymlink || h.Linkname != "bin" {
		t.Fatalf("lib: %+v", h)
	}
	if got["nix/store/abc-x/"] == nil {
		t.Fatal("missing root dir")
	}
	if len(entries) != 5 {
		t.Fatalf("entries: %d", len(entries))
	}
}

func TestToTarRejectsTraversal(t *testing.T) {
	w := &narWriter{}
	w.strs("nix-archive-1", "(", "type", "directory", "entry", "(", "name", "..", "node", "(", "type", "regular", "contents", "x", ")", ")", ")")
	if err := ToTar(bytes.NewReader(w.Bytes()), tar.NewWriter(io.Discard), "p", nil); err == nil {
		t.Fatal("expected error for '..' entry")
	}
}

func TestBase32(t *testing.T) {
	// sha256("") in nix base32
	sum := sha256.Sum256(nil)
	got, err := ParseSHA256("sha256:0mdqa9w1p6cmli6976v4wi0sw9r4p5prkj7lzfd1877wk11c9c73")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, sum[:]) {
		t.Fatalf("base32 decode mismatch: %x", got)
	}
}

func TestNarInfoVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	info := "StorePath: /nix/store/abc-x\nURL: nar/abc.nar.xz\nCompression: xz\nNarHash: sha256:0mdqa9w1p6cmli6976v4wi0sw9r4p5prkj7lzfd1877wk11c9c73\nNarSize: 8\nReferences: abc-x def-y\n"
	ni, err := ParseNarInfo(strings.NewReader(info))
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(priv, []byte(ni.Fingerprint()))
	ni.Sigs = []string{"test-1:" + base64.StdEncoding.EncodeToString(sig)}
	key := "test-1:" + base64.StdEncoding.EncodeToString(pub)
	if err := ni.Verify([]string{key}); err != nil {
		t.Fatal(err)
	}
	ni.NarSize = 9
	if err := ni.Verify([]string{key}); err == nil {
		t.Fatal("tampered narinfo verified")
	}
	if !strings.Contains(ni.Fingerprint(), "/nix/store/abc-x,/nix/store/def-y") {
		t.Fatal(ni.Fingerprint())
	}
}
