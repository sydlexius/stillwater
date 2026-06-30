package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/webhook"
)

func webhookValidationRouter(t *testing.T) *Router {
	t.Helper()
	r, _ := testRouter(t)
	r.webhookService = webhook.NewService(r.db)
	return r
}

// TestHandleCreateWebhook_RejectsUnknownEvent: a subscription naming an event
// type not in event.WebhookEventTypes is rejected at the API boundary (400),
// rather than silently stored and never fired (#2009 #6).
func TestHandleCreateWebhook_RejectsUnknownEvent(t *testing.T) {
	t.Parallel()
	r := webhookValidationRouter(t)

	body := `{"name":"wh","url":"https://example.test/hook","type":"generic","events":["artist.new","bogus.event"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handleCreateWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "bogus.event") {
		t.Errorf("error response should name the bad event; body: %s", w.Body.String())
	}
}

// TestHandleCreateWebhook_AcceptsValidEvents: a subscription using only
// canonical event types is created (201).
func TestHandleCreateWebhook_AcceptsValidEvents(t *testing.T) {
	t.Parallel()
	r := webhookValidationRouter(t)

	body := `{"name":"wh","url":"https://example.test/hook","type":"generic","events":["artist.new","metadata.fixed"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handleCreateWebhook(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}
}

// TestHandleUpdateWebhook_RejectsUnknownEvent: the same validation applies on
// update.
func TestHandleUpdateWebhook_RejectsUnknownEvent(t *testing.T) {
	t.Parallel()
	r := webhookValidationRouter(t)

	// Seed a valid webhook directly via the service.
	wh := &webhook.Webhook{Name: "wh", URL: "https://example.test/hook", Type: "generic", Events: []string{"artist.new"}, Enabled: true}
	if err := r.webhookService.Create(context.Background(), wh); err != nil {
		t.Fatalf("seeding webhook: %v", err)
	}

	body := `{"events":["scan.completed","not.a.real.event"]}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/webhooks/"+wh.ID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", wh.ID)
	w := httptest.NewRecorder()
	r.handleUpdateWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not.a.real.event") {
		t.Errorf("error response should name the bad event; body: %s", w.Body.String())
	}
}
