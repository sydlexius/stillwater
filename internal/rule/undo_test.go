package rule

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestUndoStore_RegisterAndPop(t *testing.T) {
	store := NewUndoStore()
	called := false
	revert := func(_ context.Context) error {
		called = true
		return nil
	}

	id := store.Register("v-1", revert)
	if id == "" {
		t.Fatal("expected non-empty undo ID")
	}

	entry, ok := store.Pop(id)
	if !ok {
		t.Fatal("expected Pop to return entry")
	}
	if entry.ViolationID != "v-1" {
		t.Errorf("ViolationID = %q, want %q", entry.ViolationID, "v-1")
	}

	// Execute the revert function to verify it works.
	if err := entry.Revert(context.Background()); err != nil {
		t.Fatalf("revert error: %v", err)
	}
	if !called {
		t.Error("expected revert function to have been called")
	}

	// Popping the same ID again should return nothing (already consumed).
	_, ok2 := store.Pop(id)
	if ok2 {
		t.Error("expected second Pop to return false (already consumed)")
	}
}

func TestUndoStore_PopMissing(t *testing.T) {
	store := NewUndoStore()
	_, ok := store.Pop("nonexistent-id")
	if ok {
		t.Error("expected Pop of nonexistent ID to return false")
	}
}

func TestUndoStore_PopExpired(t *testing.T) {
	store := NewUndoStore()

	revert := func(_ context.Context) error { return nil }
	id := store.Register("v-exp", revert)

	// Force the entry to expire.
	store.ForceExpire(id)

	_, ok := store.Pop(id)
	if ok {
		t.Error("expected Pop of expired entry to return false")
	}
}

func TestUndoStore_Expire(t *testing.T) {
	store := NewUndoStore()

	revert := func(_ context.Context) error { return nil }
	id1 := store.Register("v-1", revert)
	id2 := store.Register("v-2", revert)

	// Force the first entry to expire.
	store.ForceExpire(id1)

	store.Expire()

	store.mu.Lock()
	_, has1 := store.entries[id1]
	_, has2 := store.entries[id2]
	store.mu.Unlock()

	if has1 {
		t.Error("expected expired entry to be removed")
	}
	if !has2 {
		t.Error("expected non-expired entry to remain")
	}
}

func TestUndoStore_ConcurrentAccess(t *testing.T) {
	store := NewUndoStore()
	var wg sync.WaitGroup
	ids := make([]string, 20)

	// Register concurrently.
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i] = store.Register("v-concurrent", func(_ context.Context) error { return nil })
		}(i)
	}
	wg.Wait()

	// Pop concurrently.
	results := make([]bool, 20)
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, ok := store.Pop(ids[i])
			results[i] = ok
		}(i)
	}
	wg.Wait()

	// All should have been popped once (no double-pops since IDs are unique).
	count := 0
	for _, ok := range results {
		if ok {
			count++
		}
	}
	if count != 20 {
		t.Errorf("expected 20 successful pops, got %d", count)
	}
}

// --- FileSnapshot and FileRevert ---

func TestCaptureFile_Exists(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "artist.nfo")
	if err := os.WriteFile(p, []byte("original"), 0o644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	snap, err := CaptureFile(p)
	if err != nil {
		t.Fatalf("CaptureFile error: %v", err)
	}
	if !snap.Exists {
		t.Error("expected Exists=true")
	}
	if string(snap.Content) != "original" {
		t.Errorf("Content = %q, want %q", string(snap.Content), "original")
	}
}

func TestCaptureFile_NotExists(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "missing.nfo")

	snap, err := CaptureFile(p)
	if err != nil {
		t.Fatalf("CaptureFile error: %v", err)
	}
	if snap.Exists {
		t.Error("expected Exists=false for missing file")
	}
}

func TestFileRevert_ModifiedFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "artist.nfo")

	// Write the "before" state.
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatalf("writing original: %v", err)
	}

	// Capture the pre-fix snapshot.
	snap, err := CaptureFile(target)
	if err != nil {
		t.Fatalf("capturing: %v", err)
	}

	// Apply the "fix" (overwrite with new content).
	if err := os.WriteFile(target, []byte("fixed content"), 0o644); err != nil {
		t.Fatalf("applying fix: %v", err)
	}

	// Revert.
	if err := FileRevert(snap)(context.Background()); err != nil {
		t.Fatalf("FileRevert error: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading reverted file: %v", err)
	}
	if string(got) != "original" {
		t.Errorf("restored content = %q, want %q", string(got), "original")
	}
}

func TestFileRevert_NewlyCreatedFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "new.nfo")

	// Snapshot before the file exists.
	snap, err := CaptureFile(target)
	if err != nil {
		t.Fatalf("capturing: %v", err)
	}
	if snap.Exists {
		t.Error("expected Exists=false before file creation")
	}

	// Apply the "fix" (create the file).
	if err := os.WriteFile(target, []byte("created by fix"), 0o644); err != nil {
		t.Fatalf("creating file: %v", err)
	}

	// Revert should remove the file.
	if err := FileRevert(snap)(context.Background()); err != nil {
		t.Fatalf("FileRevert error: %v", err)
	}

	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Error("expected file to be removed after revert")
	}
}

func TestMultiFileRevert(t *testing.T) {
	dir := t.TempDir()
	f1 := filepath.Join(dir, "folder.jpg")
	f2 := filepath.Join(dir, "artist.jpg")

	// Capture snapshots: f1 pre-existing, f2 not yet created.
	if err := os.WriteFile(f1, []byte("orig f1"), 0o644); err != nil {
		t.Fatalf("writing f1: %v", err)
	}
	snap1, err := CaptureFile(f1)
	if err != nil {
		t.Fatalf("capturing f1: %v", err)
	}
	snap2, err := CaptureFile(f2)
	if err != nil {
		t.Fatalf("capturing f2: %v", err)
	}

	// Simulate fix: overwrite f1, create f2.
	if err := os.WriteFile(f1, []byte("fixed f1"), 0o644); err != nil {
		t.Fatalf("overwriting f1: %v", err)
	}
	if err := os.WriteFile(f2, []byte("new f2"), 0o644); err != nil {
		t.Fatalf("creating f2: %v", err)
	}

	// Revert both.
	revert := MultiFileRevert([]FileSnapshot{snap1, snap2})
	if err := revert(context.Background()); err != nil {
		t.Fatalf("MultiFileRevert error: %v", err)
	}

	// f1 should be restored to original.
	got1, err := os.ReadFile(f1)
	if err != nil {
		t.Fatalf("reading f1: %v", err)
	}
	if string(got1) != "orig f1" {
		t.Errorf("f1 content = %q, want %q", string(got1), "orig f1")
	}

	// f2 should be removed.
	if _, err := os.Stat(f2); !os.IsNotExist(err) {
		t.Error("expected f2 to be removed after revert")
	}
}

func TestDirectoryRenameRevert(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old-name")
	newPath := filepath.Join(dir, "new-name")

	// Create the directory at newPath to simulate a completed rename.
	if err := os.Mkdir(newPath, 0o755); err != nil {
		t.Fatalf("creating newPath directory: %v", err)
	}

	revert := DirectoryRenameRevert(oldPath, newPath)
	if err := revert(context.Background()); err != nil {
		t.Fatalf("DirectoryRenameRevert error: %v", err)
	}

	if _, err := os.Stat(oldPath); os.IsNotExist(err) {
		t.Error("expected directory to exist at oldPath after revert")
	}
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Error("expected newPath to no longer exist after revert")
	}
}
