package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/nfo"
)

func TestHandleNFOConflictCheck_PathlessArtist(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	result, err := parseNFOFile("/nonexistent/path/artist.nfo")
	if result != nil {
		t.Error("expected nil for non-existent file")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want os.ErrNotExist", err)
	}
}

func TestParseNFOFile_Valid(t *testing.T) {
	t.Parallel()
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

	result, err := parseNFOFile(path)
	if err != nil {
		t.Fatalf("parseNFOFile err = %v", err)
	}
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
