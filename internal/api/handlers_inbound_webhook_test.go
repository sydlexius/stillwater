package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/webhook"
)

// TestHandleEmbyWebhook_OK verifies the handler returns 200 immediately.
func TestHandleEmbyWebhook_OK(t *testing.T) {
	r, _ := testRouter(t)

	body := `{"Event":"system.notificationtest"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/inbound/emby",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleEmbyWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
}

// TestHandleEmbyWebhook_InvalidJSON verifies 400 on bad JSON.
func TestHandleEmbyWebhook_InvalidJSON(t *testing.T) {
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/inbound/emby",
		strings.NewReader("{bad json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleEmbyWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandleEmbyWebhook_MissingEvent verifies 400 when Event field is absent.
func TestHandleEmbyWebhook_MissingEvent(t *testing.T) {
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/inbound/emby",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleEmbyWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandleEmbyWebhook_UnknownEventType verifies 200 with unknown event type (handled gracefully).
func TestHandleEmbyWebhook_UnknownEventType(t *testing.T) {
	r, _ := testRouter(t)

	body := `{"Event":"some.unknown.event"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/inbound/emby",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleEmbyWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

// TestHandleJellyfinWebhook_OK verifies the handler returns 200 immediately.
func TestHandleJellyfinWebhook_OK(t *testing.T) {
	r, _ := testRouter(t)

	body := `{"NotificationType":"Test"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/inbound/jellyfin",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleJellyfinWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
}

// TestHandleJellyfinWebhook_InvalidJSON verifies 400 on bad JSON.
func TestHandleJellyfinWebhook_InvalidJSON(t *testing.T) {
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/inbound/jellyfin",
		strings.NewReader("{bad json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleJellyfinWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandleJellyfinWebhook_MissingNotificationType verifies 400 when field is absent.
func TestHandleJellyfinWebhook_MissingNotificationType(t *testing.T) {
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/inbound/jellyfin",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleJellyfinWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandleJellyfinWebhook_UnknownEventType verifies 200 with unknown event type.
func TestHandleJellyfinWebhook_UnknownEventType(t *testing.T) {
	r, _ := testRouter(t)

	body := `{"NotificationType":"SomeUnknownEvent"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/inbound/jellyfin",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleJellyfinWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

// TestLidarrArtistAdd_NilPipeline verifies that handleLidarrArtistAdd does not
// panic when pipeline is nil and an existing artist is found.
func TestLidarrArtistAdd_NilPipeline(t *testing.T) {
	r, artistSvc := testRouter(t)
	// testRouter does not set pipeline, so r.pipeline == nil.

	mbid := "a74b1b7f-71a5-4011-9441-d0b5e4122711"
	a := &artist.Artist{
		Name:          "Radiohead",
		SortName:      "Radiohead",
		Type:          "group",
		Path:          "/music/Radiohead",
		MusicBrainzID: mbid,
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	payload := webhook.LidarrPayload{
		EventType: webhook.LidarrEventArtistAdd,
		Artist:    &webhook.LidarrArtist{Name: "Radiohead", MBId: mbid},
	}

	// Should not panic; should log warning and return gracefully.
	r.handleLidarrArtistAdd(context.Background(), payload)
}

// TestLidarrDownload_NilPipeline verifies that handleLidarrDownload does not
// panic when pipeline is nil and an existing artist is found.
func TestLidarrDownload_NilPipeline(t *testing.T) {
	r, artistSvc := testRouter(t)

	mbid := "a74b1b7f-71a5-4011-9441-d0b5e4122711"
	a := &artist.Artist{
		Name:          "Radiohead",
		SortName:      "Radiohead",
		Type:          "group",
		Path:          "/music/Radiohead",
		MusicBrainzID: mbid,
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	payload := webhook.LidarrPayload{
		EventType: webhook.LidarrEventDownload,
		Artist:    &webhook.LidarrArtist{Name: "Radiohead", MBId: mbid},
	}

	// Should not panic; should log warning and return gracefully.
	r.handleLidarrDownload(context.Background(), payload)
}
