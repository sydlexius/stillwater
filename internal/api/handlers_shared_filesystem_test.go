package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/sydlexius/stillwater/internal/library"
)

func TestHandleSharedFilesystemStatus(t *testing.T) {
	r, _, _ := testRouterWithLibrary(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/shared-filesystem/status", nil)
	w := httptest.NewRecorder()
	r.handleSharedFilesystemStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Decode into a raw map first to verify JSON shape ([] not null).
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decoding raw response: %v", err)
	}

	// Libraries must be an empty JSON array, not null.
	libsRaw := string(raw["libraries"])
	if libsRaw != "[]" {
		t.Errorf("expected libraries to be [], got %s", libsRaw)
	}

	var status SharedFilesystemStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	// Fresh database should have no overlaps.
	if status.HasOverlaps {
		t.Error("expected no overlaps on fresh database")
	}
	if status.Dismissed {
		t.Error("expected not dismissed on fresh database")
	}
}

func TestHandleSharedFilesystemDismiss(t *testing.T) {
	r, _, _ := testRouterWithLibrary(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/shared-filesystem/dismiss", nil)
	w := httptest.NewRecorder()
	r.handleSharedFilesystemDismiss(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify the dismiss persisted by checking status.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/shared-filesystem/status", nil)
	w2 := httptest.NewRecorder()
	r.handleSharedFilesystemStatus(w2, req2)

	var status SharedFilesystemStatus
	if err := json.NewDecoder(w2.Body).Decode(&status); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if !status.Dismissed {
		t.Error("expected dismissed to be true after dismiss call")
	}
}

func TestHandleSharedFilesystemStatusOverlapWith(t *testing.T) {
	// Verify that OverlapWith is populated when overlapping libraries exist.
	r, libSvc, _ := testRouterWithLibrary(t)
	ctx := context.Background()

	tmp := t.TempDir()
	musicPath := tmp + "/music"
	if err := os.MkdirAll(musicPath, 0o755); err != nil {
		t.Fatalf("creating music dir: %v", err)
	}

	// Create a manual library and an Emby library at the same path so they
	// trigger overlap detection.
	manualLib := &library.Library{
		Name:   "My Music",
		Path:   musicPath,
		Type:   library.TypeRegular,
		Source: library.SourceManual,
	}
	if err := libSvc.Create(ctx, manualLib); err != nil {
		t.Fatalf("creating manual library: %v", err)
	}
	embyLib := &library.Library{
		Name:   "Emby Music",
		Path:   musicPath,
		Type:   library.TypeRegular,
		Source: library.SourceEmby,
	}
	if err := libSvc.Create(ctx, embyLib); err != nil {
		t.Fatalf("creating emby library: %v", err)
	}

	// Run a recheck so shared_filesystem flags are set.
	recheckReq := httptest.NewRequest(http.MethodPost, "/api/v1/shared-filesystem/recheck", nil)
	recheckW := httptest.NewRecorder()
	r.handleSharedFilesystemRecheck(recheckW, recheckReq)
	if recheckW.Code != http.StatusOK {
		t.Fatalf("recheck: expected 200, got %d: %s", recheckW.Code, recheckW.Body.String())
	}

	// Fetch status and verify OverlapWith is populated.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/shared-filesystem/status", nil)
	w := httptest.NewRecorder()
	r.handleSharedFilesystemStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var status SharedFilesystemStatus
	if err := json.NewDecoder(w.Body).Decode(&status); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if !status.HasOverlaps {
		t.Fatal("expected overlaps to be detected")
	}

	for _, entry := range status.Libraries {
		if entry.OverlapWith == "" {
			t.Errorf("library %q (id=%s) has empty OverlapWith; expected overlap description",
				entry.LibraryName, entry.LibraryID)
		}
	}
}

func TestHandleSharedFilesystemRecheck(t *testing.T) {
	r, _, _ := testRouterWithLibrary(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/shared-filesystem/recheck", nil)
	w := httptest.NewRecorder()
	r.handleSharedFilesystemRecheck(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result map[string]any
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	val, ok := result["overlaps_found"]
	if !ok {
		t.Fatal("expected overlaps_found field in response")
	}
	// json.Unmarshal decodes numbers as float64; verify it is numeric.
	if _, isNum := val.(float64); !isNum {
		t.Errorf("expected overlaps_found to be a number, got %T", val)
	}
}
