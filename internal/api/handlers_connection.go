package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/connection/lidarr"
	"github.com/sydlexius/stillwater/web/templates"
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

	libs, err := r.libraryService.ListByConnectionID(req.Context(), id)
	if err != nil {
		r.logger.Error("listing libraries for connection", "connection_id", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	artistCount, err := r.libraryService.CountArtistsByConnectionID(req.Context(), id)
	if err != nil {
		r.logger.Error("counting artists for connection", "connection_id", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	resp := toConnectionResponse(*c)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":              resp.ID,
		"name":            resp.Name,
		"type":            resp.Type,
		"url":             resp.URL,
		"has_key":         resp.HasKey,
		"enabled":         resp.Enabled,
		"status":          resp.Status,
		"status_message":  resp.StatusMessage,
		"last_checked_at": resp.LastCheckedAt,
		"created_at":      resp.CreatedAt,
		"updated_at":      resp.UpdatedAt,
		"library_count":   len(libs),
		"artist_count":    artistCount,
	})
}

// testConnectionDirect tests a connection without requiring it to be saved first.
// The client is constructed directly from the provided URL and API key.
func (r *Router) testConnectionDirect(ctx context.Context, connType, url, apiKey string) error {
	testCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	switch connType {
	case connection.TypeEmby:
		return emby.New(url, apiKey, r.logger).TestConnection(testCtx)
	case connection.TypeJellyfin:
		return jellyfin.New(url, apiKey, r.logger).TestConnection(testCtx)
	case connection.TypeLidarr:
		return lidarr.New(url, apiKey, r.logger).TestConnection(testCtx)
	default:
		return nil
	}
}

func (r *Router) handleCreateConnection(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		URL      string `json:"url"`
		APIKey   string `json:"api_key"` //nolint:gosec // G101: not a hardcoded secret, this is a request field
		Enabled  bool   `json:"enabled"`
		SkipTest bool   `json:"skip_test"` //nolint:gosec // G101: not a credential
	}
	if strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
	} else {
		body.Name = req.FormValue("name")
		body.Type = req.FormValue("type")
		body.URL = req.FormValue("url")
		body.APIKey = req.FormValue("api_key")
		body.Enabled = true
		body.SkipTest = req.FormValue("skip_test") == "true"
	}

	isOOBE := strings.Contains(req.Header.Get("HX-Current-URL"), "/setup/wizard")

	// Test-before-save: verify the connection works before persisting.
	if !body.SkipTest {
		if testErr := r.testConnectionDirect(req.Context(), body.Type, body.URL, body.APIKey); testErr != nil {
			r.logger.Info("connection test failed before save", "type", body.Type, "url", body.URL, "error", testErr)
			if isHTMXRequest(req) {
				renderTempl(w, req, templates.ConnectionTestSaveFailure(body.Type, body.Name, body.URL, body.APIKey, testErr.Error(), isOOBE))
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{
				"status": "test_failed",
				"error":  testErr.Error(),
			})
			return
		}
	}

	// Prevent duplicate connections with the same type+url
	existing, err := r.connectionService.GetByTypeAndURL(req.Context(), body.Type, body.URL)
	if err != nil {
		r.logger.Error("checking for existing connection", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if existing != nil {
		existing.Name = body.Name
		if body.APIKey != "" {
			existing.APIKey = body.APIKey
		}
		existing.Enabled = body.Enabled
		if updateErr := r.connectionService.Update(req.Context(), existing); updateErr != nil {
			r.logger.Error("updating existing connection", "error", updateErr)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		// Persist test status for the updated connection.
		connStatus := "unknown"
		if !body.SkipTest {
			connStatus = "ok"
		}
		if updateErr := r.connectionService.UpdateStatus(req.Context(), existing.ID, connStatus, ""); updateErr != nil {
			r.logger.Error("updating connection status after save", "error", updateErr)
		}
		r.handleCreateConnectionSuccess(w, req, *existing, isOOBE)
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
	// Persist test status for the new connection.
	connStatus := "unknown"
	if !body.SkipTest {
		connStatus = "ok"
	}
	if updateErr := r.connectionService.UpdateStatus(req.Context(), c.ID, connStatus, ""); updateErr != nil {
		r.logger.Error("updating connection status after create", "error", updateErr)
	}
	r.handleCreateConnectionSuccess(w, req, *c, isOOBE)
}

// handleCreateConnectionSuccess sends the appropriate response after a
// successful connection create/update. For HTMX Settings requests, it triggers
// a page refresh. For HTMX OOBE requests, it returns JSON for the JS callback.
// For JSON API requests, it returns the connection response.
func (r *Router) handleCreateConnectionSuccess(w http.ResponseWriter, req *http.Request, c connection.Connection, isOOBE bool) {
	if isHTMXRequest(req) {
		if isOOBE {
			// OOBE relies on the JSON response + onConnectionSaved callback
			writeJSON(w, http.StatusOK, toConnectionResponse(c))
			return
		}
		// Settings page: trigger full page refresh to show the new connection
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	status := http.StatusCreated
	if c.UpdatedAt.After(c.CreatedAt) {
		status = http.StatusOK
	}
	writeJSON(w, status, toConnectionResponse(c))
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
	deleteLibraries := req.URL.Query().Get("deleteLibraries") == "true"
	deleteArtists := req.URL.Query().Get("deleteArtists") == "true"

	if deleteLibraries {
		libs, err := r.libraryService.ListByConnectionID(req.Context(), id)
		if err != nil {
			r.logger.Error("listing connection libraries for deletion", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		for _, lib := range libs {
			if deleteArtists {
				if err := r.libraryService.DeleteWithArtists(req.Context(), lib.ID); err != nil {
					r.logger.Error("deleting library with artists", "library_id", lib.ID, "error", err)
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
					return
				}
			} else {
				if err := r.libraryService.Delete(req.Context(), lib.ID); err != nil {
					r.logger.Error("deleting library", "library_id", lib.ID, "error", err)
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
					return
				}
			}
		}
	} else {
		// Default: clear library FK references. Imported libraries keep their
		// source/external_id for provenance.
		if err := r.libraryService.ClearConnectionID(req.Context(), id); err != nil {
			r.logger.Error("clearing library connection references", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	}

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
	case connection.TypeEmby:
		client := emby.New(conn.URL, conn.APIKey, r.logger)
		testErr = client.TestConnection(req.Context())
	case connection.TypeJellyfin:
		client := jellyfin.New(conn.URL, conn.APIKey, r.logger)
		testErr = client.TestConnection(req.Context())
	case connection.TypeLidarr:
		client := lidarr.New(conn.URL, conn.APIKey, r.logger)
		testErr = client.TestConnection(req.Context())
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported connection type: " + conn.Type})
		return
	}

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
