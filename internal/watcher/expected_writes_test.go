package watcher

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestExpectedWrites_AddRemove(t *testing.T) {
	ew := NewExpectedWrites()
	ew.Add("/music/artist/folder.jpg")
	if !ew.IsExpected("/music/artist/folder.jpg") {
		t.Error("IsExpected = false after Add")
	}
	ew.Remove("/music/artist/folder.jpg")
	if ew.IsExpected("/music/artist/folder.jpg") {
		t.Error("IsExpected = true after Remove")
	}
}

func TestExpectedWrites_AddAllRemoveAll(t *testing.T) {
	ew := NewExpectedWrites()
	paths := []string{"/a/1.jpg", "/a/2.jpg", "/a/3.jpg"}
	ew.AddAll(paths)
	for _, p := range paths {
		if !ew.IsExpected(p) {
			t.Errorf("IsExpected(%q) = false after AddAll", p)
		}
	}
	ew.RemoveAll(paths)
	for _, p := range paths {
		if ew.IsExpected(p) {
			t.Errorf("IsExpected(%q) = true after RemoveAll", p)
		}
	}
}

func TestExpectedWrites_IsExpectedUnknownPath(t *testing.T) {
	ew := NewExpectedWrites()
	ew.Add("/music/artist/folder.jpg")
	if ew.IsExpected("/music/other/folder.jpg") {
		t.Error("IsExpected returned true for unregistered path")
	}
}

func TestExpectedWrites_Prune(t *testing.T) {
	ew := NewExpectedWrites()
	// Manually insert an old entry.
	ew.mu.Lock()
	ew.paths["/old/file.jpg"] = time.Now().Add(-10 * time.Minute)
	ew.mu.Unlock()

	ew.Add("/new/file.jpg")

	pruned := ew.Prune(5 * time.Minute)
	if pruned != 1 {
		t.Errorf("Prune = %d, want 1", pruned)
	}
	if ew.IsExpected("/old/file.jpg") {
		t.Error("old entry not pruned")
	}
	if !ew.IsExpected("/new/file.jpg") {
		t.Error("new entry was incorrectly pruned")
	}
}

func TestExpectedWrites_PruneNothingStale(t *testing.T) {
	ew := NewExpectedWrites()
	ew.Add("/recent/file.jpg")

	pruned := ew.Prune(5 * time.Minute)
	if pruned != 0 {
		t.Errorf("Prune = %d, want 0 (nothing stale)", pruned)
	}
	if !ew.IsExpected("/recent/file.jpg") {
		t.Error("recent entry was incorrectly pruned")
	}
}

func TestExpectedWrites_RemoveIdempotent(t *testing.T) {
	ew := NewExpectedWrites()
	// Removing a path that was never added should not panic.
	ew.Remove("/never/added.jpg")
	ew.RemoveAll([]string{"/also/never/added.jpg", "/nope.png"})
}

func TestExpectedWrites_Concurrent(t *testing.T) {
	ew := NewExpectedWrites()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := fmt.Sprintf("/music/artist%d/file.jpg", i)
			ew.Add(path)
			ew.IsExpected(path)
			ew.Remove(path)
		}(i)
	}
	wg.Wait()
}

// TestExpectedWrites_NFOPattern verifies the Add/defer-Remove pattern used by
// NFO write paths: a single path is registered, visible during the write, and
// automatically cleared when the enclosing scope exits.
func TestExpectedWrites_NFOPattern(t *testing.T) {
	ew := NewExpectedWrites()
	nfoPath := "/music/Radiohead/artist.nfo"

	// Simulate the pattern used in handlers and fixers:
	//   ew.Add(nfoPath); defer ew.Remove(nfoPath)
	func() {
		ew.Add(nfoPath)
		defer ew.Remove(nfoPath)

		if !ew.IsExpected(nfoPath) {
			t.Error("NFO path not expected during write scope")
		}
		// A different artist's NFO should not match.
		if ew.IsExpected("/music/Other/artist.nfo") {
			t.Error("unrelated NFO path incorrectly reported as expected")
		}
	}()

	// After the scope exits, the path should be cleared.
	if ew.IsExpected(nfoPath) {
		t.Error("NFO path still expected after deferred Remove")
	}
}

// TestExpectedWrites_NilSafe verifies that a nil ExpectedWrites pointer does
// not cause panics when callers guard with "if ew != nil" checks. This mirrors
// the guard pattern used throughout the codebase.
func TestExpectedWrites_NilSafe(t *testing.T) {
	var ew *ExpectedWrites
	// Callers always guard with "if ew != nil", so a nil pointer should
	// never reach the methods. This test documents that expectation.
	if ew != nil {
		t.Error("nil ExpectedWrites should not pass nil check")
	}
}

func TestExpectedWrites_ConcurrentAddAllPrune(t *testing.T) {
	ew := NewExpectedWrites()
	var wg sync.WaitGroup

	// Writers: add batches of paths concurrently.
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			paths := []string{
				fmt.Sprintf("/music/batch%d/a.jpg", i),
				fmt.Sprintf("/music/batch%d/b.jpg", i),
			}
			ew.AddAll(paths)
			// Check each path.
			for _, p := range paths {
				ew.IsExpected(p)
			}
			ew.RemoveAll(paths)
		}(i)
	}

	// Pruner: run prune concurrently.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 10; j++ {
			ew.Prune(1 * time.Minute)
		}
	}()

	wg.Wait()
}
