// Package nar converts Nix ARchives into tar streams without touching the
// host filesystem, so Linux store paths can be prepared on any host OS
// (including case-insensitive macOS volumes).
package nar

import (
	"archive/tar"
	"encoding/binary"
	"fmt"
	"io"
	"path"
	"time"
)

// Entry describes one filesystem object discovered while converting.
type Entry struct {
	Path       string // relative to the store path root ("" for the root)
	Type       byte   // tar.TypeReg, tar.TypeDir, tar.TypeSymlink
	Executable bool
	Target     string
	Size       int64
}

// Epoch is the mtime nix uses for every store object.
var Epoch = time.Unix(1, 0).UTC()

type reader struct {
	r   io.Reader
	buf [8]byte
}

func (r *reader) u64() (uint64, error) {
	if _, err := io.ReadFull(r.r, r.buf[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(r.buf[:]), nil
}

func (r *reader) pad(n uint64) error {
	if p := (8 - n%8) % 8; p > 0 {
		if _, err := io.ReadFull(r.r, r.buf[:p]); err != nil {
			return err
		}
	}
	return nil
}

func (r *reader) str(max uint64) (string, error) {
	n, err := r.u64()
	if err != nil {
		return "", err
	}
	if n > max {
		return "", fmt.Errorf("nar: string too long (%d)", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r.r, b); err != nil {
		return "", err
	}
	return string(b), r.pad(n)
}

func (r *reader) expect(want string) error {
	s, err := r.str(256)
	if err != nil {
		return err
	}
	if s != want {
		return fmt.Errorf("nar: expected %q, got %q", want, s)
	}
	return nil
}

// ToTar reads a NAR from src and writes its contents into tw under prefix
// (e.g. "nix/store/<hash>-<name>"). Parent directories of prefix are not
// emitted. The callback, if non-nil, observes every entry.
func ToTar(src io.Reader, tw *tar.Writer, prefix string, fn func(Entry)) error {
	r := &reader{r: src}
	if err := r.expect("nix-archive-1"); err != nil {
		return err
	}
	return r.node(tw, prefix, "", fn)
}

func (r *reader) node(tw *tar.Writer, prefix, rel string, fn func(Entry)) error {
	if err := r.expect("("); err != nil {
		return err
	}
	if err := r.expect("type"); err != nil {
		return err
	}
	typ, err := r.str(64)
	if err != nil {
		return err
	}
	name := prefix
	if rel != "" {
		name = path.Join(prefix, rel)
	}
	hdr := &tar.Header{
		Name:    name,
		ModTime: Epoch,
		Uid:     0, Gid: 0,
		Format: tar.FormatPAX,
	}
	switch typ {
	case "regular":
		exec := false
		tok, err := r.str(64)
		if err != nil {
			return err
		}
		if tok == "executable" {
			exec = true
			if err := r.expect(""); err != nil {
				return err
			}
			if tok, err = r.str(64); err != nil {
				return err
			}
		}
		if tok != "contents" {
			return fmt.Errorf("nar: expected contents, got %q", tok)
		}
		size, err := r.u64()
		if err != nil {
			return err
		}
		hdr.Typeflag = tar.TypeReg
		hdr.Size = int64(size)
		hdr.Mode = 0o444
		if exec {
			hdr.Mode = 0o555
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := io.CopyN(tw, r.r, int64(size)); err != nil {
			return err
		}
		if err := r.pad(size); err != nil {
			return err
		}
		if fn != nil {
			fn(Entry{Path: rel, Type: tar.TypeReg, Executable: exec, Size: int64(size)})
		}
		return r.expect(")")
	case "symlink":
		if err := r.expect("target"); err != nil {
			return err
		}
		target, err := r.str(4096)
		if err != nil {
			return err
		}
		hdr.Typeflag = tar.TypeSymlink
		hdr.Linkname = target
		hdr.Mode = 0o777
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fn != nil {
			fn(Entry{Path: rel, Type: tar.TypeSymlink, Target: target})
		}
		return r.expect(")")
	case "directory":
		hdr.Typeflag = tar.TypeDir
		hdr.Name += "/"
		hdr.Mode = 0o555
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fn != nil {
			fn(Entry{Path: rel, Type: tar.TypeDir})
		}
		for {
			tok, err := r.str(64)
			if err != nil {
				return err
			}
			if tok == ")" {
				return nil
			}
			if tok != "entry" {
				return fmt.Errorf("nar: expected entry, got %q", tok)
			}
			if err := r.expect("("); err != nil {
				return err
			}
			if err := r.expect("name"); err != nil {
				return err
			}
			child, err := r.str(255)
			if err != nil {
				return err
			}
			if child == "" || child == "." || child == ".." || path.Base(child) != child {
				return fmt.Errorf("nar: invalid entry name %q", child)
			}
			if err := r.expect("node"); err != nil {
				return err
			}
			if err := r.node(tw, prefix, path.Join(rel, child), fn); err != nil {
				return err
			}
			if err := r.expect(")"); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("nar: unknown node type %q", typ)
	}
}
