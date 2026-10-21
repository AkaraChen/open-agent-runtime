//go:build linux

package agent

import (
	"sync"
	"testing"
	"time"
)

// The writeback loop follows the number of uploads in progress: it keeps
// running while any is active (however long) and stops after the last.
func TestWritebackLifecycle(t *testing.T) {
	s := &Server{cfg: Config{Root: t.TempDir()}}
	active := func() bool {
		writebackMu.Lock()
		defer writebackMu.Unlock()
		return writebackActive
	}
	var wg sync.WaitGroup
	done := make([]func(), 4)
	for i := range done {
		wg.Add(1)
		go func() { defer wg.Done(); done[i] = s.beginWriteback() }()
	}
	wg.Wait()
	// longer than the idle period with uploads still running
	time.Sleep(2 * writebackIdle)
	if !active() {
		t.Fatal("loop stopped while uploads were in progress")
	}
	for _, d := range done {
		d()
	}
	deadline := time.Now().Add(5 * time.Second)
	for active() {
		if time.Now().After(deadline) {
			t.Fatal("loop still running after all uploads finished")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// restarts for a later upload
	d := s.beginWriteback()
	if !active() {
		t.Fatal("loop not restarted")
	}
	d()
}
