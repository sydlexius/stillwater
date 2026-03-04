package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

func addTestConnection(t *testing.T, r *Router, id, name, connType string) {
	t.Helper()
	_, err := r.db.ExecContext(context.Background(), `
		INSERT INTO connections (id, name, type, url, encrypted_api_key, enabled, status, created_at, updated_at)
		VALUES (?, ?, ?, 'http://test:8096', 'enc-key', 1, 'ok', datetime('now'), datetime('now'))
	`, id, name, connType)
	if err != nil {
		t.Fatalf("creating test connection %s: %v", id, err)
	}
}

func TestHandleGetPlatformIDs_Empty(t *testing.T) {
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Radiohead")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/platform-ids", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleGetPlatformIDs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var ids []artist.PlatformID
	if err := json.NewDecoder(w.Body).Decode(&ids); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("got %d platform ids, want 0", len(ids))
	}
}

func TestHandleGetPlatformIDs_WithData(t *testing.T) {
	r, artistSvc := testRouter(t)
	addTestConnection(t, r, "conn-1", "Emby", "emby")
	a := addTestArtist(t, artistSvc, "Radiohead")
	artistSvc.SetPlatformID(context.Background(), a.ID, "conn-1", "emby-123")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/platform-ids", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleGetPlatformIDs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var ids []artist.PlatformID
	if err := json.NewDecoder(w.Body).Decode(&ids); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("got %d platform ids, want 1", len(ids))
	}
	if ids[0].PlatformArtistID != "emby-123" {
		t.Errorf("platform_artist_id = %q, want %q", ids[0].PlatformArtistID, "emby-123")
	}
}

func TestHandleSetPlatformID(t *testing.T) {
	r, artistSvc := testRouter(t)
	addTestConnection(t, r, "conn-1", "Emby", "emby")
	a := addTestArtist(t, artistSvc, "Radiohead")

	body := `{"platform_artist_id": "emby-456"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/artists/"+a.ID+"/platform-ids/conn-1",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.SetPathValue("connectionId", "conn-1")
	w := httptest.NewRecorder()

	r.handleSetPlatformID(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify the ID was stored.
	got, _ := artistSvc.GetPlatformID(context.Background(), a.ID, "conn-1")
	if got != "emby-456" {
		t.Errorf("stored platform id = %q, want %q", got, "emby-456")
	}
}

func TestHandleSetPlatformID_MissingBody(t *testing.T) {
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Radiohead")

	body := `{"platform_artist_id": ""}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/artists/"+a.ID+"/platform-ids/conn-1",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.SetPathValue("connectionId", "conn-1")
	w := httptest.NewRecorder()

	r.handleSetPlatformID(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleDeletePlatformID(t *testing.T) {
	r, artistSvc := testRouter(t)
	addTestConnection(t, r, "conn-1", "Emby", "emby")
	a := addTestArtist(t, artistSvc, "Radiohead")
	artistSvc.SetPlatformID(context.Background(), a.ID, "conn-1", "emby-123")

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/platform-ids/conn-1", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("connectionId", "conn-1")
	w := httptest.NewRecorder()

	r.handleDeletePlatformID(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify it was deleted.
	got, _ := artistSvc.GetPlatformID(context.Background(), a.ID, "conn-1")
	if got != "" {
		t.Errorf("expected empty after delete, got %q", got)
	}
}

func TestHandleDeletePlatformID_NotFound(t *testing.T) {
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Radiohead")

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/platform-ids/nonexistent", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("connectionId", "nonexistent")
	w := httptest.NewRecorder()

	r.handleDeletePlatformID(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}
