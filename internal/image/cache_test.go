package image

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func writeTestFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// --- CacheStats tests ---

func TestCacheStats_Empty(t *testing.T) {
	dir := t.TempDir()
	size, files, artists, err := CacheStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	if size != 0 || files != 0 || artists != 0 {
		t.Fatalf("expected all zeros, got size=%d files=%d artists=%d", size, files, artists)
	}
}

func TestCacheStats_WithFiles(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "artist1", "folder.jpg"), 1000)
	writeTestFile(t, filepath.Join(dir, "artist1", "fanart.jpg"), 2000)
	writeTestFile(t, filepath.Join(dir, "artist2", "folder.jpg"), 500)

	size, files, artists, err := CacheStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	if size != 3500 {
		t.Fatalf("expected size=3500, got %d", size)
	}
	if files != 3 {
		t.Fatalf("expected files=3, got %d", files)
	}
	if artists != 2 {
		t.Fatalf("expected artists=2, got %d", artists)
	}
}

func TestCacheStats_SkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "artist1", "folder.jpg")
	writeTestFile(t, real, 1000)
	link := filepath.Join(dir, "artist1", "artist.jpg")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks not supported")
	}

	_, files, _, err := CacheStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	if files != 1 {
		t.Fatalf("expected files=1 (symlink skipped), got %d", files)
	}
}

func TestCacheStats_NonexistentDir(t *testing.T) {
	size, files, artists, err := CacheStats("/nonexistent/path")
	if err != nil {
		t.Fatal(err)
	}
	if size != 0 || files != 0 || artists != 0 {
		t.Fatalf("expected all zeros for missing dir, got size=%d files=%d artists=%d", size, files, artists)
	}
}

// --- EnforceCacheLimit tests ---

func TestEnforceCacheLimit_UnderLimit(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a1", "folder.jpg"), 500)

	err := EnforceCacheLimit(dir, 1000, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}

	// File should still exist.
	if _, err := os.Stat(filepath.Join(dir, "a1", "folder.jpg")); err != nil {
		t.Fatal("file should not be evicted when under limit")
	}
}

func TestEnforceCacheLimit_OverLimit_EvictsOldest(t *testing.T) {
	dir := t.TempDir()

	// Create two files: old (1000 bytes) and new (1000 bytes). Limit = 1200.
	oldFile := filepath.Join(dir, "a1", "fanart.jpg")
	newFile := filepath.Join(dir, "a1", "folder.jpg")
	writeTestFile(t, oldFile, 1000)

	// Ensure different mtime by advancing file time.
	oldTime := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(oldFile, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	writeTestFile(t, newFile, 1000)

	err := EnforceCacheLimit(dir, 1200, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}

	// Old file should be evicted, new file should remain.
	if _, err := os.Stat(oldFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old file should be evicted")
	}
	if _, err := os.Stat(newFile); err != nil {
		t.Fatal("new file should be preserved")
	}
}

func TestEnforceCacheLimit_Unlimited(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a1", "folder.jpg"), 5000)

	err := EnforceCacheLimit(dir, 0, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}

	// File should still exist (unlimited mode).
	if _, err := os.Stat(filepath.Join(dir, "a1", "folder.jpg")); err != nil {
		t.Fatal("file should not be evicted in unlimited mode")
	}
}

func TestEnforceCacheLimit_CleansEmptyDirs(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a1", "folder.jpg"), 1000)

	// Evict everything (limit = 1 byte).
	err := EnforceCacheLimit(dir, 1, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}

	// The a1 subdirectory should be removed since it is now empty.
	if _, err := os.Stat(filepath.Join(dir, "a1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty artist directory should be removed after eviction")
	}
}

func TestEnforceCacheLimit_ConcurrentCallers(t *testing.T) {
	dir := t.TempDir()

	// Create 10 files across 5 artist dirs, 1000 bytes each = 10000 total.
	for i := 0; i < 10; i++ {
		artistDir := fmt.Sprintf("a%d", i/2)
		file := fmt.Sprintf("file%d.jpg", i%2)
		writeTestFile(t, filepath.Join(dir, artistDir, file), 1000)
	}

	limit := int64(5000) // Should evict roughly half.
	var wg sync.WaitGroup
	errs := make([]error, 2)

	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = EnforceCacheLimit(dir, limit, testLogger(t))
		}(g)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d returned error: %v", i, err)
		}
	}

	// Verify total size is within limit.
	totalSize, _, _, err := CacheStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	if totalSize > limit {
		t.Fatalf("expected total <= %d, got %d", limit, totalSize)
	}
}
