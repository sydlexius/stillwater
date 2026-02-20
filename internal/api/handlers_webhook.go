package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/sydlexius/stillwater/internal/event"
	"github.com/sydlexius/stillwater/internal/webhook"
)

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

func (r *Router) handleGetWebhook(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	wh, err := r.webhookService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "webhook not found"})
		return
	}
	writeJSON(w, http.StatusOK, wh)
}

func (r *Router) handleCreateWebhook(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Name    string   `json:"name"`
		URL     string   `json:"url"`
		Type    string   `json:"type"`
		Events  []string `json:"events"`
		Enabled bool     `json:"enabled"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
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

func (r *Router) handleUpdateWebhook(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
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
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
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

func (r *Router) handleDeleteWebhook(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if err := r.webhookService.Delete(req.Context(), id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (r *Router) handleTestWebhook(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
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
