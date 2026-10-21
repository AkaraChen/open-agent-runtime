//go:build linux

package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AkaraChen/open-agent-runtime/internal/compose"
)

// A new closure usually differs from an older one by a few store paths, yet
// its view is another tree of every file. Building it costs a link per file;
// moving the store path directories out of an unused older view costs one
// rename each. So a view being built takes the subtrees it needs from the
// unused view that shares the most with it, and links only the rest. The
// donor is then rebuilt in the background (the moved store paths linked
// back from the cache), so the old closure is whole again by the time
// anyone rolls back to it: the work is the same as before, but the view
// that is needed first is ready first.
//
// A view that a container has mounted is never touched: a bind mount shows
// the directory's current entries, so moving one out would remove the store
// path from under a running process. instantiate leaves a marker file in
// the instance directory before it asks for the view, and the marker lives
// as long as the instance does (running, prepared, hot standby, or about to
// be restored from a snapshot, which asks for its view again).
//
// Crash safety: the donor loses its .done marker before anything moves, and
// every other state is a directory the next start removes or rebuilds:
// views are only ever published complete (rename of a finished temporary
// directory, then the marker), and a subtree moves as a whole.

// viewMarker names the file in an instance directory that holds the path of
// the store view its container mounts.
const viewMarker = "storeview"

// minReused is the fewest store paths worth taking from a donor.
const minReused = 8

// viewsInUse returns the store views that an instance mounts or is about to.
func (s *Server) viewsInUse() map[string]bool {
	use := map[string]bool{}
	entries, _ := os.ReadDir(s.layout.InstancesDir())
	for _, e := range entries {
		dir := s.instDir(e.Name())
		if b, err := os.ReadFile(filepath.Join(dir, viewMarker)); err == nil {
			use[string(b)] = true
			continue
		}
		// created before the marker existed
		var in instance
		if b, err := os.ReadFile(filepath.Join(dir, "instance.json")); err != nil || json.Unmarshal(b, &in) != nil {
			continue
		}
		var bundle compose.Bundle
		if b, err := os.ReadFile(filepath.Join(s.layout.BundleDir(in.BundleDigest), "bundle.json")); err == nil && json.Unmarshal(b, &bundle) == nil && bundle.StoreView != "" {
			use[bundle.StoreView] = true
		}
	}
	return use
}

// stealViews moves the store paths that layers need out of the unused view
// that holds the most of them into tmp, and returns their names. The donor
// is rebuilt in the background. The caller holds groups.mu.
func (s *Server) stealViews(tmp string, layers []string, view string) map[string]bool {
	needed := make(map[string]bool, len(layers))
	for _, l := range layers {
		needed[filepath.Base(l)] = true
	}
	viewsDir := filepath.Dir(view)
	entries, err := os.ReadDir(viewsDir)
	if err != nil {
		return nil
	}
	var inUse map[string]bool
	var donor string
	var donorNames, shared []string
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(viewsDir, name)
		if !e.IsDir() || strings.HasPrefix(name, "store.tmp-") || path == view || s.groups.builds[path] != nil {
			continue
		}
		if _, err := os.Stat(path + ".done"); err != nil {
			continue
		}
		subs, err := os.ReadDir(path)
		if err != nil {
			continue
		}
		var names, common []string
		for _, sub := range subs {
			names = append(names, sub.Name())
			if needed[sub.Name()] {
				common = append(common, sub.Name())
			}
		}
		if len(common) < minReused || len(common)*2 < len(needed) || len(common) <= len(shared) {
			continue
		}
		if inUse == nil {
			inUse = s.viewsInUse()
		}
		if inUse[path] {
			continue
		}
		donor, donorNames, shared = path, names, common
	}
	if donor == "" {
		return nil
	}
	// out of sight first: from here on the donor is not a complete view
	if err := os.Remove(donor + ".done"); err != nil {
		return nil
	}
	delete(s.groups.mounted, donor)
	var rnd [6]byte
	rand.Read(rnd[:])
	held := filepath.Join(viewsDir, "store.tmp-"+hex.EncodeToString(rnd[:]))
	if err := os.Rename(donor, held); err != nil {
		os.WriteFile(donor+".done", nil, 0o644)
		s.groups.mounted[donor] = true
		return nil
	}
	moved := map[string]bool{}
	for _, name := range shared {
		if os.Rename(filepath.Join(held, name), filepath.Join(tmp, name)) == nil {
			moved[name] = true
		}
	}
	// the donor is what is left, and gets back what it gave away
	vb := &viewBuild{tmp: held, done: make(chan struct{}), pending: map[string]bool{}}
	var restore []string
	for _, name := range donorNames {
		if moved[name] {
			layer := s.layout.StoreLayer(name)
			vb.pending[layer] = true
			restore = append(restore, layer)
		}
	}
	if s.groups.builds == nil {
		s.groups.builds = map[string]*viewBuild{}
	}
	s.groups.builds[donor] = vb
	vb.mu.Lock()
	vb.finish()
	vb.mu.Unlock()
	go s.restoreView(donor, vb, restore)
	return moved
}

// restoreView links the store paths a donor gave away back into it, one
// after another (a rollback that needs the view meanwhile links the
// remainder in parallel), and publishes it unless ensureStoreView took the
// build over.
func (s *Server) restoreView(view string, vb *viewBuild, layers []string) {
	for _, l := range layers {
		if vb.claim(l) {
			vb.run(l)
		}
	}
	vb.mu.Lock()
	if len(vb.pending) > 0 {
		if vb.err == nil {
			vb.err = fmt.Errorf("store view: %d store paths missing", len(vb.pending))
		}
		vb.pending = nil
		vb.finish()
	}
	vb.mu.Unlock()
	<-vb.done
	s.groups.mu.Lock()
	defer s.groups.mu.Unlock()
	if s.groups.builds[view] != vb {
		return
	}
	delete(s.groups.builds, view)
	if vb.err != nil {
		os.RemoveAll(vb.tmp) // built from scratch when someone asks
		return
	}
	s.publishView(vb.tmp, view, view+".done")
}
