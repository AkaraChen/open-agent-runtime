//go:build linux

package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/AkaraChen/open-agent-runtime/internal/compose"
)

// A new closure usually differs from an older one by a few store paths, yet
// its view is another tree of every file. Building it costs a link per file;
// moving the store path directories out of an older view costs one rename
// each. So a view being built takes the subtrees it needs from the donor
// that shares the most with it, and links only the rest. The donor is
// invalidated: a later request for its closure builds it again from
// scratch, as for any closure without a view.
//
// A donor is a complete view that has been started and is idle now.
//
// Started means that an instance was created on the view at some point
// (markViewUsed).
//
// PERF: a view that was only warmed ahead is the one the first start of its
// bundle expects to find whole. The first version of this (perf/view-reuse)
// took any complete view that no instance mounted; the benchmark warms every
// combination before it starts any, so the views it had warmed ahead were all
// donors, and the first start after a warm linked the view it had been
// promised: 2-3x slower. With the mark only views that have served an
// instance can be taken, and 4-start/first.settled stays where it was (native, quiet A/B: 52 vs 43 ms). A run,
// reload, reload loop links the view of its first reload (the version it
// replaces is running, and no other view has been started) and takes the
// views of the later ones by rename: 2-warm/reuse.view.next 75-86 -> 4-5 ms on
// native, BenchmarkViewReuse (a closure one store path away from the idle
// one, 120 paths of ~190 files) 46-49 -> 3.1-3.6 ms. Going back to a closure
// that was given away builds its view again, here from another idle view
// (reuse.view.back 2-3 ms; without reuse every view stays and this costs 0).
//
// Idle means that no instance mounts the view. A view that a container has
// mounted is never touched: a bind mount shows the directory's current
// entries, so moving one out would remove the store path from under a
// running process. instantiate leaves a marker file in the instance
// directory before it asks for the view, and the marker lives as long as the
// instance does (running, prepared, hot standby, or about to be restored from
// a snapshot, which asks for its view again). A closure that is still running
// or on standby is therefore always there to roll back to. Between
// createInstance's first request for the view and that marker the view is
// held (holdView) instead.
//
// The mark is a file, <view>.used, next to the view's .done marker, and
// belongs to the closure: one view serves every bundle with that closure.
// It is removed with .done when the view is taken apart. There is no
// collection of views, so a mark stays as long as its view does.
//
// A mark is written under groups.mu, the lock a steal holds when it picks
// its donor, so the two never overlap. Only a view with a .done marker and
// no entry in groups.builds is a candidate: one that is being built or
// published is never picked.
//
// Crash safety: the donor loses its .done marker before anything moves, so
// a view without one is never taken for complete; what is left of it is
// removed by whoever asks for that view next, or with the store.tmp-*
// directories at agent start. Views are only ever published complete
// (rename of a finished temporary directory, then the marker), and a
// subtree moves as a whole.

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

// holdView keeps view from being taken apart until the returned function is
// called. createInstance asks for its view before instantiate writes the
// instance's marker, which protects the view from then on; in between, the
// view of a bundle that was started before is a donor like any other.
func (s *Server) holdView(view string) (release func()) {
	s.groups.heldMu.Lock()
	defer s.groups.heldMu.Unlock()
	if s.groups.held == nil {
		s.groups.held = map[string]int{}
	}
	s.groups.held[view]++
	return func() {
		s.groups.heldMu.Lock()
		defer s.groups.heldMu.Unlock()
		if s.groups.held[view]--; s.groups.held[view] <= 0 {
			delete(s.groups.held, view)
		}
	}
}

// viewHeld reports whether an instance creation holds view.
func (s *Server) viewHeld(view string) bool {
	s.groups.heldMu.Lock()
	defer s.groups.heldMu.Unlock()
	return s.groups.held[view] > 0
}

// markViewUsed records that an instance is being created on view: from now
// on it can be taken apart for another closure whenever no instance mounts
// it. The caller holds groups.mu. The file survives an agent restart; if it
// cannot be written, the view is a donor candidate until then only.
func (s *Server) markViewUsed(view string) {
	if s.groups.used[view] {
		return
	}
	if s.groups.used == nil {
		s.groups.used = map[string]bool{}
	}
	s.groups.used[view] = true
	os.WriteFile(view+".used", nil, 0o644)
}

// viewUsed reports whether view has been marked used. The caller holds
// groups.mu.
func (s *Server) viewUsed(view string) bool {
	if s.groups.used[view] {
		return true
	}
	if _, err := os.Stat(view + ".used"); err != nil {
		return false
	}
	if s.groups.used == nil {
		s.groups.used = map[string]bool{}
	}
	s.groups.used[view] = true
	return true
}

// beginViewBuild is called with groups.mu held when view, which is not
// complete, is about to be built. A mark left by an earlier view of the
// closure (the agent died while taking it apart) does not belong to the new
// one, and a closure whose view a steal took counts as rebuilt.
func (s *Server) beginViewBuild(view string) {
	delete(s.groups.used, view)
	os.Remove(view + ".used")
	if s.groups.donated[view] {
		delete(s.groups.donated, view)
		s.groups.rebuilds.Add(1)
	}
}

// stealViews moves the store paths that layers need out of the donor that
// holds the most of them into tmp, and returns their names. The donor is
// invalidated. The caller holds groups.mu.
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
	var shared []string
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(viewsDir, name)
		if !e.IsDir() || strings.HasPrefix(name, "store.tmp-") || path == view || s.groups.builds[path] != nil {
			continue
		}
		if _, err := os.Stat(path + ".done"); err != nil {
			continue
		}
		if !s.viewUsed(path) {
			continue // warmed ahead, not started yet
		}
		subs, err := os.ReadDir(path)
		if err != nil {
			continue
		}
		var common []string
		for _, sub := range subs {
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
		if inUse[path] || s.viewHeld(path) {
			continue
		}
		donor, shared = path, common
	}
	if donor == "" {
		return nil
	}
	// out of sight first: from here on the donor is not a complete view
	if err := os.Remove(donor + ".done"); err != nil {
		return nil
	}
	// the mark goes with the view; the one that is built for its closure
	// next has not been started
	os.Remove(donor + ".used")
	delete(s.groups.used, donor)
	delete(s.groups.mounted, donor)
	if s.groups.donated == nil {
		s.groups.donated = map[string]bool{}
	}
	s.groups.donated[donor] = true
	s.groups.steals.Add(1)
	// the rest of the donor goes away in the background
	var rnd [6]byte
	rand.Read(rnd[:])
	gone := filepath.Join(viewsDir, "store.tmp-"+hex.EncodeToString(rnd[:]))
	if err := os.Rename(donor, gone); err != nil {
		return nil // left without its marker: removed by the next request
	}
	moved := map[string]bool{}
	for _, name := range shared {
		if os.Rename(filepath.Join(gone, name), filepath.Join(tmp, name)) == nil {
			moved[name] = true
		}
	}
	s.cleanups.Add(1)
	go func() {
		defer s.cleanups.Done()
		os.RemoveAll(gone)
	}()
	return moved
}
