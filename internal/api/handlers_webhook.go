package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/event"
	"github.com/sydlexius/stillwater/internal/webhook"
)

// handleListWebhooks returns all configured outbound webhooks.
// GET /api/v1/webhooks
func (r *Router) handleListWebhooks(w http.ResponseWriter, req *http.Request) {
	webhooks, err := r.webhookService.List(req.Context())
	if err != nil {
		r.logger.Error("listing webhooks", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if webhooks == nil {
		webhooks = []webhook.Webhook{}
	}
	writeJSON(w, http.StatusOK, webhooks)
}

// handleGetWebhook returns a single webhook by ID.
// GET /api/v1/webhooks/{id}
func (r *Router) handleGetWebhook(w http.ResponseWriter, req *http.Request) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	wh, err := r.webhookService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "webhook not found"})
		return
	}
	writeJSON(w, http.StatusOK, wh)
}

// handleCreateWebhook registers a new outbound webhook.
// POST /api/v1/webhooks
func (r *Router) handleCreateWebhook(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Name    string   `json:"name"`
		URL     string   `json:"url"`
		Type    string   `json:"type"`
		Events  []string `json:"events"`
		Enabled bool     `json:"enabled"`
	}
	if strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
	} else {
		body.Name = req.FormValue("name")
		body.URL = req.FormValue("url")
		body.Type = req.FormValue("type")
		body.Enabled = true
	}

	wh := &webhook.Webhook{
		Name:    body.Name,
		URL:     body.URL,
		Type:    body.Type,
		Events:  body.Events,
		Enabled: body.Enabled,
	}
	if wh.Events == nil {
		wh.Events = []string{}
	}

	if err := r.webhookService.Create(req.Context(), wh); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, wh)
}

// handleUpdateWebhook partially updates an existing webhook's configuration.
// PUT /api/v1/webhooks/{id}
func (r *Router) handleUpdateWebhook(w http.ResponseWriter, req *http.Request) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	existing, err := r.webhookService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "webhook not found"})
		return
	}

	var body struct {
		Name    string   `json:"name"`
		URL     string   `json:"url"`
		Type    string   `json:"type"`
		Events  []string `json:"events"`
		Enabled *bool    `json:"enabled"`
	}
	if !DecodeJSON(w, req, &body) {
		return
	}

	if body.Name != "" {
		existing.Name = body.Name
	}
	if body.URL != "" {
		existing.URL = body.URL
	}
	if body.Type != "" {
		existing.Type = body.Type
	}
	if body.Events != nil {
		existing.Events = body.Events
	}
	if body.Enabled != nil {
		existing.Enabled = *body.Enabled
	}

	if err := r.webhookService.Update(req.Context(), existing); err != nil {
		r.logger.Error("updating webhook", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, existing)
}

// handleDeleteWebhook removes a webhook by ID.
// DELETE /api/v1/webhooks/{id}
func (r *Router) handleDeleteWebhook(w http.ResponseWriter, req *http.Request) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	if err := r.webhookService.Delete(req.Context(), id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleTestWebhook sends a test event to the specified webhook.
// POST /api/v1/webhooks/{id}/test
func (r *Router) handleTestWebhook(w http.ResponseWriter, req *http.Request) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	wh, err := r.webhookService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "webhook not found"})
		return
	}

	testEvent := event.Event{
		Type:      event.Type("test"),
		Timestamp: time.Now().UTC(),
		Data: map[string]any{
			"message": "Test event from Stillwater",
		},
	}

	r.webhookDispatcher.HandleEvent(testEvent)
	_ = wh // we used wh to validate it exists
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}
