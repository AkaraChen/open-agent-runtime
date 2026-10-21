package vm

import (
	"os"
	"path/filepath"
	"testing"
)

func head(t *testing.T, p string) string {
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := make([]byte, 10)
	f.Read(b)
	return string(b)
}

func TestDataDiskUncleanShutdown(t *testing.T) {
	dir := t.TempDir()
	disk, err := OpenDataDisk(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(disk, os.O_WRONLY, 0)
	f.WriteString("guest data")
	f.Close()

	// a clean shutdown keeps the image
	CloseDataDisk(dir)
	if fileExists(filepath.Join(dir, diskMarker)) {
		t.Fatal("marker left behind after a clean shutdown")
	}
	if disk, err = OpenDataDisk(dir, 1); err != nil {
		t.Fatal(err)
	}
	if got := head(t, disk); got != "guest data" {
		t.Fatalf("image discarded after a clean shutdown: %q", got)
	}

	// no CloseDataDisk (killed supervisor, host crash): a fresh image
	if disk, err = OpenDataDisk(dir, 1); err != nil {
		t.Fatal(err)
	}
	if got := head(t, disk); got != string(make([]byte, 10)) {
		t.Fatalf("image kept after an unclean shutdown: %q", got)
	}
}
