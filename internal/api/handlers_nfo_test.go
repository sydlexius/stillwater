package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/nfo"
)

func TestHandleNFOSnapshotList(t *testing.T) {
	r, artistSvc := testRouter(t)

	a := addTestArtist(t, artistSvc, "Snapshot Artist")

	ctx := context.Background()
	if _, err := r.nfoSnapshotService.Save(ctx, a.ID, "<artist><name>V1</name></artist>"); err != nil {
		t.Fatalf("saving snapshot 1: %v", err)
	}
	if _, err := r.nfoSnapshotService.Save(ctx, a.ID, "<artist><name>V2</name></artist>"); err != nil {
		t.Fatalf("saving snapshot 2: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/nfo/snapshots", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleNFOSnapshotList(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string][]nfo.Snapshot
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if len(resp["snapshots"]) != 2 {
		t.Errorf("snapshot count = %d, want 2", len(resp["snapshots"]))
	}
}

func TestHandleNFOSnapshotList_Empty(t *testing.T) {
	r, artistSvc := testRouter(t)

	a := addTestArtist(t, artistSvc, "No Snapshots")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/nfo/snapshots", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleNFOSnapshotList(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string][]nfo.Snapshot
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if len(resp["snapshots"]) != 0 {
		t.Errorf("expected empty snapshots, got %d", len(resp["snapshots"]))
	}
}

func TestHandleNFOSnapshotList_MissingID(t *testing.T) {
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists//nfo/snapshots", nil)
	w := httptest.NewRecorder()

	r.handleNFOSnapshotList(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleNFODiff_JSON(t *testing.T) {
	r, artistSvc := testRouter(t)

	a := addTestArtist(t, artistSvc, "Diff Artist")

	ctx := context.Background()
	if _, err := r.nfoSnapshotService.Save(ctx, a.ID, "<artist><name>Old Name</name></artist>"); err != nil {
		t.Fatalf("saving snapshot: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/nfo/diff", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleNFODiff(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp nfo.DiffResult
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if len(resp.Fields) == 0 {
		t.Error("expected diff fields in result")
	}
}

func TestHandleNFODiff_HTMX(t *testing.T) {
	r, artistSvc := testRouter(t)

	a := addTestArtist(t, artistSvc, "HTMX Diff")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/nfo/diff", nil)
	req.SetPathValue("id", a.ID)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()

	r.handleNFODiff(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	ct := w.Header().Get("Content-Type")
	if ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

func TestHandleNFODiff_NotFound(t *testing.T) {
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/nonexistent/nfo/diff", nil)
	req.SetPathValue("id", "nonexistent")
	w := httptest.NewRecorder()

	r.handleNFODiff(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleNFOSnapshotRestore_ArtistNotFound(t *testing.T) {
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/nonexistent/nfo/snapshots/abc/restore", nil)
	req.SetPathValue("id", "nonexistent")
	req.SetPathValue("snapshotId", "abc")
	w := httptest.NewRecorder()

	r.handleNFOSnapshotRestore(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleNFOSnapshotRestore_MissingParams(t *testing.T) {
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists//nfo/snapshots//restore", nil)
	w := httptest.NewRecorder()

	r.handleNFOSnapshotRestore(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestParseNFOFile_NonExistent(t *testing.T) {
	result := parseNFOFile("/nonexistent/path/artist.nfo")
	if result != nil {
		t.Error("expected nil for non-existent file")
	}
}

func TestParseNFOFile_Valid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "artist.nfo")
	content := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<artist>
  <name>Test Artist</name>
  <musicbrainzartistid>abc-123</musicbrainzartistid>
</artist>`)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	result := parseNFOFile(path)
	if result == nil {
		t.Fatal("expected non-nil result for valid NFO")
	}
	if result.Name != "Test Artist" {
		t.Errorf("Name = %q, want %q", result.Name, "Test Artist")
	}
	if result.MusicBrainzArtistID != "abc-123" {
		t.Errorf("MBID = %q, want %q", result.MusicBrainzArtistID, "abc-123")
	}
}
