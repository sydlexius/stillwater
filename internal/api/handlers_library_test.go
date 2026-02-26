package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/rule"
)

func testRouterWithLibrary(t *testing.T) (*Router, *library.Service, *artist.Service) {
	t.Helper()

	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	libSvc := library.NewService(db)
	artistSvc := artist.NewService(db)
	authSvc := auth.NewService(db)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	nfoSnapSvc := nfo.NewSnapshotService(db)

	r := NewRouter(RouterDeps{
		AuthService:        authSvc,
		ArtistService:      artistSvc,
		LibraryService:     libSvc,
		RuleService:        ruleSvc,
		NFOSnapshotService: nfoSnapSvc,
		DB:                 db,
		Logger:             logger,
		StaticDir:          "../../web/static",
	})

	return r, libSvc, artistSvc
}

func TestHandleListLibraries_Empty(t *testing.T) {
	r, _, _ := testRouterWithLibrary(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/libraries", nil)
	w := httptest.NewRecorder()

	r.handleListLibraries(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var libs []library.Library
	if err := json.NewDecoder(w.Body).Decode(&libs); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(libs) != 0 {
		t.Errorf("expected 0 libraries, got %d", len(libs))
	}
}

func TestHandleCreateLibrary_JSON(t *testing.T) {
	r, _, _ := testRouterWithLibrary(t)

	body := `{"name":"Music","path":"/music","type":"regular"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/libraries", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleCreateLibrary(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var lib library.Library
	if err := json.NewDecoder(w.Body).Decode(&lib); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if lib.ID == "" {
		t.Error("expected non-empty id")
	}
	if lib.Name != "Music" {
		t.Errorf("name = %q, want %q", lib.Name, "Music")
	}
	if lib.Path != "/music" {
		t.Errorf("path = %q, want %q", lib.Path, "/music")
	}
	if lib.Type != "regular" {
		t.Errorf("type = %q, want %q", lib.Type, "regular")
	}
}

func TestHandleCreateLibrary_FormEncoded(t *testing.T) {
	r, _, _ := testRouterWithLibrary(t)

	body := "name=Classical&path=/music/classical&type=classical"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/libraries", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	r.handleCreateLibrary(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var lib library.Library
	if err := json.NewDecoder(w.Body).Decode(&lib); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if lib.Name != "Classical" {
		t.Errorf("name = %q, want %q", lib.Name, "Classical")
	}
	if lib.Type != "classical" {
		t.Errorf("type = %q, want %q", lib.Type, "classical")
	}
}

func TestHandleCreateLibrary_MissingName(t *testing.T) {
	r, _, _ := testRouterWithLibrary(t)

	body := `{"path":"/music","type":"regular"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/libraries", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleCreateLibrary(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleCreateLibrary_InvalidType(t *testing.T) {
	r, _, _ := testRouterWithLibrary(t)

	body := `{"name":"Bad","path":"/bad","type":"invalid"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/libraries", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleCreateLibrary(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleGetLibrary_WithArtistCount(t *testing.T) {
	r, libSvc, artistSvc := testRouterWithLibrary(t)

	lib := &library.Library{Name: "Music", Path: "/music", Type: "regular"}
	if err := libSvc.Create(context.Background(), lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	// Create an artist in the library
	a := &artist.Artist{Name: "Test Artist", Path: "/music/test-artist", LibraryID: lib.ID}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/libraries/"+lib.ID, nil)
	req.SetPathValue("id", lib.ID)
	w := httptest.NewRecorder()

	r.handleGetLibrary(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	count, ok := resp["artist_count"].(float64)
	if !ok || count != 1 {
		t.Errorf("artist_count = %v, want 1", resp["artist_count"])
	}
}

func TestHandleGetLibrary_NotFound(t *testing.T) {
	r, _, _ := testRouterWithLibrary(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/libraries/nonexistent", nil)
	req.SetPathValue("id", "nonexistent")
	w := httptest.NewRecorder()

	r.handleGetLibrary(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleUpdateLibrary(t *testing.T) {
	r, libSvc, _ := testRouterWithLibrary(t)

	lib := &library.Library{Name: "Music", Path: "/music", Type: "regular"}
	if err := libSvc.Create(context.Background(), lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	body := `{"name":"Updated Music","path":"/music/updated","type":"classical"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/libraries/"+lib.ID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", lib.ID)
	w := httptest.NewRecorder()

	r.handleUpdateLibrary(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var updated library.Library
	if err := json.NewDecoder(w.Body).Decode(&updated); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if updated.Name != "Updated Music" {
		t.Errorf("name = %q, want %q", updated.Name, "Updated Music")
	}
	if updated.Type != "classical" {
		t.Errorf("type = %q, want %q", updated.Type, "classical")
	}
}

func TestHandleUpdateLibrary_NotFound(t *testing.T) {
	r, _, _ := testRouterWithLibrary(t)

	body := `{"name":"Nope"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/libraries/nonexistent", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "nonexistent")
	w := httptest.NewRecorder()

	r.handleUpdateLibrary(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleDeleteLibrary_Empty(t *testing.T) {
	r, libSvc, _ := testRouterWithLibrary(t)

	lib := &library.Library{Name: "Music", Path: "/music", Type: "regular"}
	if err := libSvc.Create(context.Background(), lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/libraries/"+lib.ID, nil)
	req.SetPathValue("id", lib.ID)
	w := httptest.NewRecorder()

	r.handleDeleteLibrary(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["status"] != "deleted" {
		t.Errorf("status = %q, want %q", resp["status"], "deleted")
	}
}

func TestHandleDeleteLibrary_WithArtists(t *testing.T) {
	r, libSvc, artistSvc := testRouterWithLibrary(t)

	lib := &library.Library{Name: "Music", Path: "/music", Type: "regular"}
	if err := libSvc.Create(context.Background(), lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	a := &artist.Artist{Name: "Test Artist", Path: "/music/test-artist", LibraryID: lib.ID}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/libraries/"+lib.ID, nil)
	req.SetPathValue("id", lib.ID)
	w := httptest.NewRecorder()

	r.handleDeleteLibrary(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify the artist was dereferenced, not deleted.
	updated, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("artist should still exist: %v", err)
	}
	if updated.LibraryID != "" {
		t.Errorf("artist library_id = %q, want empty (dereferenced)", updated.LibraryID)
	}
}

func TestHandleListLibraries_AfterCreate(t *testing.T) {
	r, libSvc, _ := testRouterWithLibrary(t)

	lib := &library.Library{Name: "Music", Path: "/music", Type: "regular"}
	if err := libSvc.Create(context.Background(), lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/libraries", nil)
	w := httptest.NewRecorder()

	r.handleListLibraries(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var libs []library.Library
	if err := json.NewDecoder(w.Body).Decode(&libs); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(libs) != 1 {
		t.Fatalf("expected 1 library, got %d", len(libs))
	}
	if libs[0].Name != "Music" {
		t.Errorf("name = %q, want %q", libs[0].Name, "Music")
	}
}
