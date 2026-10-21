package builder

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/AkaraChen/open-agent-runtime/internal/manifest"
	"github.com/AkaraChen/open-agent-runtime/internal/nar"
	"github.com/AkaraChen/open-agent-runtime/internal/spec"
)

// runSourceBuild executes the optional per-platform build command of a
// source before it is imported.
func (b *Builder) runSourceBuild(name string, src *spec.Source, p spec.Platform) error {
	if len(src.Build) == 0 {
		return nil
	}
	args := make([]string, len(src.Build))
	for i, a := range src.Build {
		args[i] = spec.Expand(a, p)
	}
	b.srcMu.Lock()
	defer b.srcMu.Unlock()
	cmd := exec.CommandContext(b.ctx, args[0], args[1:]...)
	cmd.Dir = b.cfg.Dir
	cmd.Env = append(os.Environ(), "OAR_ARCH="+p.Arch, "OAR_OS="+p.OS, "OAR_PLATFORM="+p.String())
	for k, v := range src.BuildEnv {
		cmd.Env = append(cmd.Env, k+"="+spec.Expand(v, p))
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build source %s: %w\n%s", name, err, out.String())
	}
	return nil
}

// buildComponent imports a binary, file or directory into a deterministic
// tree.tar (uid/gid 0, fixed mtime) and records its metadata.
func (b *Builder) buildComponent(ref spec.Ref, p spec.Platform) (*manifest.Component, error) {
	src := b.cfg.Sources[ref.Name]
	if err := b.runSourceBuild(ref.Name, src, p); err != nil {
		return nil, err
	}
	inline := src.Content != nil
	var srcPath string
	var st os.FileInfo
	var err error
	if !inline {
		srcPath = spec.Expand(src.Path, p)
		if !filepath.IsAbs(srcPath) {
			srcPath = filepath.Join(b.cfg.Dir, srcPath)
		}
		if st, err = os.Stat(srcPath); err != nil {
			return nil, fmt.Errorf("component %s: %w", ref, err)
		}
	}
	target := src.Target
	if target == "" {
		target = "/opt/" + ref.Name
	}
	if !path.IsAbs(target) {
		return nil, fmt.Errorf("component %s: target %q must be absolute", ref, target)
	}
	target = path.Clean(target)
	switch {
	case inline:
	case ref.Kind == spec.KindBinary || ref.Kind == spec.KindFile:
		if st.IsDir() {
			return nil, fmt.Errorf("component %s: %s is a directory (use dir:)", ref, srcPath)
		}
	case ref.Kind == spec.KindDir:
		if !st.IsDir() {
			return nil, fmt.Errorf("component %s: %s is not a directory", ref, srcPath)
		}
	}

	dir := filepath.Join(b.opts.Artifacts, "components", ref.Name, p.Slug())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, "tree.*.tmp")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(tmp, h)}
	tw := tar.NewWriter(cw)
	c := &manifest.Component{
		Name:        ref.Name,
		Kind:        string(ref.Kind),
		Target:      target,
		Env:         src.Env,
		PathEntries: src.PathEntries,
		Override:    src.Override,
	}
	// parent directories of the target
	parts := strings.Split(strings.TrimPrefix(target, "/"), "/")
	for i := 1; i < len(parts); i++ {
		d := strings.Join(parts[:i], "/")
		if err := writeDir(tw, d, 0o755); err != nil {
			return nil, err
		}
		c.Files = append(c.Files, d+"/")
	}
	addFile := func(rel, abs string, mode int64) error {
		info, err := os.Lstat(abs)
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(rel, "/")
		hdr := &tar.Header{Name: name, ModTime: nar.Epoch, Format: tar.FormatPAX}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			l, err := os.Readlink(abs)
			if err != nil {
				return err
			}
			hdr.Typeflag, hdr.Linkname, hdr.Mode = tar.TypeSymlink, l, 0o777
			c.Files = append(c.Files, name)
			return tw.WriteHeader(hdr)
		case info.IsDir():
			c.Files = append(c.Files, name+"/")
			return writeDir(tw, name, 0o755)
		case info.Mode().IsRegular():
			if mode == 0 {
				mode = int64(info.Mode().Perm())
			}
			hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeReg, mode, info.Size()
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			f, err := os.Open(abs)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.Copy(tw, f); err != nil {
				return err
			}
			c.Files = append(c.Files, name)
			if ei, ok := inspectELF(f); ok {
				ei.Path = "/" + name
				c.ELF = append(c.ELF, ei)
			}
			return nil
		}
		return fmt.Errorf("%s: unsupported file type %s", abs, info.Mode().Type())
	}

	var fileMode int64
	if src.Mode != "" {
		m, err := strconv.ParseInt(src.Mode, 8, 32)
		if err != nil {
			return nil, fmt.Errorf("component %s: bad mode %q", ref, src.Mode)
		}
		fileMode = m
	} else if ref.Kind == spec.KindBinary {
		fileMode = 0o755
	}

	if ref.Kind == spec.KindDir {
		var rels []string
		err = filepath.WalkDir(srcPath, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(srcPath, p)
			rels = append(rels, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			return nil, err
		}
		sort.Strings(rels)
		for _, rel := range rels {
			containerPath := path.Join(target, rel)
			if err := addFile(containerPath, filepath.Join(srcPath, filepath.FromSlash(rel)), 0); err != nil {
				return nil, err
			}
		}
	} else if inline {
		if fileMode == 0 {
			fileMode = 0o644
		}
		data := []byte(*src.Content)
		name := strings.TrimPrefix(target, "/")
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: fileMode, Size: int64(len(data)), ModTime: nar.Epoch, Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
		c.Files = append(c.Files, name)
		if ei, ok := inspectELF(bytes.NewReader(data)); ok {
			ei.Path = target
			c.ELF = append(c.ELF, ei)
		}
	} else if err := addFile(target, srcPath, fileMode); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	// Content addressed: configs (or versions) that share a component name
	// but differ in content must not overwrite each other's artifacts.
	c.Key = "sha256:" + hex.EncodeToString(h.Sum(nil))
	keyHex := hex.EncodeToString(h.Sum(nil))
	treePath := filepath.Join(dir, keyHex+".tar")
	fresh := false
	if st, err := os.Stat(treePath); err != nil || st.Size() != cw.n {
		if err := os.Rename(tmp.Name(), treePath); err != nil {
			return nil, err
		}
		fresh = true
	}
	c.Size = cw.n
	c.Path = filepath.ToSlash(filepath.Join("components", ref.Name, p.Slug(), keyHex+".tar"))
	sort.Strings(c.Files)
	if fresh {
		b.logf("component %s (%s): %d entries, %s", ref, p, len(c.Files), humanSize(c.Size))
	}
	if err := manifest.WriteJSON(filepath.Join(dir, keyHex+".json"), c); err != nil {
		return nil, err
	}
	return c, nil
}

func writeDir(tw *tar.Writer, name string, mode int64) error {
	return tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeDir, Name: strings.TrimSuffix(name, "/") + "/",
		Mode: mode, ModTime: nar.Epoch, Format: tar.FormatPAX,
	})
}

func inspectELF(r io.ReaderAt) (manifest.ELFInfo, bool) {
	f, err := elf.NewFile(r)
	if err != nil {
		return manifest.ELFInfo{}, false
	}
	ei := manifest.ELFInfo{Machine: elfMachine(f.Machine), Static: true}
	for _, prog := range f.Progs {
		if prog.Type == elf.PT_INTERP {
			b := make([]byte, prog.Filesz)
			if _, err := prog.ReadAt(b, 0); err == nil {
				ei.Interpreter = strings.TrimRight(string(b), "\x00")
				ei.Static = false
			}
		}
	}
	if libs, err := f.ImportedLibraries(); err == nil {
		ei.Needed = libs
	}
	return ei, true
}

func elfMachine(m elf.Machine) string {
	switch m {
	case elf.EM_X86_64:
		return "amd64"
	case elf.EM_AARCH64:
		return "arm64"
	case elf.EM_386:
		return "386"
	case elf.EM_ARM:
		return "arm"
	case elf.EM_RISCV:
		return "riscv64"
	}
	return m.String()
}
