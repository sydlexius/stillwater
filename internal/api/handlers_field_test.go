package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/nfo"
)

func TestHandleFieldUpdate_WritesBackNFO(t *testing.T) {
	r, artistSvc := testRouter(t)

	dir := t.TempDir()
	a := addTestArtist(t, artistSvc, "NFO Writer")

	// Set path and NFOExists on the artist so write-back is triggered
	a.Path = dir
	a.NFOExists = true
	a.Biography = "Original bio"
	if err := artistSvc.Update(context.Background(), a); err != nil {
		t.Fatalf("updating artist: %v", err)
	}

	// Seed an initial artist.nfo on disk
	seedNFO := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<artist><name>NFO Writer</name><biography>Original bio</biography></artist>\n"
	if err := os.WriteFile(filepath.Join(dir, "artist.nfo"), []byte(seedNFO), 0o644); err != nil {
		t.Fatalf("writing seed nfo: %v", err)
	}

	// PATCH the biography field
	body := strings.NewReader("value=Updated biography text")
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/artists/"+a.ID+"/fields/biography", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", a.ID)
	req.SetPathValue("field", "biography")
	w := httptest.NewRecorder()

	r.handleFieldUpdate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Read the NFO file back and verify the biography was updated
	data, err := os.ReadFile(filepath.Join(dir, "artist.nfo"))
	if err != nil {
		t.Fatalf("reading nfo: %v", err)
	}
	parsed, err := nfo.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parsing nfo: %v", err)
	}
	if parsed.Biography != "Updated biography text" {
		t.Errorf("NFO biography = %q, want %q", parsed.Biography, "Updated biography text")
	}
}

func TestHandleFieldUpdate_NoPath_SkipsWriteBack(t *testing.T) {
	r, artistSvc := testRouter(t)

	a := addTestArtist(t, artistSvc, "No Path Artist")
	// Path is empty string by default from addTestArtist -- override to ""
	a.Path = ""
	if err := artistSvc.Update(context.Background(), a); err != nil {
		t.Fatalf("updating artist: %v", err)
	}

	body := strings.NewReader("value=New bio")
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/artists/"+a.ID+"/fields/biography", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", a.ID)
	req.SetPathValue("field", "biography")
	w := httptest.NewRecorder()

	r.handleFieldUpdate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestHandleFieldUpdate_NoNFO_SkipsWriteBack(t *testing.T) {
	r, artistSvc := testRouter(t)

	dir := t.TempDir()
	a := addTestArtist(t, artistSvc, "No NFO Artist")
	a.Path = dir
	a.NFOExists = false
	if err := artistSvc.Update(context.Background(), a); err != nil {
		t.Fatalf("updating artist: %v", err)
	}

	body := strings.NewReader("value=Some bio")
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/artists/"+a.ID+"/fields/biography", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", a.ID)
	req.SetPathValue("field", "biography")
	w := httptest.NewRecorder()

	r.handleFieldUpdate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// No artist.nfo should have been created
	if _, err := os.Stat(filepath.Join(dir, "artist.nfo")); err == nil {
		t.Error("artist.nfo was created but NFOExists was false; write-back should have been skipped")
	}
}

func TestHandleFieldClear_WritesBackNFO(t *testing.T) {
	r, artistSvc := testRouter(t)

	dir := t.TempDir()
	a := addTestArtist(t, artistSvc, "Clear Test")
	a.Path = dir
	a.NFOExists = true
	a.Biography = "Bio to be cleared"
	if err := artistSvc.Update(context.Background(), a); err != nil {
		t.Fatalf("updating artist: %v", err)
	}

	// Seed an initial artist.nfo with biography
	seedNFO := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<artist><name>Clear Test</name><biography>Bio to be cleared</biography></artist>\n"
	if err := os.WriteFile(filepath.Join(dir, "artist.nfo"), []byte(seedNFO), 0o644); err != nil {
		t.Fatalf("writing seed nfo: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/fields/biography", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("field", "biography")
	w := httptest.NewRecorder()

	r.handleFieldClear(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	data, err := os.ReadFile(filepath.Join(dir, "artist.nfo"))
	if err != nil {
		t.Fatalf("reading nfo: %v", err)
	}
	parsed, err := nfo.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parsing nfo: %v", err)
	}
	if parsed.Biography != "" {
		t.Errorf("NFO biography = %q, want empty", parsed.Biography)
	}
}

func TestWriteBackNFO_CreatesSnapshot(t *testing.T) {
	r, artistSvc := testRouter(t)

	dir := t.TempDir()
	a := addTestArtist(t, artistSvc, "Snapshot Test")
	a.Path = dir
	a.NFOExists = true
	a.Biography = "Old bio"
	if err := artistSvc.Update(context.Background(), a); err != nil {
		t.Fatalf("updating artist: %v", err)
	}

	// Seed existing artist.nfo
	oldContent := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<artist><name>Snapshot Test</name><biography>Old bio</biography></artist>\n"
	if err := os.WriteFile(filepath.Join(dir, "artist.nfo"), []byte(oldContent), 0o644); err != nil {
		t.Fatalf("writing seed nfo: %v", err)
	}

	// Re-fetch to get the full artist state from DB
	a, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("re-fetching artist: %v", err)
	}

	r.writeBackNFO(context.Background(), a)

	// Verify a snapshot was saved
	snapshots, err := r.nfoSnapshotService.List(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("listing snapshots: %v", err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(snapshots))
	}
	if snapshots[0].Content != oldContent {
		t.Errorf("snapshot content = %q, want %q", snapshots[0].Content, oldContent)
	}
}

// TestWriteBackNFO_StatNotExist verifies that writeBackNFO skips silently when
// the artist has a path and NFOExists is true in the DB, but the file was
// deleted from disk (os.IsNotExist). No NFO file should be created.
func TestWriteBackNFO_StatNotExist(t *testing.T) {
	r, artistSvc := testRouter(t)

	dir := t.TempDir()
	a := addTestArtist(t, artistSvc, "Stat Test")
	a.Path = dir
	a.NFOExists = true
	if err := artistSvc.Update(context.Background(), a); err != nil {
		t.Fatalf("updating artist: %v", err)
	}

	// Do NOT seed an artist.nfo on disk -- file is missing
	r.writeBackNFO(context.Background(), a)

	// Verify no NFO was created (the NotExist branch should return early)
	if _, err := os.Stat(filepath.Join(dir, "artist.nfo")); err == nil {
		t.Error("artist.nfo was created but should have been skipped (file did not exist on disk)")
	}
}
