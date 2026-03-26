package rule

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFSCache_ReadDir(t *testing.T) {
	dir := t.TempDir()

	// Create a couple of test files so ReadDir returns something.
	if err := os.WriteFile(filepath.Join(dir, "file1.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("creating file1: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatalf("creating subdir: %v", err)
	}

	cache := NewFSCache(5*time.Second, 100, slog.Default())

	// First call should fetch from filesystem.
	entries, err := cache.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 entries, got %d", len(entries))
	}

	// Verify entry properties.
	entryMap := make(map[string]DirEntry)
	for _, e := range entries {
		entryMap[e.Name] = e
	}
	if e, ok := entryMap["file1.txt"]; !ok || e.IsDir {
		t.Errorf("expected file1.txt as non-dir, got %+v (found=%v)", e, ok)
	}
	if e, ok := entryMap["subdir"]; !ok || !e.IsDir {
		t.Errorf("expected subdir as dir, got %+v (found=%v)", e, ok)
	}

	// Second call should return cached data (verify by checking cache length).
	if cache.Len() != 1 { // 1 dir entry cached
		t.Errorf("expected cache length 1, got %d", cache.Len())
	}
	entries2, err := cache.ReadDir(dir)
	if err != nil {
		t.Fatalf("second ReadDir: %v", err)
	}
	if len(entries2) != 2 {
		t.Errorf("expected 2 entries from cache, got %d", len(entries2))
	}

	// Add a new file on disk -- cached result should still show old data.
	if err := os.WriteFile(filepath.Join(dir, "file2.txt"), []byte("world"), 0o644); err != nil {
		t.Fatalf("creating file2: %v", err)
	}
	entries3, err := cache.ReadDir(dir)
	if err != nil {
		t.Fatalf("third ReadDir: %v", err)
	}
	// Should still see 2 (cached), not 3 (fresh).
	if len(entries3) != 2 {
		t.Errorf("expected 2 entries from cache (not refreshed), got %d", len(entries3))
	}
}

func TestFSCache_Stat(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(filePath, []byte("hello world"), 0o644); err != nil {
		t.Fatalf("creating test file: %v", err)
	}

	cache := NewFSCache(5*time.Second, 100, slog.Default())

	// First call should fetch from filesystem.
	info, err := cache.Stat(filePath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != 11 {
		t.Errorf("expected size 11, got %d", info.Size)
	}
	if info.IsDir {
		t.Error("expected IsDir=false for a file")
	}
	if info.ModTime.IsZero() {
		t.Error("expected non-zero ModTime")
	}

	// Verify entry is cached.
	if cache.Len() != 1 { // 1 stat entry cached
		t.Errorf("expected cache length 1, got %d", cache.Len())
	}

	// Second call should return cached data.
	info2, err := cache.Stat(filePath)
	if err != nil {
		t.Fatalf("second Stat: %v", err)
	}
	if info2.Size != 11 {
		t.Errorf("expected cached size 11, got %d", info2.Size)
	}
}

func TestFSCache_Stat_CachesErrors(t *testing.T) {
	cache := NewFSCache(5*time.Second, 100, slog.Default())

	nonexistent := filepath.Join(t.TempDir(), "does-not-exist.txt")

	// First call should return an error and cache it.
	_, err := cache.Stat(nonexistent)
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}

	// The error should be cached.
	if cache.Len() != 1 {
		t.Errorf("expected cache length 1 (error cached), got %d", cache.Len())
	}

	// Second call should return the cached error without hitting the filesystem.
	_, err2 := cache.Stat(nonexistent)
	if err2 == nil {
		t.Fatal("expected cached error for nonexistent file")
	}
}

func TestFSCache_InvalidatePath(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(filePath, []byte("hello"), 0o644); err != nil {
		t.Fatalf("creating test file: %v", err)
	}

	cache := NewFSCache(5*time.Second, 100, slog.Default())

	// Populate both caches.
	if _, err := cache.ReadDir(dir); err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if _, err := cache.Stat(filePath); err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if cache.Len() != 2 { // 1 dir + 1 stat
		t.Errorf("expected cache length 2, got %d", cache.Len())
	}

	// Invalidate the file path. This should remove the stat entry and
	// also invalidate the parent directory listing.
	cache.InvalidatePath(filePath)

	if cache.Len() != 0 {
		t.Errorf("expected cache length 0 after invalidation, got %d", cache.Len())
	}

	// Add a new file to the directory.
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new"), 0o644); err != nil {
		t.Fatalf("creating new file: %v", err)
	}

	// Re-reading the directory should now show the new file.
	entries, err := cache.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir after invalidation: %v", err)
	}
	if len(entries) != 2 { // test.txt + new.txt
		t.Errorf("expected 2 entries after invalidation, got %d", len(entries))
	}
}

func TestFSCache_InvalidateAll(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(filePath, []byte("hello"), 0o644); err != nil {
		t.Fatalf("creating test file: %v", err)
	}

	cache := NewFSCache(5*time.Second, 100, slog.Default())

	// Populate caches.
	if _, err := cache.ReadDir(dir); err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if _, err := cache.Stat(filePath); err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if cache.Len() != 2 {
		t.Errorf("expected cache length 2, got %d", cache.Len())
	}

	// Clear everything.
	cache.InvalidateAll()

	if cache.Len() != 0 {
		t.Errorf("expected cache length 0 after InvalidateAll, got %d", cache.Len())
	}
}

func TestFSCache_TTLExpiry(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("data"), 0o644); err != nil {
		t.Fatalf("creating file: %v", err)
	}

	// Use a very short TTL so entries expire quickly.
	cache := NewFSCache(50*time.Millisecond, 100, slog.Default())

	// Populate the cache.
	entries, err := cache.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 entry, got %d", len(entries))
	}

	// Add a new file while the cache is still fresh.
	if err := os.WriteFile(filepath.Join(dir, "file2.txt"), []byte("data2"), 0o644); err != nil {
		t.Fatalf("creating file2: %v", err)
	}

	// Wait for TTL to expire.
	time.Sleep(100 * time.Millisecond)

	// After expiry, ReadDir should re-fetch and see the new file.
	entries2, err := cache.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir after TTL expiry: %v", err)
	}
	if len(entries2) != 2 {
		t.Errorf("expected 2 entries after TTL expiry, got %d", len(entries2))
	}
}

func TestFSCache_MaxSize(t *testing.T) {
	// Create a cache with a very small max size.
	cache := NewFSCache(5*time.Second, 3, slog.Default())

	// Create 5 directories and cache their listings.
	dirs := make([]string, 5)
	for i := range dirs {
		dirs[i] = t.TempDir()
		if _, err := cache.ReadDir(dirs[i]); err != nil {
			t.Fatalf("ReadDir[%d]: %v", i, err)
		}
	}

	// The dir cache should have at most 3 entries (maxSize).
	cache.mu.RLock()
	dirCount := len(cache.dirs)
	cache.mu.RUnlock()

	if dirCount > 3 {
		t.Errorf("expected at most 3 dir cache entries, got %d", dirCount)
	}

	// The most recent entries should still be cached.
	// Entries for dirs[0] and dirs[1] should have been evicted.
	cache.mu.RLock()
	_, hasOldest := cache.dirs[dirs[0]]
	_, hasNewest := cache.dirs[dirs[4]]
	cache.mu.RUnlock()

	if hasOldest {
		t.Error("expected oldest entry (dirs[0]) to be evicted")
	}
	if !hasNewest {
		t.Error("expected newest entry (dirs[4]) to be present")
	}
}

func TestFSCache_NilSafe(t *testing.T) {
	// Engine with nil FSCache should fall back to direct OS calls.
	engine := &Engine{logger: slog.Default()}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "test.txt"), []byte("data"), 0o644); err != nil {
		t.Fatalf("creating file: %v", err)
	}

	// readDirCached should work without a cache.
	entries, err := engine.readDirCached(dir)
	if err != nil {
		t.Fatalf("readDirCached: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 entry, got %d", len(entries))
	}

	// fileModTimeCached should work without a cache.
	filePath := filepath.Join(dir, "test.txt")
	modTime, err := engine.fileModTimeCached(filePath)
	if err != nil {
		t.Fatalf("fileModTimeCached: %v", err)
	}
	if modTime.IsZero() {
		t.Error("expected non-zero modification time")
	}
}

func TestFSCache_ReadDir_ErrorNotCached(t *testing.T) {
	cache := NewFSCache(5*time.Second, 100, slog.Default())

	nonexistent := filepath.Join(t.TempDir(), "nonexistent-dir")

	// Reading a nonexistent directory should return an error.
	_, err := cache.ReadDir(nonexistent)
	if err == nil {
		t.Fatal("expected error for nonexistent directory")
	}

	// ReadDir errors are NOT cached (unlike Stat errors), because a directory
	// not existing is a transient state that changes when the directory is created.
	if cache.Len() != 0 {
		t.Errorf("expected cache length 0 (ReadDir errors not cached), got %d", cache.Len())
	}
}

func TestFSCache_MaxSize_StatEviction(t *testing.T) {
	cache := NewFSCache(5*time.Second, 3, slog.Default())

	// Create 5 files and cache their stats.
	dir := t.TempDir()
	files := make([]string, 5)
	for i := range files {
		files[i] = filepath.Join(dir, filepath.Base(t.TempDir()))
		if err := os.WriteFile(files[i], []byte("data"), 0o644); err != nil {
			t.Fatalf("creating file[%d]: %v", i, err)
		}
		if _, err := cache.Stat(files[i]); err != nil {
			t.Fatalf("Stat[%d]: %v", i, err)
		}
	}

	// The stat cache should have at most 3 entries (maxSize).
	cache.mu.RLock()
	statCount := len(cache.stats)
	cache.mu.RUnlock()

	if statCount > 3 {
		t.Errorf("expected at most 3 stat cache entries, got %d", statCount)
	}
}

func TestBuildLowerToActual(t *testing.T) {
	entries := []DirEntry{
		{Name: "Folder.JPG", IsDir: false},
		{Name: "subdir", IsDir: true},
		{Name: "Artist.nfo", IsDir: false},
	}

	m := buildLowerToActual(entries)

	// Directories should be excluded.
	if _, ok := m["subdir"]; ok {
		t.Error("expected directories to be excluded from lookup map")
	}

	// Files should be indexed by lowercase name.
	if actual, ok := m["folder.jpg"]; !ok || actual != "Folder.JPG" {
		t.Errorf("expected Folder.JPG for folder.jpg key, got %q (found=%v)", actual, ok)
	}
	if actual, ok := m["artist.nfo"]; !ok || actual != "Artist.nfo" {
		t.Errorf("expected Artist.nfo for artist.nfo key, got %q (found=%v)", actual, ok)
	}
}
