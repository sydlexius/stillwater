package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
