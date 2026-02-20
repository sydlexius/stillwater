package api

import (
	"encoding/json"
	"net/http"

	"github.com/sydlexius/stillwater/internal/connection"
)

// connectionResponse is a Connection without the raw API key for list responses.
type connectionResponse struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Type          string  `json:"type"`
	URL           string  `json:"url"`
	HasKey        bool    `json:"has_key"`
	Enabled       bool    `json:"enabled"`
	Status        string  `json:"status"`
	StatusMessage string  `json:"status_message,omitempty"`
	LastCheckedAt *string `json:"last_checked_at,omitempty"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
}

func toConnectionResponse(c connection.Connection) connectionResponse {
	resp := connectionResponse{
		ID:            c.ID,
		Name:          c.Name,
		Type:          c.Type,
		URL:           c.URL,
		HasKey:        c.APIKey != "",
		Enabled:       c.Enabled,
		Status:        c.Status,
		StatusMessage: c.StatusMessage,
		CreatedAt:     c.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:     c.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if c.LastCheckedAt != nil {
		s := c.LastCheckedAt.Format("2006-01-02T15:04:05Z07:00")
		resp.LastCheckedAt = &s
	}
	return resp
}

func (r *Router) handleListConnections(w http.ResponseWriter, req *http.Request) {
	conns, err := r.connectionService.List(req.Context())
	if err != nil {
		r.logger.Error("listing connections", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	resp := make([]connectionResponse, len(conns))
	for i, c := range conns {
		resp[i] = toConnectionResponse(c)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (r *Router) handleGetConnection(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	c, err := r.connectionService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}
	writeJSON(w, http.StatusOK, toConnectionResponse(*c))
}

func (r *Router) handleCreateConnection(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Name    string `json:"name"`
		Type    string `json:"type"`
		URL     string `json:"url"`
		APIKey  string `json:"api_key"` //nolint:gosec // G101: not a hardcoded secret, this is a request field
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	c := &connection.Connection{
		Name:    body.Name,
		Type:    body.Type,
		URL:     body.URL,
		APIKey:  body.APIKey,
		Enabled: body.Enabled,
	}
	if err := r.connectionService.Create(req.Context(), c); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, toConnectionResponse(*c))
}

func (r *Router) handleUpdateConnection(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	existing, err := r.connectionService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}

	var body struct {
		Name    string `json:"name"`
		Type    string `json:"type"`
		URL     string `json:"url"`
		APIKey  string `json:"api_key"` //nolint:gosec // G101: not a hardcoded secret, this is a request field
		Enabled *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if body.Name != "" {
		existing.Name = body.Name
	}
	if body.Type != "" {
		existing.Type = body.Type
	}
	if body.URL != "" {
		existing.URL = body.URL
	}
	if body.APIKey != "" {
		existing.APIKey = body.APIKey
	}
	if body.Enabled != nil {
		existing.Enabled = *body.Enabled
	}

	if err := r.connectionService.Update(req.Context(), existing); err != nil {
		r.logger.Error("updating connection", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, toConnectionResponse(*existing))
}

func (r *Router) handleDeleteConnection(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if err := r.connectionService.Delete(req.Context(), id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (r *Router) handleTestConnection(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	conn, err := r.connectionService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}

	var testErr error
	switch conn.Type {
	case connection.TypeEmby, connection.TypeJellyfin, connection.TypeLidarr:
		// Platform clients will be wired in Steps 2-4
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "test not yet available for " + conn.Type})
		return
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported connection type: " + conn.Type})
		return
	}

	// This code will be reached once platform clients are wired in
	status := "ok"
	msg := ""
	if testErr != nil {
		status = "error"
		msg = testErr.Error()
	}
	if updateErr := r.connectionService.UpdateStatus(req.Context(), id, status, msg); updateErr != nil {
		r.logger.Error("updating connection status", "error", updateErr)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status, "message": msg})
}
