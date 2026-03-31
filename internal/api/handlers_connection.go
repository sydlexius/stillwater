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
	ID                    string  `json:"id"`
	Name                  string  `json:"name"`
	Type                  string  `json:"type"`
	URL                   string  `json:"url"`
	HasKey                bool    `json:"has_key"`
	HasPlatformUserID     bool    `json:"has_platform_user_id"`
	Enabled               bool    `json:"enabled"`
	Status                string  `json:"status"`
	StatusMessage         string  `json:"status_message,omitempty"`
	LastCheckedAt         *string `json:"last_checked_at,omitempty"`
	CreatedAt             string  `json:"created_at"`
	UpdatedAt             string  `json:"updated_at"`
	FeatureLibraryImport  bool    `json:"feature_library_import"`
	FeatureNFOWrite       bool    `json:"feature_nfo_write"`
	FeatureImageWrite     bool    `json:"feature_image_write"`
	FeatureMetadataPush   bool    `json:"feature_metadata_push"`
	FeatureTriggerRefresh bool    `json:"feature_trigger_refresh"`
}

func toConnectionResponse(c connection.Connection) connectionResponse {
	resp := connectionResponse{
		ID:                    c.ID,
		Name:                  c.Name,
		Type:                  c.Type,
		URL:                   c.URL,
		HasKey:                c.APIKey != "",
		HasPlatformUserID:     c.PlatformUserID != "",
		Enabled:               c.Enabled,
		Status:                c.Status,
		StatusMessage:         c.StatusMessage,
		CreatedAt:             c.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:             c.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
		FeatureLibraryImport:  c.FeatureLibraryImport,
		FeatureNFOWrite:       c.FeatureNFOWrite,
		FeatureImageWrite:     c.FeatureImageWrite,
		FeatureMetadataPush:   c.FeatureMetadataPush,
		FeatureTriggerRefresh: c.FeatureTriggerRefresh,
	}
	if c.LastCheckedAt != nil {
		s := c.LastCheckedAt.Format("2006-01-02T15:04:05Z07:00")
		resp.LastCheckedAt = &s
	}
	return resp
}

// handleListConnections returns all configured connections.
// GET /api/v1/connections
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

// handleGetConnection returns a single connection by ID.
// GET /api/v1/connections/{id}
func (r *Router) handleGetConnection(w http.ResponseWriter, req *http.Request) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
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
		"id":                      resp.ID,
		"name":                    resp.Name,
		"type":                    resp.Type,
		"url":                     resp.URL,
		"has_key":                 resp.HasKey,
		"has_platform_user_id":    resp.HasPlatformUserID,
		"enabled":                 resp.Enabled,
		"status":                  resp.Status,
		"status_message":          resp.StatusMessage,
		"last_checked_at":         resp.LastCheckedAt,
		"created_at":              resp.CreatedAt,
		"updated_at":              resp.UpdatedAt,
		"feature_library_import":  resp.FeatureLibraryImport,
		"feature_nfo_write":       resp.FeatureNFOWrite,
		"feature_image_write":     resp.FeatureImageWrite,
		"feature_metadata_push":   resp.FeatureMetadataPush,
		"feature_trigger_refresh": resp.FeatureTriggerRefresh,
		"library_count":           len(libs),
		"artist_count":            artistCount,
	})
}

// testConnectionDirect tests a connection without requiring it to be saved first.
// The client is constructed directly from the provided URL and API key.
func (r *Router) testConnectionDirect(ctx context.Context, connType, url, apiKey string) error {
	testCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	switch connType {
	case connection.TypeEmby:
		return emby.New(url, apiKey, "", r.logger).TestConnection(testCtx)
	case connection.TypeJellyfin:
		return jellyfin.New(url, apiKey, "", r.logger).TestConnection(testCtx)
	case connection.TypeLidarr:
		return lidarr.New(url, apiKey, r.logger).TestConnection(testCtx)
	default:
		return nil
	}
}

// resolvePlatformUserID calls GET /Users on an emby/jellyfin server and returns the first
// user ID. Returns an empty string without logging when the connection type does not
// support users. Logs a warning and returns an empty string when resolution fails.
// This is called after a successful connection test so that the user ID can be persisted
// in the connections table.
func (r *Router) resolvePlatformUserID(ctx context.Context, connType, url, apiKey string) string {
	resolveCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	switch connType {
	case connection.TypeEmby:
		uid, err := emby.New(url, apiKey, "", r.logger).GetFirstUserID(resolveCtx)
		if err != nil {
			r.logger.Warn("could not resolve emby platform user id", "error", err)
			return ""
		}
		return uid
	case connection.TypeJellyfin:
		uid, err := jellyfin.New(url, apiKey, "", r.logger).GetFirstUserID(resolveCtx)
		if err != nil {
			r.logger.Warn("could not resolve jellyfin platform user id", "error", err)
			return ""
		}
		return uid
	}
	return ""
}

// handleCreateConnection creates a new platform connection with test-before-save.
// POST /api/v1/connections
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
		body.Name = req.FormValue("name")      //nolint:gosec // G120: admin-only form on self-hosted instance
		body.Type = req.FormValue("type")      //nolint:gosec // G120: admin-only form on self-hosted instance
		body.URL = req.FormValue("url")        //nolint:gosec // G120: admin-only form on self-hosted instance
		body.APIKey = req.FormValue("api_key") //nolint:gosec // G120: admin-only form on self-hosted instance
		body.Enabled = true
		body.SkipTest = req.FormValue("skip_test") == "true" //nolint:gosec // G120: admin-only form on self-hosted instance
	}

	isOOBE := strings.Contains(req.Header.Get("HX-Current-URL"), "/setup/wizard")

	// Test-before-save: verify the connection works before persisting.
	if !body.SkipTest {
		if testErr := r.testConnectionDirect(req.Context(), body.Type, body.URL, body.APIKey); testErr != nil {
			r.logger.Info("connection test failed before save", "type", body.Type, "url", body.URL, "error", testErr)
			if isHTMXRequest(req) {
				if isOOBE {
					w.Header().Set("HX-Retarget", "#ob-conn-result-"+body.Type)
					w.Header().Set("HX-Reswap", "innerHTML")
				}
				w.WriteHeader(http.StatusUnprocessableEntity)
				renderTempl(w, req, templates.ConnectionTestSaveFailure(body.Type, body.Name, body.URL, body.APIKey, testErr.Error(), isOOBE))
				return
			}
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"status": "test_failed",
				"error":  testErr.Error(),
			})
			return
		}
	}

	// Resolve platform user ID for emby/jellyfin connections after a successful test.
	var platformUserID string
	if !body.SkipTest {
		platformUserID = r.resolvePlatformUserID(req.Context(), body.Type, body.URL, body.APIKey)
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
		if !body.SkipTest {
			existing.PlatformUserID = platformUserID
		}
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

	// Lidarr is a read-only metadata source (MBID seeding). Default its
	// library-import and write feature flags to false.
	libImport := body.Type != connection.TypeLidarr
	nfoWrite := body.Type != connection.TypeLidarr
	imageWrite := body.Type != connection.TypeLidarr

	c := &connection.Connection{
		Name:                 body.Name,
		Type:                 body.Type,
		URL:                  body.URL,
		APIKey:               body.APIKey,
		Enabled:              body.Enabled,
		FeatureLibraryImport: libImport,
		FeatureNFOWrite:      nfoWrite,
		FeatureImageWrite:    imageWrite,
		PlatformUserID:       platformUserID,
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
			// OOBE relies on the JSON response + onConnectionSaved callback.
			// Prevent HTMX from swapping the JSON into the page.
			w.Header().Set("HX-Reswap", "none")
			writeJSON(w, http.StatusOK, toConnectionResponse(c))
			return
		}
		// Settings page: trigger full page refresh to show the new connection.
		// Use 204 and HX-Reswap: none to prevent HTMX from swapping an empty
		// response into the target element before the refresh occurs.
		w.Header().Set("HX-Refresh", "true")
		w.Header().Set("HX-Reswap", "none")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	status := http.StatusCreated
	if c.UpdatedAt.After(c.CreatedAt) {
		status = http.StatusOK
	}
	writeJSON(w, status, toConnectionResponse(c))
}

// handleUpdateConnection updates an existing connection's configuration.
// PUT /api/v1/connections/{id}
func (r *Router) handleUpdateConnection(w http.ResponseWriter, req *http.Request) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	existing, err := r.connectionService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}

	var body struct {
		Name                  string `json:"name"`
		Type                  string `json:"type"`
		URL                   string `json:"url"`
		APIKey                string `json:"api_key"` //nolint:gosec // G101: not a hardcoded secret, this is a request field
		Enabled               *bool  `json:"enabled"`
		FeatureLibraryImport  *bool  `json:"feature_library_import"`
		FeatureNFOWrite       *bool  `json:"feature_nfo_write"`
		FeatureImageWrite     *bool  `json:"feature_image_write"`
		FeatureMetadataPush   *bool  `json:"feature_metadata_push"`
		FeatureTriggerRefresh *bool  `json:"feature_trigger_refresh"`
	}
	if !DecodeJSON(w, req, &body) {
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
	if body.FeatureLibraryImport != nil {
		existing.FeatureLibraryImport = *body.FeatureLibraryImport
	}
	if body.FeatureNFOWrite != nil {
		existing.FeatureNFOWrite = *body.FeatureNFOWrite
	}
	if body.FeatureImageWrite != nil {
		existing.FeatureImageWrite = *body.FeatureImageWrite
	}
	if body.FeatureMetadataPush != nil {
		existing.FeatureMetadataPush = *body.FeatureMetadataPush
	}
	if body.FeatureTriggerRefresh != nil {
		existing.FeatureTriggerRefresh = *body.FeatureTriggerRefresh
	}

	if err := r.connectionService.Update(req.Context(), existing); err != nil {
		r.logger.Error("updating connection", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, toConnectionResponse(*existing))
}

// handleDeleteConnection removes a connection. Libraries are optionally deleted
// via the deleteLibraries query parameter (and their artists via deleteArtists);
// the default clears the connection reference without deleting libraries.
// DELETE /api/v1/connections/{id}
func (r *Router) handleDeleteConnection(w http.ResponseWriter, req *http.Request) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
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

// handleTestConnection tests connectivity to a platform and updates its status.
// POST /api/v1/connections/{id}/test
func (r *Router) handleTestConnection(w http.ResponseWriter, req *http.Request) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	conn, err := r.connectionService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}

	testCtx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()

	var testErr error
	var driftWarnings []string
	switch conn.Type {
	case connection.TypeEmby:
		client := emby.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger)
		testErr = client.TestConnection(testCtx)
		if testErr == nil {
			uid, uidErr := client.GetFirstUserID(testCtx)
			if uidErr != nil {
				r.logger.Warn("could not resolve emby platform user id", "error", uidErr)
			}
			if updErr := r.connectionService.UpdatePlatformUserID(testCtx, id, uid); updErr != nil {
				r.logger.Error("persisting emby platform user id", "error", updErr)
			}
			// Drift detection: check for conflicting platform settings.
			if settings, settingsErr := client.GetLibrarySettings(testCtx); settingsErr == nil {
				for _, s := range settings {
					if s.HasConflicts {
						warning := "library " + s.LibraryName + " has active fetchers/savers that may overwrite Stillwater-managed metadata"
						driftWarnings = append(driftWarnings, warning)
						r.logger.Warn("emby platform settings drift detected", "connection_id", id, "library", s.LibraryName)
					}
				}
			} else {
				r.logger.Warn("could not inspect emby platform settings for drift", "connection_id", id, "error", settingsErr)
				driftWarnings = append(driftWarnings, "could not inspect platform settings for drift")
			}
		}
	case connection.TypeJellyfin:
		client := jellyfin.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger)
		testErr = client.TestConnection(testCtx)
		if testErr == nil {
			uid, uidErr := client.GetFirstUserID(testCtx)
			if uidErr != nil {
				r.logger.Warn("could not resolve jellyfin platform user id", "error", uidErr)
			}
			if updErr := r.connectionService.UpdatePlatformUserID(testCtx, id, uid); updErr != nil {
				r.logger.Error("persisting jellyfin platform user id", "error", updErr)
			}
			// Drift detection: check for conflicting platform settings.
			if settings, settingsErr := client.GetLibrarySettings(testCtx); settingsErr == nil {
				for _, s := range settings {
					if s.HasConflicts {
						warning := "library " + s.LibraryName + " has active fetchers that may overwrite Stillwater-managed metadata"
						driftWarnings = append(driftWarnings, warning)
						r.logger.Warn("jellyfin platform settings drift detected", "connection_id", id, "library", s.LibraryName)
					}
				}
			} else {
				r.logger.Warn("could not inspect jellyfin platform settings for drift", "connection_id", id, "error", settingsErr)
				driftWarnings = append(driftWarnings, "could not inspect platform settings for drift")
			}
		}
	case connection.TypeLidarr:
		client := lidarr.New(conn.URL, conn.APIKey, r.logger)
		testErr = client.TestConnection(testCtx)
		if testErr == nil {
			// Drift detection: check for enabled metadata consumers.
			if consumers, consumersErr := client.GetMetadataConsumers(testCtx); consumersErr == nil {
				for _, c := range consumers {
					if c.Enabled {
						warning := "metadata consumer " + c.ConsumerName + " is enabled and may write NFO files"
						driftWarnings = append(driftWarnings, warning)
						r.logger.Warn("lidarr platform settings drift detected", "connection_id", id, "consumer", c.ConsumerName)
					}
				}
			} else {
				r.logger.Warn("could not inspect lidarr platform settings for drift", "connection_id", id, "error", consumersErr)
				driftWarnings = append(driftWarnings, "could not inspect platform settings for drift")
			}
		}
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
	resp := map[string]any{"status": status, "message": msg}
	if len(driftWarnings) > 0 {
		resp["drift_warnings"] = driftWarnings
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleUpdateConnectionFeatures toggles feature flags on a connection.
// PATCH /api/v1/connections/{id}/features
func (r *Router) handleUpdateConnectionFeatures(w http.ResponseWriter, req *http.Request) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	existing, err := r.connectionService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}

	var body struct {
		FeatureLibraryImport  *bool `json:"feature_library_import"`
		FeatureNFOWrite       *bool `json:"feature_nfo_write"`
		FeatureImageWrite     *bool `json:"feature_image_write"`
		FeatureMetadataPush   *bool `json:"feature_metadata_push"`
		FeatureTriggerRefresh *bool `json:"feature_trigger_refresh"`
	}
	if !DecodeJSON(w, req, &body) {
		return
	}

	libImport := existing.FeatureLibraryImport
	nfoWrite := existing.FeatureNFOWrite
	imageWrite := existing.FeatureImageWrite
	metadataPush := existing.FeatureMetadataPush
	triggerRefresh := existing.FeatureTriggerRefresh
	if body.FeatureLibraryImport != nil {
		libImport = *body.FeatureLibraryImport
	}
	if body.FeatureNFOWrite != nil {
		nfoWrite = *body.FeatureNFOWrite
	}
	if body.FeatureImageWrite != nil {
		imageWrite = *body.FeatureImageWrite
	}
	if body.FeatureMetadataPush != nil {
		metadataPush = *body.FeatureMetadataPush
	}
	if body.FeatureTriggerRefresh != nil {
		triggerRefresh = *body.FeatureTriggerRefresh
	}

	if err := r.connectionService.UpdateFeatures(req.Context(), id, libImport, nfoWrite, imageWrite, metadataPush, triggerRefresh); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
			return
		}
		r.logger.Error("updating connection features", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// handleGetPlatformSettings returns the fetcher/saver/downloader configuration for all
// music libraries on a connection's platform.
// GET /api/v1/connections/{id}/platform-settings
func (r *Router) handleGetPlatformSettings(w http.ResponseWriter, req *http.Request) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	conn, err := r.connectionService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}

	settingsCtx, cancel := context.WithTimeout(req.Context(), 15*time.Second)
	defer cancel()

	switch conn.Type {
	case connection.TypeEmby:
		client := emby.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger)
		settings, settingsErr := client.GetLibrarySettings(settingsCtx)
		if settingsErr != nil {
			r.logger.Error("reading emby platform settings", "connection_id", id, "error", settingsErr)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not read platform settings"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"connection_type": conn.Type,
			"libraries":       settings,
		})

	case connection.TypeJellyfin:
		client := jellyfin.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger)
		settings, settingsErr := client.GetLibrarySettings(settingsCtx)
		if settingsErr != nil {
			r.logger.Error("reading jellyfin platform settings", "connection_id", id, "error", settingsErr)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not read platform settings"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"connection_type": conn.Type,
			"libraries":       settings,
			"note":            "Jellyfin ignores MetadataSavers=[] for NFO writes. Use lockdata injection for reliable NFO protection.",
		})

	case connection.TypeLidarr:
		client := lidarr.New(conn.URL, conn.APIKey, r.logger)
		consumers, consumersErr := client.GetMetadataConsumers(settingsCtx)
		if consumersErr != nil {
			r.logger.Error("reading lidarr metadata consumers", "connection_id", id, "error", consumersErr)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not read platform settings"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"connection_type":    conn.Type,
			"metadata_consumers": consumers,
			"note":               "Lidarr metadata consumers are a global setting, not per-library.",
		})

	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported connection type"})
	}
}

// handleDisablePlatformSettings disables conflicting fetchers/savers for a specific library.
// For Emby/Jellyfin, this operates per-library. For Lidarr, the consumer_id body field
// identifies the global metadata consumer to disable.
// POST /api/v1/connections/{id}/platform-settings/disable
func (r *Router) handleDisablePlatformSettings(w http.ResponseWriter, req *http.Request) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	conn, err := r.connectionService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}

	var body struct {
		LibraryID  string `json:"library_id"`
		ConsumerID int    `json:"consumer_id"`
	}
	if !DecodeJSON(w, req, &body) {
		return
	}

	disableCtx, cancel := context.WithTimeout(req.Context(), 15*time.Second)
	defer cancel()

	switch conn.Type {
	case connection.TypeEmby:
		if body.LibraryID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "library_id is required for Emby connections"})
			return
		}
		client := emby.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger)
		if disableErr := client.DisableConflictingSettings(disableCtx, body.LibraryID); disableErr != nil {
			r.logger.Error("disabling emby conflicting settings", "connection_id", id, "library_id", body.LibraryID, "error", disableErr)
			if strings.Contains(disableErr.Error(), "not found") {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "library not found on platform"})
				return
			}
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not disable platform settings"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "disabled"})

	case connection.TypeJellyfin:
		if body.LibraryID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "library_id is required for Jellyfin connections"})
			return
		}
		client := jellyfin.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger)
		if disableErr := client.DisableConflictingSettings(disableCtx, body.LibraryID); disableErr != nil {
			r.logger.Error("disabling jellyfin conflicting settings", "connection_id", id, "library_id", body.LibraryID, "error", disableErr)
			if strings.Contains(disableErr.Error(), "not found") {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "library not found on platform"})
				return
			}
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not disable platform settings"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"status": "disabled",
			"note":   "NFO protection requires lockdata injection. Clearing MetadataSavers alone is not reliable for Jellyfin.",
		})

	case connection.TypeLidarr:
		if body.ConsumerID == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "consumer_id is required for Lidarr connections"})
			return
		}
		client := lidarr.New(conn.URL, conn.APIKey, r.logger)
		if disableErr := client.DisableMetadataConsumer(disableCtx, body.ConsumerID); disableErr != nil {
			r.logger.Error("disabling lidarr metadata consumer", "connection_id", id, "consumer_id", body.ConsumerID, "error", disableErr)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not disable metadata consumer"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "disabled"})

	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported connection type"})
	}
}

// handleGetPlatformSummary returns a summary of platform setting management status for
// a connection. Used for the connection card badge.
// GET /api/v1/connections/{id}/platform-summary
func (r *Router) handleGetPlatformSummary(w http.ResponseWriter, req *http.Request) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	conn, err := r.connectionService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}

	summaryCtx, cancel := context.WithTimeout(req.Context(), 15*time.Second)
	defer cancel()

	switch conn.Type {
	case connection.TypeEmby:
		client := emby.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger)
		settings, settingsErr := client.GetLibrarySettings(summaryCtx)
		if settingsErr != nil {
			r.logger.Error("reading emby settings for summary", "connection_id", id, "error", settingsErr)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not read platform settings"})
			return
		}
		total := len(settings)
		managed := 0
		for _, s := range settings {
			if !s.HasConflicts {
				managed++
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"total_libraries":   total,
			"managed_libraries": managed,
			"has_conflicts":     managed < total,
		})

	case connection.TypeJellyfin:
		client := jellyfin.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger)
		settings, settingsErr := client.GetLibrarySettings(summaryCtx)
		if settingsErr != nil {
			r.logger.Error("reading jellyfin settings for summary", "connection_id", id, "error", settingsErr)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not read platform settings"})
			return
		}
		total := len(settings)
		managed := 0
		for _, s := range settings {
			if !s.HasConflicts {
				managed++
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"total_libraries":   total,
			"managed_libraries": managed,
			"has_conflicts":     managed < total,
			"needs_lockdata":    true,
		})

	case connection.TypeLidarr:
		client := lidarr.New(conn.URL, conn.APIKey, r.logger)
		consumers, consumersErr := client.GetMetadataConsumers(summaryCtx)
		if consumersErr != nil {
			r.logger.Error("reading lidarr consumers for summary", "connection_id", id, "error", consumersErr)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not read platform settings"})
			return
		}
		hasConflicts := false
		for _, c := range consumers {
			if c.Enabled {
				hasConflicts = true
				break
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"total_consumers": len(consumers),
			"has_conflicts":   hasConflicts,
		})

	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported connection type"})
	}
}
