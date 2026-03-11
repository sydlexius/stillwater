package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestFieldUpdate_AutoPush_FiresForPlatformArtist verifies that a PATCH field
// request triggers an async metadata push to the connected Emby server.
func TestFieldUpdate_AutoPush_FiresForPlatformArtist(t *testing.T) {
	received := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/Items/") {
			select {
			case received <- struct{}{}:
			default:
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	router, artistSvc := testRouter(t)
	addTestConnectionWithURL(t, router, "conn-emby", "Emby", "emby", srv.URL)
	a := addTestArtist(t, artistSvc, "Radiohead")
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-artist-1"); err != nil {
		t.Fatalf("setting platform id: %v", err)
	}

	body := strings.NewReader(`{"value":"Updated biography"}`)
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/artists/"+a.ID+"/fields/biography", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	req.SetPathValue("field", "biography")
	w := httptest.NewRecorder()

	router.handleFieldUpdate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	select {
	case <-received:
		// push arrived
	case <-time.After(2 * time.Second):
		t.Error("auto-push: no request received by mock Emby server within 2s")
	}
}

// TestFieldClear_AutoPush_FiresForPlatformArtist verifies that a DELETE field
// request also triggers an async metadata push.
func TestFieldClear_AutoPush_FiresForPlatformArtist(t *testing.T) {
	received := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/Items/") {
			select {
			case received <- struct{}{}:
			default:
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	router, artistSvc := testRouter(t)
	addTestConnectionWithURL(t, router, "conn-emby", "Emby", "emby", srv.URL)
	a := addTestArtist(t, artistSvc, "Portishead")
	a.Biography = "Bio to clear"
	if err := artistSvc.Update(context.Background(), a); err != nil {
		t.Fatalf("updating artist: %v", err)
	}
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-artist-2"); err != nil {
		t.Fatalf("setting platform id: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/fields/biography", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("field", "biography")
	w := httptest.NewRecorder()

	router.handleFieldClear(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	select {
	case <-received:
		// push arrived
	case <-time.After(2 * time.Second):
		t.Error("auto-push: no request received by mock Emby server within 2s")
	}
}

// TestFieldUpdate_AutoPush_SkipsLocalArtist verifies that artists with no
// platform ID mappings do not trigger any outbound push requests.
func TestFieldUpdate_AutoPush_SkipsLocalArtist(t *testing.T) {
	called := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case called <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	router, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Local Only Artist")
	// No platform ID set -- local-only artist.

	body := strings.NewReader(`{"value":"Some bio"}`)
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/artists/"+a.ID+"/fields/biography", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	req.SetPathValue("field", "biography")
	w := httptest.NewRecorder()

	router.handleFieldUpdate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Give any errant goroutine a window to fire.
	select {
	case <-called:
		t.Error("auto-push: mock server was called for a local-only artist; expected no push")
	case <-time.After(100 * time.Millisecond):
		// Correct: no push.
	}
}

// TestFieldUpdate_AutoPush_SkipsDisabledConnection verifies that disabled
// connections are not pushed to.
func TestFieldUpdate_AutoPush_SkipsDisabledConnection(t *testing.T) {
	called := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case called <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	router, artistSvc := testRouter(t)
	// Create a disabled connection.
	addTestConnectionWithURL(t, router, "conn-disabled", "Disabled Emby", "emby", srv.URL)
	conn, err := router.connectionService.GetByID(context.Background(), "conn-disabled")
	if err != nil {
		t.Fatalf("getting connection: %v", err)
	}
	conn.Enabled = false
	if err := router.connectionService.Update(context.Background(), conn); err != nil {
		t.Fatalf("disabling connection: %v", err)
	}

	a := addTestArtist(t, artistSvc, "Disabled Push Artist")
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-disabled", "emby-artist-3"); err != nil {
		t.Fatalf("setting platform id: %v", err)
	}

	body := strings.NewReader(`{"value":"Some bio"}`)
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/artists/"+a.ID+"/fields/biography", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	req.SetPathValue("field", "biography")
	w := httptest.NewRecorder()

	router.handleFieldUpdate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	select {
	case <-called:
		t.Error("auto-push: mock server was called for a disabled connection; expected no push")
	case <-time.After(100 * time.Millisecond):
		// Correct: no push.
	}
}

// TestFieldUpdate_AutoPush_JellyfinFires verifies that the Jellyfin connection
// type is also pushed to (not silently skipped by the type switch).
func TestFieldUpdate_AutoPush_JellyfinFires(t *testing.T) {
	received := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Jellyfin PushMetadata now GETs the current item before POSTing the
		// merged body. Serve a valid item response for the fetch.
		if r.Method == http.MethodGet && r.URL.Path == "/Items" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Items":[{"Name":"Portishead","Id":"jf-artist-1"}]}`))
			return
		}
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/Items/") {
			select {
			case received <- struct{}{}:
			default:
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	router, artistSvc := testRouter(t)
	addTestConnectionWithURL(t, router, "conn-jf", "Jellyfin", "jellyfin", srv.URL)
	a := addTestArtist(t, artistSvc, "Portishead")
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-jf", "jf-artist-1"); err != nil {
		t.Fatalf("setting platform id: %v", err)
	}

	body := strings.NewReader(`{"value":"Trip-hop pioneers"}`)
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/artists/"+a.ID+"/fields/biography", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	req.SetPathValue("field", "biography")
	w := httptest.NewRecorder()

	router.handleFieldUpdate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	select {
	case <-received:
		// push arrived
	case <-time.After(2 * time.Second):
		t.Error("auto-push: no request received by mock Jellyfin server within 2s")
	}
}

// TestFieldUpdate_AutoPush_DoesNotBlockSave verifies that even if the mock
// server returns 500, the PATCH handler still returns HTTP 200 immediately,
// and that the push was still attempted (error path is exercised, not skipped).
func TestFieldUpdate_AutoPush_DoesNotBlockSave(t *testing.T) {
	attempted := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case attempted <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	router, artistSvc := testRouter(t)
	addTestConnectionWithURL(t, router, "conn-emby", "Emby", "emby", srv.URL)
	a := addTestArtist(t, artistSvc, "Error Push Artist")
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-artist-4"); err != nil {
		t.Fatalf("setting platform id: %v", err)
	}

	body := strings.NewReader(`{"value":"Some bio"}`)
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/artists/"+a.ID+"/fields/biography", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	req.SetPathValue("field", "biography")
	w := httptest.NewRecorder()

	router.handleFieldUpdate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify the push was attempted despite the server error.
	select {
	case <-attempted:
		// push was fired
	case <-time.After(2 * time.Second):
		t.Error("auto-push: push was not attempted within 2s; expected fire-and-forget even on error")
	}
}
