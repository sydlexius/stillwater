package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	img "github.com/sydlexius/stillwater/internal/image"
)

func TestHandleCacheStats_Empty(t *testing.T) {
	r, _ := testRouter(t)
	r.imageCacheDir = t.TempDir()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/cache/stats", nil)
	w := httptest.NewRecorder()
	r.handleCacheStats(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		SizeBytes   int64 `json:"size_bytes"`
		FileCount   int   `json:"file_count"`
		ArtistCount int   `json:"artist_count"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.SizeBytes != 0 || resp.FileCount != 0 || resp.ArtistCount != 0 {
		t.Fatalf("expected all zeros, got %+v", resp)
	}
}

func TestHandleCacheStats_WithFiles(t *testing.T) {
	r, _ := testRouter(t)
	r.imageCacheDir = t.TempDir()

	cacheWriteFile(t, filepath.Join(r.imageCacheDir, "artist1", "folder.jpg"), 1000)
	cacheWriteFile(t, filepath.Join(r.imageCacheDir, "artist2", "folder.jpg"), 2000)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/cache/stats", nil)
	w := httptest.NewRecorder()
	r.handleCacheStats(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp struct {
		SizeBytes   int64 `json:"size_bytes"`
		FileCount   int   `json:"file_count"`
		ArtistCount int   `json:"artist_count"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.SizeBytes != 3000 {
		t.Fatalf("expected size_bytes=3000, got %d", resp.SizeBytes)
	}
	if resp.FileCount != 2 {
		t.Fatalf("expected file_count=2, got %d", resp.FileCount)
	}
	if resp.ArtistCount != 2 {
		t.Fatalf("expected artist_count=2, got %d", resp.ArtistCount)
	}
}

func TestHandleCacheClear_WithFiles(t *testing.T) {
	r, _ := testRouter(t)
	r.imageCacheDir = t.TempDir()

	cacheWriteFile(t, filepath.Join(r.imageCacheDir, "artist1", "folder.jpg"), 1000)
	cacheWriteFile(t, filepath.Join(r.imageCacheDir, "artist2", "folder.jpg"), 2000)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/settings/cache", nil)
	w := httptest.NewRecorder()
	r.handleCacheClear(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		FilesDeleted int   `json:"files_deleted"`
		BytesFreed   int64 `json:"bytes_freed"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.FilesDeleted != 2 {
		t.Fatalf("expected files_deleted=2, got %d", resp.FilesDeleted)
	}
	if resp.BytesFreed != 3000 {
		t.Fatalf("expected bytes_freed=3000, got %d", resp.BytesFreed)
	}

	// Verify directory is empty.
	size, files, _, _ := img.CacheStats(r.imageCacheDir)
	if size != 0 || files != 0 {
		t.Fatalf("cache should be empty after clear, got size=%d files=%d", size, files)
	}
}

func TestHandleCacheClear_Empty(t *testing.T) {
	r, _ := testRouter(t)
	r.imageCacheDir = t.TempDir()

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/settings/cache", nil)
	w := httptest.NewRecorder()
	r.handleCacheClear(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp struct {
		FilesDeleted int   `json:"files_deleted"`
		BytesFreed   int64 `json:"bytes_freed"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.FilesDeleted != 0 || resp.BytesFreed != 0 {
		t.Fatalf("expected zeros, got %+v", resp)
	}
}

func TestHandleCacheClear_ResetsExistsFlag(t *testing.T) {
	r, _ := testRouter(t)
	r.imageCacheDir = t.TempDir()

	ctx := context.Background()

	// Insert a pathless artist directly (path = "").
	artistID := "test-pathless-artist"
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO artists (id, name, path) VALUES (?, ?, '')`,
		artistID, "Pathless Artist")
	if err != nil {
		t.Fatal(err)
	}

	// Insert an artist_images row with exists_flag = 1.
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO artist_images (id, artist_id, image_type, exists_flag)
		 VALUES (?, ?, 'thumb', 1)`,
		"img-"+artistID, artistID)
	if err != nil {
		t.Fatal(err)
	}

	cacheWriteFile(t, filepath.Join(r.imageCacheDir, artistID, "folder.jpg"), 1000)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/settings/cache", nil)
	w := httptest.NewRecorder()
	r.handleCacheClear(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Verify exists_flag was reset to 0.
	var flag int
	err = r.db.QueryRowContext(ctx,
		`SELECT exists_flag FROM artist_images WHERE artist_id = ? AND image_type = 'thumb'`,
		artistID).Scan(&flag)
	if err != nil {
		t.Fatal(err)
	}
	if flag != 0 {
		t.Fatalf("expected exists_flag=0 after cache clear, got %d", flag)
	}
}

// cacheWriteFile creates a file of the given size at path, creating parent dirs.
func cacheWriteFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}
