package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection"
)

// addTestConnectionWithURL creates a connection with a custom URL for handler tests
// that need to call a mock HTTP server.
func addTestConnectionWithURL(t *testing.T, r *Router, id, name, connType, url string) {
	t.Helper()
	c := &connection.Connection{
		ID:      id,
		Name:    name,
		Type:    connType,
		URL:     url,
		APIKey:  "test-key",
		Enabled: true,
		Status:  "ok",
	}
	if err := r.connectionService.Create(context.Background(), c); err != nil {
		t.Fatalf("creating test connection %s: %v", id, err)
	}
}

func TestHandleDeletePushImage_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/Images/Primary") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	router, artistSvc := testRouter(t)
	addTestConnectionWithURL(t, router, "conn-1", "Emby", "emby", srv.URL)
	a := addTestArtist(t, artistSvc, "Radiohead")

	body := `{"connection_id":"conn-1","platform_artist_id":"emby-artist-1"}`
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/push/images/thumb",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()

	router.handleDeletePushImage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["status"] != "deleted" {
		t.Errorf("status = %q, want deleted", resp["status"])
	}
}

func TestHandleDeletePushImage_AutoLookupPlatformID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/Items/emby-stored-id/Images/Primary") {
			t.Errorf("unexpected path: %s (want stored platform id in path)", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	router, artistSvc := testRouter(t)
	addTestConnectionWithURL(t, router, "conn-1", "Emby", "emby", srv.URL)
	a := addTestArtist(t, artistSvc, "Radiohead")

	// Store a platform ID mapping.
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-1", "emby-stored-id"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	// Omit platform_artist_id -- handler should auto-lookup from stored mapping.
	body := `{"connection_id":"conn-1"}`
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/push/images/thumb",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()

	router.handleDeletePushImage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestHandleDeletePushImage_InvalidType(t *testing.T) {
	router, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Radiohead")

	body := `{"connection_id":"conn-1","platform_artist_id":"emby-001"}`
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/push/images/clearart",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "clearart")
	w := httptest.NewRecorder()

	router.handleDeletePushImage(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleDeletePushImage_MissingConnectionID(t *testing.T) {
	router, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Radiohead")

	body := `{"platform_artist_id":"emby-001"}`
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/push/images/thumb",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()

	router.handleDeletePushImage(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleDeletePushImage_ConnectionNotFound(t *testing.T) {
	router, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Radiohead")

	body := `{"connection_id":"nonexistent","platform_artist_id":"emby-001"}`
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/push/images/thumb",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()

	router.handleDeletePushImage(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

func TestHandleDeletePushImage_ConnectionDisabled(t *testing.T) {
	router, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Radiohead")

	// Create a disabled connection.
	c := &connection.Connection{
		ID:      "conn-disabled",
		Name:    "Disabled Emby",
		Type:    "emby",
		URL:     "http://localhost:8096",
		APIKey:  "key",
		Enabled: false,
		Status:  "ok",
	}
	if err := router.connectionService.Create(context.Background(), c); err != nil {
		t.Fatalf("creating disabled connection: %v", err)
	}

	body := `{"connection_id":"conn-disabled","platform_artist_id":"emby-001"}`
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/push/images/thumb",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()

	router.handleDeletePushImage(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleDeletePushImage_ArtistNotFound(t *testing.T) {
	router, _ := testRouter(t)
	addTestConnection(t, router, "conn-1", "Emby", "emby")

	body := `{"connection_id":"conn-1","platform_artist_id":"emby-001"}`
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/nonexistent/push/images/thumb",
		strings.NewReader(body))
	req.SetPathValue("id", "nonexistent")
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()

	router.handleDeletePushImage(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

func TestHandleDeletePushImage_JellyfinSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/Images/Logo") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	router, artistSvc := testRouter(t)
	addTestConnectionWithURL(t, router, "conn-jf", "Jellyfin", "jellyfin", srv.URL)
	a := addTestArtist(t, artistSvc, "Portishead")

	body := `{"connection_id":"conn-jf","platform_artist_id":"jf-artist-1"}`
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/push/images/logo",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "logo")
	w := httptest.NewRecorder()

	router.handleDeletePushImage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestHandleDeletePushImage_PlatformIDNotStoredAndNotProvided(t *testing.T) {
	router, artistSvc := testRouter(t)
	addTestConnection(t, router, "conn-1", "Emby", "emby")
	a := addTestArtist(t, artistSvc, "Radiohead")

	// No platform ID stored, none provided.
	body := `{"connection_id":"conn-1"}`
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/push/images/thumb",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()

	router.handleDeletePushImage(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}
