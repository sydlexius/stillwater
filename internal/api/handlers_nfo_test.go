package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/nfo"
)

func TestHandleNFODiff_JSON(t *testing.T) {
	r, artistSvc := testRouter(t)

	a := addTestArtist(t, artistSvc, "Diff Artist")

	// For a pathless artist with no on-disk NFO, the diff compares nil vs database NFO.
	_ = a

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

func TestHandleNFODiff_PathlessArtist(t *testing.T) {
	r, artistSvc := testRouter(t)

	a := &artist.Artist{
		Name:          "Pathless Diff Artist",
		SortName:      "Pathless Diff Artist",
		Type:          "group",
		MusicBrainzID: "abc-pathless",
		Path:          "",
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
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
}

func TestHandleNFOConflictCheck_PathlessArtist(t *testing.T) {
	r, artistSvc := testRouter(t)

	a := &artist.Artist{
		Name:     "Pathless Conflict Artist",
		SortName: "Pathless Conflict Artist",
		Type:     "group",
		Path:     "",
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/nfo/conflict", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleNFOConflictCheck(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var check nfo.ConflictCheck
	if err := json.NewDecoder(w.Body).Decode(&check); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if check.HasConflict {
		t.Error("expected no conflict for pathless artist")
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
