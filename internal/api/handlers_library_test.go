package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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

	db := newTestDB(t)
	if err := database.EnableForeignKeys(db); err != nil {
		t.Fatalf("enabling foreign keys: %v", err)
	}

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
		StaticFS:           os.DirFS("../../web/static"),
	})

	return r, libSvc, artistSvc
}

func TestHandleListLibraries_Empty(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	r, _, _ := testRouterWithLibrary(t)

	dir := t.TempDir()
	body := fmt.Sprintf(`{"name":"Music","path":%q,"type":"regular"}`, dir)
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
	if lib.Path != dir {
		t.Errorf("path = %q, want %q", lib.Path, dir)
	}
	if lib.Type != "regular" {
		t.Errorf("type = %q, want %q", lib.Type, "regular")
	}
}

func TestHandleCreateLibrary_FormEncoded(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithLibrary(t)

	dir := t.TempDir()
	vals := url.Values{}
	vals.Set("name", "Classical")
	vals.Set("path", dir)
	vals.Set("type", "classical")
	body := vals.Encode()
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
	t.Parallel()
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
	t.Parallel()
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

func TestHandleCreateLibrary_EmptyPath(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithLibrary(t)

	body := `{"name":"API Only","path":"","type":"regular"}`
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
	if lib.Path != "" {
		t.Errorf("path = %q, want empty", lib.Path)
	}
}

func TestHandleCreateLibrary_RelativePath(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithLibrary(t)

	body := `{"name":"Bad","path":"music/lib","type":"regular"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/libraries", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleCreateLibrary(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleCreateLibrary_TraversalPath(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithLibrary(t)

	body := `{"name":"Bad","path":"../etc/passwd","type":"regular"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/libraries", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleCreateLibrary(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleCreateLibrary_NonexistentPath(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithLibrary(t)

	dir := filepath.Join(t.TempDir(), "no-such-dir")
	body := fmt.Sprintf(`{"name":"Bad","path":%q,"type":"regular"}`, dir)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/libraries", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleCreateLibrary(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleGetLibrary_WithArtistCount(t *testing.T) {
	t.Parallel()
	r, libSvc, artistSvc := testRouterWithLibrary(t)

	dir := t.TempDir()
	lib := &library.Library{Name: "Music", Path: dir, Type: "regular"}
	if err := libSvc.Create(context.Background(), lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	// Create an artist in the library
	a := &artist.Artist{Name: "Test Artist", Path: filepath.Join(dir, "test-artist"), LibraryID: lib.ID}
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
	t.Parallel()
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
	t.Parallel()
	r, libSvc, _ := testRouterWithLibrary(t)

	base := t.TempDir()
	origDir := filepath.Join(base, "orig")
	updatedDir := filepath.Join(base, "updated")
	if err := os.Mkdir(origDir, 0o755); err != nil {
		t.Fatalf("creating orig dir: %v", err)
	}
	if err := os.Mkdir(updatedDir, 0o755); err != nil {
		t.Fatalf("creating updated dir: %v", err)
	}

	lib := &library.Library{Name: "Music", Path: origDir, Type: "regular"}
	if err := libSvc.Create(context.Background(), lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	body := fmt.Sprintf(`{"name":"Updated Music","path":%q,"type":"classical"}`, updatedDir)
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

func TestHandleUpdateLibrary_InvalidPath(t *testing.T) {
	t.Parallel()
	r, libSvc, _ := testRouterWithLibrary(t)

	dir := t.TempDir()
	lib := &library.Library{Name: "Music", Path: dir, Type: "regular"}
	if err := libSvc.Create(context.Background(), lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	body := `{"path":"../traversal"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/libraries/"+lib.ID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", lib.ID)
	w := httptest.NewRecorder()

	r.handleUpdateLibrary(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleUpdateLibrary_NotFound(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	r, libSvc, _ := testRouterWithLibrary(t)

	dir := t.TempDir()
	lib := &library.Library{Name: "Music", Path: dir, Type: "regular"}
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

// TestHandleDeleteLibrary_PrunesZeroHomeOrphan covers issue #1613: when a
// library is deleted without ?deleteArtists=true, an artist whose only
// membership was in that library (zero remaining memberships, zero platform
// mappings) must be garbage-collected. The old behavior was to preserve all
// artists unconditionally; the new behavior prunes true zero-home orphans.
func TestHandleDeleteLibrary_PrunesZeroHomeOrphan(t *testing.T) {
	t.Parallel()
	r, libSvc, artistSvc := testRouterWithLibrary(t)

	dir := t.TempDir()
	lib := &library.Library{Name: "Music", Path: dir, Type: "regular"}
	if err := libSvc.Create(context.Background(), lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	a := &artist.Artist{Name: "Test Artist", Path: filepath.Join(dir, "test-artist"), LibraryID: lib.ID}
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

	// The artist had no other home (zero memberships, zero platform mappings);
	// it must be pruned by Service.Delete. This is the fix for #1613.
	_, err := artistSvc.GetByID(context.Background(), a.ID)
	if err == nil {
		t.Error("zero-home orphan artist must be deleted when its only library is removed (issue #1613)")
	}
}

// TestHandleDeleteLibrary_WithArtists covers the sibling-library case: when a
// library is deleted without ?deleteArtists=true, an artist with a remaining
// membership in another library must survive.
func TestHandleDeleteLibrary_WithArtists(t *testing.T) {
	t.Parallel()
	r, libSvc, artistSvc := testRouterWithLibrary(t)
	ctx := context.Background()

	base := t.TempDir()
	dir1 := filepath.Join(base, "lib1")
	dir2 := filepath.Join(base, "lib2")
	for _, d := range []string{dir1, dir2} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	lib1 := &library.Library{Name: "Music One", Path: dir1, Type: "regular"}
	lib2 := &library.Library{Name: "Music Two", Path: dir2, Type: "regular"}
	if err := libSvc.Create(ctx, lib1); err != nil {
		t.Fatalf("creating lib1: %v", err)
	}
	if err := libSvc.Create(ctx, lib2); err != nil {
		t.Fatalf("creating lib2: %v", err)
	}

	// Create the artist in lib1; it will gain a lib1 membership.
	a := &artist.Artist{Name: "Multi Home Artist", Path: filepath.Join(dir1, "multi"), LibraryID: lib1.ID}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	// Add a second membership in lib2 directly.
	if err := artistSvc.AddLibraryMembership(ctx, a.ID, lib2.ID, "filesystem"); err != nil {
		t.Fatalf("adding lib2 membership: %v", err)
	}

	// Delete lib1 without ?deleteArtists=true; the artist still has lib2.
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/libraries/"+lib1.ID, nil)
	req.SetPathValue("id", lib1.ID)
	w := httptest.NewRecorder()

	r.handleDeleteLibrary(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Artist survives because lib2 membership is still present.
	_, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Errorf("artist with sibling-library membership must survive delete: %v", err)
	}
}

func TestHandleListLibraries_AfterCreate(t *testing.T) {
	t.Parallel()
	r, libSvc, _ := testRouterWithLibrary(t)

	dir := t.TempDir()
	lib := &library.Library{Name: "Music", Path: dir, Type: "regular"}
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

// TestHandleUpdateLibrary_NFOLockData_Toggle covers the new per-library
// nfo_lock_data field added for issue #1264. Verifies the field defaults to
// false on Create, that PUT /api/v1/libraries/{id} with nfo_lock_data:true
// flips it on, and that omitting the field on a subsequent PUT preserves
// the current value (pointer-typed -> only updated when present in body).
func TestHandleUpdateLibrary_NFOLockData_Toggle(t *testing.T) {
	t.Parallel()
	r, libSvc, _ := testRouterWithLibrary(t)

	dir := t.TempDir()
	lib := &library.Library{Name: "Lockable", Path: dir, Type: "regular"}
	if err := libSvc.Create(context.Background(), lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}
	if lib.NFOLockData {
		t.Fatal("default NFOLockData must be false")
	}

	// PUT with nfo_lock_data:true flips on and round-trips in response.
	req := httptest.NewRequest(http.MethodPut, "/api/v1/libraries/"+lib.ID, strings.NewReader(`{"nfo_lock_data":true}`))
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
	if !updated.NFOLockData {
		t.Error("NFOLockData=true did not round-trip in response")
	}

	// PUT without nfo_lock_data preserves the current value (true).
	req2 := httptest.NewRequest(http.MethodPut, "/api/v1/libraries/"+lib.ID, strings.NewReader(`{"name":"Renamed"}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.SetPathValue("id", lib.ID)
	w2 := httptest.NewRecorder()
	r.handleUpdateLibrary(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("rename status = %d, body: %s", w2.Code, w2.Body.String())
	}
	persisted, err := libSvc.GetByID(context.Background(), lib.ID)
	if err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	if !persisted.NFOLockData {
		t.Error("NFOLockData regressed to false when omitted from PUT body")
	}

	// PUT with nfo_lock_data:false explicitly disables.
	req3 := httptest.NewRequest(http.MethodPut, "/api/v1/libraries/"+lib.ID, strings.NewReader(`{"nfo_lock_data":false}`))
	req3.Header.Set("Content-Type", "application/json")
	req3.SetPathValue("id", lib.ID)
	w3 := httptest.NewRecorder()
	r.handleUpdateLibrary(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("disable status = %d, body: %s", w3.Code, w3.Body.String())
	}
	// Decode the handler echo and assert the toggled-off value before the
	// re-fetch, so a regression that persists the wrong value but echoes
	// the prior value (or vice versa) is caught at the handler boundary.
	var echoed library.Library
	if err := json.Unmarshal(w3.Body.Bytes(), &echoed); err != nil {
		t.Fatalf("decoding disable response: %v; body: %s", err, w3.Body.String())
	}
	if echoed.NFOLockData {
		t.Error("handler echo: NFOLockData should be false after explicit disable")
	}
	persisted2, err := libSvc.GetByID(context.Background(), lib.ID)
	if err != nil {
		t.Fatalf("re-fetch after disable: %v", err)
	}
	if persisted2.NFOLockData {
		t.Error("NFOLockData=false did not persist on explicit disable")
	}
}

// TestHandlePatchLibrary_ConvertToRegular verifies that PATCH
// /api/v1/libraries/{id} with {"type":"regular"} converts a Classical library
// to Regular and does NOT return Deprecation/Sunset headers for the converted
// library.
func TestHandlePatchLibrary_ConvertToRegular(t *testing.T) {
	t.Parallel()
	r, libSvc, _ := testRouterWithLibrary(t)

	dir := t.TempDir()
	lib := &library.Library{Name: "Orchestra", Path: dir, Type: library.TypeClassical}
	if err := libSvc.Create(context.Background(), lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/libraries/"+lib.ID, strings.NewReader(`{"type":"regular"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", lib.ID)
	w := httptest.NewRecorder()

	r.handlePatchLibrary(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var updated library.Library
	if err := json.NewDecoder(w.Body).Decode(&updated); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if updated.Type != library.TypeRegular {
		t.Errorf("type = %q, want %q", updated.Type, library.TypeRegular)
	}
	// Converted library is now Regular; no deprecation headers expected.
	if w.Result().Header.Get("Deprecation") != "" {
		t.Error("Deprecation header should be absent after conversion to regular")
	}
	if w.Result().Header.Get("Sunset") != "" {
		t.Error("Sunset header should be absent after conversion to regular")
	}
}

// TestHandlePatchLibrary_DeprecationHeaders verifies that PATCH
// /api/v1/libraries/{id} with type=classical emits Deprecation and Sunset
// headers.
func TestHandlePatchLibrary_DeprecationHeaders(t *testing.T) {
	t.Parallel()
	r, libSvc, _ := testRouterWithLibrary(t)

	dir := t.TempDir()
	lib := &library.Library{Name: "RegLib", Path: dir, Type: library.TypeRegular}
	if err := libSvc.Create(context.Background(), lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/libraries/"+lib.ID, strings.NewReader(`{"type":"classical"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", lib.ID)
	w := httptest.NewRecorder()

	r.handlePatchLibrary(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if w.Result().Header.Get("Deprecation") != "true" {
		t.Errorf("Deprecation header = %q, want %q", w.Result().Header.Get("Deprecation"), "true")
	}
	if got := w.Result().Header.Get("Sunset"); got != library.SunsetClassicalType {
		t.Errorf("Sunset header = %q, want %q", got, library.SunsetClassicalType)
	}
}

// TestHandlePatchLibrary_NotFound verifies that PATCH returns 404 for
// a non-existent library ID.
func TestHandlePatchLibrary_NotFound(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithLibrary(t)

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/libraries/nonexistent", strings.NewReader(`{"type":"regular"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "nonexistent")
	w := httptest.NewRecorder()

	r.handlePatchLibrary(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

// TestHandlePatchLibrary_MissingType verifies that PATCH returns 400 when
// the type field is absent (JSON) or not supplied (form-encoded).
func TestHandlePatchLibrary_MissingType(t *testing.T) {
	t.Parallel()
	r, libSvc, _ := testRouterWithLibrary(t)

	dir := t.TempDir()
	lib := &library.Library{Name: "MissingTypeLib", Path: dir, Type: library.TypeRegular}
	if err := libSvc.Create(context.Background(), lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	tests := []struct {
		name        string
		body        string
		contentType string
	}{
		{"json empty object", `{}`, "application/json"},
		{"json type null", `{"type":null}`, "application/json"},
		{"form missing key", `name=irrelevant`, "application/x-www-form-urlencoded"},
		{"form empty value", `type=`, "application/x-www-form-urlencoded"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPatch, "/api/v1/libraries/"+lib.ID, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.contentType)
			req.SetPathValue("id", lib.ID)
			w := httptest.NewRecorder()
			r.handlePatchLibrary(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestHandleCreateLibrary_ClassicalDeprecationHeaders verifies that POST
// /api/v1/libraries with type=classical emits Deprecation and Sunset headers.
func TestHandleCreateLibrary_ClassicalDeprecationHeaders(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithLibrary(t)

	dir := t.TempDir()
	body := fmt.Sprintf(`{"name":"OldClassical","path":%q,"type":"classical"}`, dir)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/libraries", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleCreateLibrary(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}
	if w.Result().Header.Get("Deprecation") != "true" {
		t.Errorf("Deprecation header = %q, want %q", w.Result().Header.Get("Deprecation"), "true")
	}
	if got := w.Result().Header.Get("Sunset"); got != library.SunsetClassicalType {
		t.Errorf("Sunset header = %q, want %q", got, library.SunsetClassicalType)
	}
}

// TestHandleUpdateLibrary_FormEncoded_NFOLockData covers the
// application/x-www-form-urlencoded path for nfo_lock_data, which OpenAPI
// documents alongside the JSON body. Regression coverage for the gap where
// the form branch silently dropped the field while the JSON branch wired it
// through.
func TestHandleUpdateLibrary_FormEncoded_NFOLockData(t *testing.T) {
	t.Parallel()
	r, libSvc, _ := testRouterWithLibrary(t)
	libDir := t.TempDir()
	lib := &library.Library{Name: "FormPath", Path: libDir, Type: library.TypeRegular}
	if err := libSvc.Create(context.Background(), lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	cases := []struct {
		name string
		body string
		want bool
	}{
		{"true", "nfo_lock_data=true", true},
		{"on (browser checkbox)", "nfo_lock_data=on", true},
		{"1", "nfo_lock_data=1", true},
		{"false", "nfo_lock_data=false", false},
		{"0", "nfo_lock_data=0", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/api/v1/libraries/"+lib.ID, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.SetPathValue("id", lib.ID)
			w := httptest.NewRecorder()
			r.handleUpdateLibrary(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
			}
			persisted, err := libSvc.GetByID(context.Background(), lib.ID)
			if err != nil {
				t.Fatalf("re-fetch: %v", err)
			}
			if persisted.NFOLockData != tc.want {
				t.Errorf("NFOLockData = %v after %q form post, want %v", persisted.NFOLockData, tc.body, tc.want)
			}
		})
	}

	// Invalid value rejected with 400.
	req := httptest.NewRequest(http.MethodPut, "/api/v1/libraries/"+lib.ID, strings.NewReader("nfo_lock_data=maybe"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", lib.ID)
	w := httptest.NewRecorder()
	r.handleUpdateLibrary(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid form value status = %d, want 400; body: %s", w.Code, w.Body.String())
	}

	// Absent field preserves prior value (tristate parity with JSON).
	if err := libSvc.Update(context.Background(), &library.Library{ID: lib.ID, Name: "FormPath", Path: libDir, Type: library.TypeRegular, NFOLockData: true}); err != nil {
		t.Fatalf("seeding NFOLockData=true: %v", err)
	}
	req2 := httptest.NewRequest(http.MethodPut, "/api/v1/libraries/"+lib.ID, strings.NewReader("name=FormPath"))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.SetPathValue("id", lib.ID)
	w2 := httptest.NewRecorder()
	r.handleUpdateLibrary(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("absent-field status = %d, body: %s", w2.Code, w2.Body.String())
	}
	preserved, err := libSvc.GetByID(context.Background(), lib.ID)
	if err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	if !preserved.NFOLockData {
		t.Error("absent nfo_lock_data form key must preserve existing true value, got false")
	}
}
