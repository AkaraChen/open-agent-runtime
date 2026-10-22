//go:build linux

package agent

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// tarFile is a tar archive on the agent's own disk (native backend), read
// through a buffer with a position kept in memory: archive/tar skips file
// data with Seek, which here costs nothing, and the position after Next is
// where the entry's data starts in the file, so extraction can copy it
// file to file inside the kernel (copy_file_range) instead of reading it
// into the process and writing it back out.
type tarFile struct {
	f    *os.File
	size int64
	pos  int64
	buf  []byte // file bytes [bufOff, bufOff+len(buf))
	off  int64
}

func newTarFile(f *os.File) (*tarFile, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return &tarFile{f: f, size: st.Size()}, nil
}

const tarFileBuf = 64 << 10

func (t *tarFile) Read(p []byte) (int, error) {
	if t.pos >= t.size {
		return 0, io.EOF
	}
	if rest := t.size - t.pos; int64(len(p)) > rest {
		p = p[:rest]
	}
	if t.pos >= t.off && t.pos < t.off+int64(len(t.buf)) {
		n := copy(p, t.buf[t.pos-t.off:])
		t.pos += int64(n)
		return n, nil
	}
	if len(p) >= tarFileBuf { // large reads bypass the buffer
		n, err := t.f.ReadAt(p, t.pos)
		t.pos += int64(n)
		if n > 0 && errors.Is(err, io.EOF) {
			err = nil
		}
		return n, err
	}
	if t.buf == nil {
		t.buf = make([]byte, 0, tarFileBuf)
	}
	n, err := t.f.ReadAt(t.buf[:tarFileBuf], t.pos)
	if n == 0 {
		return 0, err
	}
	t.buf, t.off = t.buf[:n], t.pos
	n = copy(p, t.buf)
	t.pos += int64(n)
	return n, nil
}

func (t *tarFile) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		off += t.pos
	case io.SeekEnd:
		off += t.size
	}
	if off < 0 {
		return 0, errors.New("negative seek")
	}
	t.pos = off
	return off, nil
}

// copyTo copies n bytes starting at the file offset off into dst.
func (t *tarFile) copyTo(dst *fileWriter, off, n int64) error {
	for n > 0 {
		c, err := unix.CopyFileRange(int(t.f.Fd()), &off, dst.fd, nil, int(min(n, 1<<30)), 0)
		if c == 0 && err == nil {
			return io.ErrUnexpectedEOF
		}
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			// not supported between these files (EXDEV on older kernels,
			// EINVAL, ENOSYS...): copy what is left the ordinary way
			return dst.copyFrom(io.NewSectionReader(t.f, off, n))
		}
		off += int64(c)
		n -= int64(c)
	}
	return nil
}
