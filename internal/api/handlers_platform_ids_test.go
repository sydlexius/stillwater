package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
)

func addTestConnection(t *testing.T, r *Router, id, name, connType string) {
	t.Helper()
	c := &connection.Connection{
		ID:      id,
		Name:    name,
		Type:    connType,
		URL:     "http://test:8096",
		APIKey:  "test-key",
		Enabled: true,
		Status:  "ok",
	}
	if err := r.connectionService.Create(context.Background(), c); err != nil {
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
	addTestConnection(t, r, "conn-2", "Jellyfin", "jellyfin")
	a := addTestArtist(t, artistSvc, "Radiohead")
	artistSvc.SetPlatformID(context.Background(), a.ID, "conn-1", "emby-123")
	artistSvc.SetPlatformID(context.Background(), a.ID, "conn-2", "jf-456")

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
	if len(ids) != 2 {
		t.Fatalf("got %d platform ids, want 2", len(ids))
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

func TestHandleSetPlatformID_ArtistNotFound(t *testing.T) {
	r, _ := testRouter(t)
	addTestConnection(t, r, "conn-1", "Emby", "emby")

	body := `{"platform_artist_id": "emby-456"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/artists/nonexistent/platform-ids/conn-1",
		strings.NewReader(body))
	req.SetPathValue("id", "nonexistent")
	req.SetPathValue("connectionId", "conn-1")
	w := httptest.NewRecorder()

	r.handleSetPlatformID(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

func TestHandleSetPlatformID_ConnectionNotFound(t *testing.T) {
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Radiohead")

	body := `{"platform_artist_id": "emby-456"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/artists/"+a.ID+"/platform-ids/nonexistent",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.SetPathValue("connectionId", "nonexistent")
	w := httptest.NewRecorder()

	r.handleSetPlatformID(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusNotFound, w.Body.String())
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
