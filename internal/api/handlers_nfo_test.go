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

func TestHandleNFODiff_JSON(t *testing.T) {
	r, artistSvc := testRouter(t)

	t.Run("pathless artist shows no diff", func(t *testing.T) {
		// Pathless artist (no filesystem path) compares dbNFO against itself.
		a := &artist.Artist{
			Name:     "Pathless Diff Artist",
			SortName: "Pathless Diff Artist",
			Type:     "group",
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

		if resp.HasDiff {
			t.Errorf("HasDiff = true for pathless artist, want false")
		}
	})

	t.Run("path-based artist without nfo file shows diff", func(t *testing.T) {
		// Artist with path but no on-disk NFO: all populated DB fields appear as added.
		dir := t.TempDir()
		a := &artist.Artist{
			Name:     "Path Diff Artist",
			SortName: "Path Diff Artist",
			Type:     "group",
			Path:     dir,
			Genres:   []string{"Rock"},
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

		if !resp.HasDiff {
			t.Errorf("HasDiff = false for path-based artist without NFO file, want true")
		}
	})

	t.Run("path-based artist with matching nfo shows no diff", func(t *testing.T) {
		dir := t.TempDir()
		a := &artist.Artist{
			Name:     "Matching NFO Artist",
			SortName: "Matching NFO Artist",
			Type:     "group",
			Path:     dir,
		}
		if err := artistSvc.Create(context.Background(), a); err != nil {
			t.Fatalf("creating artist: %v", err)
		}

		// Write an NFO file that matches the DB state.
		nfoContent := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<artist>
  <name>Matching NFO Artist</name>
  <sortname>Matching NFO Artist</sortname>
  <type>group</type>
</artist>`
		if err := os.WriteFile(filepath.Join(dir, "artist.nfo"), []byte(nfoContent), 0644); err != nil {
			t.Fatalf("writing nfo: %v", err)
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

		if resp.HasDiff {
			t.Errorf("HasDiff = true for matching NFO, want false")
		}
	})
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
	result, err := parseNFOFile("/nonexistent/path/artist.nfo")
	if result != nil {
		t.Error("expected nil for non-existent file")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want os.ErrNotExist", err)
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
