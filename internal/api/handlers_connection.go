package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
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
	FeatureImageWrite     bool    `json:"feature_image_write"`
	FeatureMetadataPush   bool    `json:"feature_metadata_push"`
	FeatureTriggerRefresh bool    `json:"feature_trigger_refresh"`
	// PathMappings is the connection-level host<->platform path-mapping list,
	// applicable to Lidarr, Emby, and Jellyfin alike. Empty for a shared-mount
	// connection where Stillwater and the peer address the library
	// identically. See connection.Connection.PathMappings (#2303, #2380).
	PathMappings []connection.PathMapping `json:"path_mappings"`
}

func toConnectionResponse(c connection.Connection) connectionResponse {
	resp := connectionResponse{
		ID:                    c.ID,
		Name:                  c.Name,
		Type:                  c.Type,
		URL:                   c.URL,
		HasKey:                c.APIKey != "",
		HasPlatformUserID:     c.GetPlatformUserID() != "",
		Enabled:               c.Enabled,
		Status:                c.Status,
		StatusMessage:         c.StatusMessage,
		CreatedAt:             c.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:             c.UpdatedAt.UTC().Format(time.RFC3339),
		FeatureImageWrite:     c.GetFeatureImageWrite(),
		FeatureMetadataPush:   c.GetFeatureMetadataPush(),
		FeatureTriggerRefresh: c.GetFeatureTriggerRefresh(),
		PathMappings:          c.GetPathMappings(),
	}
	if c.LastCheckedAt != nil {
		s := c.LastCheckedAt.UTC().Format(time.RFC3339)
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
	for i := range conns {
		resp[i] = toConnectionResponse(conns[i])
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
		APIKey   string `json:"api_key"`
		Enabled  bool   `json:"enabled"`
		SkipTest bool   `json:"skip_test"`
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
			existing.SetPlatformUserID(platformUserID)
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
		// Best-effort: auto-derive Lidarr path mappings when none are set yet.
		r.applyInferredPathMappingsIfEmpty(req.Context(), existing)
		r.handleCreateConnectionSuccess(w, req, *existing, isOOBE)
		return
	}

	// Lidarr is a read-only metadata source (MBID seeding) with no write
	// features: setMediaServerDefaults seeds the three default write toggles
	// (true) plus the resolved platform user ID only for emby/jellyfin, and
	// is a no-op for Lidarr (Validate then allocates an empty LidarrConfig).
	c := &connection.Connection{
		Name:    body.Name,
		Type:    body.Type,
		URL:     body.URL,
		APIKey:  body.APIKey,
		Enabled: body.Enabled,
	}
	setMediaServerDefaults(c, platformUserID)
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
	// Best-effort: auto-derive Lidarr path mappings when none are set yet.
	r.applyInferredPathMappingsIfEmpty(req.Context(), c)
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
		writeFormError(w, req, http.StatusNotFound, "connection not found")
		return
	}

	var body struct {
		Name                  string `json:"name"`
		Type                  string `json:"type"`
		URL                   string `json:"url"`
		APIKey                string `json:"api_key"`
		Enabled               *bool  `json:"enabled"`
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
	if body.Type != "" && body.Type != existing.Type {
		// A type change invalidates the platform-specific config carried from
		// the old type; clear all sub-configs so Validate re-allocates the one
		// matching the new type (#1686).
		existing.Type = body.Type
		existing.Lidarr, existing.Emby, existing.Jellyfin = nil, nil, nil
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
	// Apply the partial feature-flag update against the matching media
	// sub-config: read current values via the getters, override the fields
	// present in the request, then write them back. No-op for Lidarr.
	imageWrite := existing.GetFeatureImageWrite()
	metadataPush := existing.GetFeatureMetadataPush()
	triggerRefresh := existing.GetFeatureTriggerRefresh()
	if body.FeatureImageWrite != nil {
		imageWrite = *body.FeatureImageWrite
	}
	if body.FeatureMetadataPush != nil {
		metadataPush = *body.FeatureMetadataPush
	}
	if body.FeatureTriggerRefresh != nil {
		triggerRefresh = *body.FeatureTriggerRefresh
	}
	existing.SetFeatures(imageWrite, metadataPush, triggerRefresh)

	if err := r.connectionService.Update(req.Context(), existing); err != nil {
		r.logger.Error("updating connection", "error", err)
		writeFormError(w, req, http.StatusInternalServerError, "internal error")
		return
	}
	// Best-effort: a Lidarr connection updated to enabled with no path mappings
	// yet gets them auto-derived (no-op when disabled or already mapped -- #2329).
	r.applyInferredPathMappingsIfEmpty(req.Context(), existing)
	// HTMX form submission from the settings page edit panel: trigger a full
	// page refresh so the updated connection values appear in the read-only
	// row, matching the handleCreateConnectionSuccess pattern.
	if isHTMXRequest(req) {
		w.Header().Set("HX-Refresh", "true")
		w.Header().Set("HX-Reswap", "none")
		w.WriteHeader(http.StatusNoContent)
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
		for i := range libs {
			lib := &libs[i]
			if deleteArtists {
				if err := r.libraryService.DeleteWithArtists(req.Context(), lib.ID); err != nil {
					r.logger.Error("deleting library with artists", "library_id", lib.ID, "error", err)
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
					return
				}
			} else {
				// Dismiss active violations before the delete NULLs library_id.
				if _, dismissErr := r.ruleService.DismissViolationsForLibrary(req.Context(), lib.ID); dismissErr != nil {
					r.logger.Error("dismissing violations for library removal", "library_id", lib.ID, "error", dismissErr)
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
					return
				}
				if err := r.libraryService.Delete(req.Context(), lib.ID); err != nil {
					r.logger.Error("deleting library", "library_id", lib.ID, "error", err)
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
					return
				}
			}
		}
		// When the last local library is removed, auto-disable filesystem-dependent
		// rules so they do not evaluate against artists without filesystem paths.
		r.maybeDisableFilesystemRules(req.Context())
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

// connectionProbeResult captures the outcome of testing a single platform
// connection: whether the test succeeded, any drift warnings surfaced, and
// (for platforms that support it) the resolved platform user/server IDs to
// persist. HasPlatformUserID/HasServerID distinguish "not applicable
// to this platform" from "resolution failed" (the latter still requests a
// persist call, preserving the original always-persist-on-attempt behavior
// for the user ID and persist-only-on-success behavior for the server ID).
type connectionProbeResult struct {
	TestErr           error
	DriftWarnings     []string
	PlatformName      string
	HasPlatformUserID bool
	PlatformUserID    string
	HasServerID       bool
	PlatformServerID  string
}

// connectionProber abstracts the per-connection-type test/resolve/drift-detect
// sequence so handleTestConnection can dispatch without inlining each
// platform's branch.
type connectionProber interface {
	Probe(ctx context.Context, id string, conn *connection.Connection) *connectionProbeResult
}

// embyProber tests an Emby connection.
type embyProber struct {
	logger *slog.Logger
}

func (p *embyProber) Probe(ctx context.Context, id string, conn *connection.Connection) *connectionProbeResult {
	result := &connectionProbeResult{PlatformName: "emby"}
	client := emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), p.logger)
	result.TestErr = client.TestConnection(ctx)
	if result.TestErr != nil {
		return result
	}

	uid, uidErr := client.GetFirstUserID(ctx)
	if uidErr != nil {
		p.logger.Warn("could not resolve emby platform user id", "error", uidErr)
	}
	result.HasPlatformUserID = true
	result.PlatformUserID = uid

	// Capture the server identity for deep-link URL building. A missing
	// server ID is non-fatal: the link will simply be built without
	// the ?serverId= parameter and the web client will fall back to
	// its default behavior (works for single-server setups).
	if sid, sidErr := client.GetServerID(ctx); sidErr != nil {
		p.logger.Warn("could not resolve emby platform server id", "error", sidErr)
	} else {
		result.HasServerID = true
		result.PlatformServerID = sid
	}

	// Drift detection: check for conflicting platform settings.
	if settings, settingsErr := client.GetLibrarySettings(ctx); settingsErr == nil {
		for _, s := range settings {
			if s.HasConflicts {
				warning := "library " + s.LibraryName + " has active fetchers/savers that may overwrite Stillwater-managed metadata"
				result.DriftWarnings = append(result.DriftWarnings, warning)
				p.logger.Warn("emby platform settings drift detected", "connection_id", id, "library", s.LibraryName)
			}
		}
	} else {
		p.logger.Warn("could not inspect emby platform settings for drift", "connection_id", id, "error", settingsErr)
		result.DriftWarnings = append(result.DriftWarnings, "could not inspect platform settings for drift")
	}
	return result
}

// jellyfinProber tests a Jellyfin connection.
type jellyfinProber struct {
	logger *slog.Logger
}

func (p *jellyfinProber) Probe(ctx context.Context, id string, conn *connection.Connection) *connectionProbeResult {
	result := &connectionProbeResult{PlatformName: "jellyfin"}
	client := jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), p.logger)
	result.TestErr = client.TestConnection(ctx)
	if result.TestErr != nil {
		return result
	}

	uid, uidErr := client.GetFirstUserID(ctx)
	if uidErr != nil {
		p.logger.Warn("could not resolve jellyfin platform user id", "error", uidErr)
	}
	result.HasPlatformUserID = true
	result.PlatformUserID = uid

	// Capture the server identity for deep-link URL building. See
	// the equivalent Emby branch above for the no-id fallback rationale.
	if sid, sidErr := client.GetServerID(ctx); sidErr != nil {
		p.logger.Warn("could not resolve jellyfin platform server id", "error", sidErr)
	} else {
		result.HasServerID = true
		result.PlatformServerID = sid
	}

	// Drift detection: check for conflicting platform settings.
	if settings, settingsErr := client.GetLibrarySettings(ctx); settingsErr == nil {
		for _, s := range settings {
			if s.HasConflicts {
				warning := "library " + s.LibraryName + " has active fetchers that may overwrite Stillwater-managed metadata"
				result.DriftWarnings = append(result.DriftWarnings, warning)
				p.logger.Warn("jellyfin platform settings drift detected", "connection_id", id, "library", s.LibraryName)
			}
		}
	} else {
		p.logger.Warn("could not inspect jellyfin platform settings for drift", "connection_id", id, "error", settingsErr)
		result.DriftWarnings = append(result.DriftWarnings, "could not inspect platform settings for drift")
	}
	return result
}

// lidarrProber tests a Lidarr connection. Lidarr does not resolve a platform
// user/server ID, so those result fields remain unset.
type lidarrProber struct {
	logger *slog.Logger
}

func (p *lidarrProber) Probe(ctx context.Context, id string, conn *connection.Connection) *connectionProbeResult {
	result := &connectionProbeResult{PlatformName: "lidarr"}
	client := lidarr.New(conn.URL, conn.APIKey, p.logger)
	result.TestErr = client.TestConnection(ctx)
	if result.TestErr != nil {
		return result
	}

	// Drift detection: check for enabled metadata consumers.
	if consumers, consumersErr := client.GetMetadataConsumers(ctx); consumersErr == nil {
		for _, c := range consumers {
			if c.Enabled {
				warning := "metadata consumer " + c.ConsumerName + " is enabled and may write NFO files"
				result.DriftWarnings = append(result.DriftWarnings, warning)
				p.logger.Warn("lidarr platform settings drift detected", "connection_id", id, "consumer", c.ConsumerName)
			}
		}
	} else {
		p.logger.Warn("could not inspect lidarr platform settings for drift", "connection_id", id, "error", consumersErr)
		result.DriftWarnings = append(result.DriftWarnings, "could not inspect platform settings for drift")
	}
	return result
}

// newConnectionProber returns the connectionProber for connType, or an error
// if the type is not supported.
func (r *Router) newConnectionProber(connType string) (connectionProber, error) {
	switch connType {
	case connection.TypeEmby:
		return &embyProber{logger: r.logger}, nil
	case connection.TypeJellyfin:
		return &jellyfinProber{logger: r.logger}, nil
	case connection.TypeLidarr:
		return &lidarrProber{logger: r.logger}, nil
	default:
		return nil, errors.New("unsupported connection type: " + connType)
	}
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

	prober, err := r.newConnectionProber(conn.Type)
	if err != nil {
		// Log the underlying prober-selection error server-side but return a
		// controlled generic message so no raw error text reaches the client
		// (matches origin/main byte-for-byte; keeps the raw-error-leak gate clean).
		r.logger.Warn("selecting connection prober", "type", conn.Type, "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported connection type: " + conn.Type})
		return
	}

	testCtx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()

	result := prober.Probe(testCtx, id, conn)

	if result.HasPlatformUserID {
		if updErr := r.connectionService.UpdatePlatformUserID(testCtx, id, result.PlatformUserID); updErr != nil {
			r.logger.Error("persisting "+result.PlatformName+" platform user id", "error", updErr)
		}
	}
	if result.HasServerID {
		if updErr := r.connectionService.UpdatePlatformServerID(testCtx, id, result.PlatformServerID); updErr != nil {
			r.logger.Error("persisting "+result.PlatformName+" platform server id", "error", updErr)
		}
	}

	status := "ok"
	msg := ""
	if result.TestErr != nil {
		status = "error"
		msg = result.TestErr.Error()
	}
	if updateErr := r.connectionService.UpdateStatus(req.Context(), id, status, msg); updateErr != nil {
		r.logger.Error("updating connection status", "error", updateErr)
	}
	// A successful test is a second cheap opportunity to fill in path mappings
	// the create-time attempt could not derive (empty library at the time). No-op
	// when the connection is already mapped or the library still has no MBIDs;
	// the authoritative re-run is the post-scan hook (see
	// applyInferredPathMappingsAllConnections).
	if status == "ok" {
		r.applyInferredPathMappingsIfEmpty(req.Context(), conn)
	}
	resp := map[string]any{"status": status, "message": msg}
	if len(result.DriftWarnings) > 0 {
		resp["drift_warnings"] = result.DriftWarnings
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
		FeatureImageWrite     *bool `json:"feature_image_write"`
		FeatureMetadataPush   *bool `json:"feature_metadata_push"`
		FeatureTriggerRefresh *bool `json:"feature_trigger_refresh"`
	}
	if !DecodeJSON(w, req, &body) {
		return
	}

	imageWrite := existing.GetFeatureImageWrite()
	metadataPush := existing.GetFeatureMetadataPush()
	triggerRefresh := existing.GetFeatureTriggerRefresh()
	if body.FeatureImageWrite != nil {
		imageWrite = *body.FeatureImageWrite
	}
	if body.FeatureMetadataPush != nil {
		metadataPush = *body.FeatureMetadataPush
	}
	if body.FeatureTriggerRefresh != nil {
		triggerRefresh = *body.FeatureTriggerRefresh
	}

	if err := r.connectionService.UpdateFeatures(req.Context(), id, imageWrite, metadataPush, triggerRefresh); err != nil {
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

// pathMappingsRequest is the body of handleSetPathMappings. The full list
// replaces the connection's existing mappings (PUT-like semantics): an empty
// or omitted list clears them, restoring verbatim path propagation.
type pathMappingsRequest struct {
	PathMappings []connection.PathMapping `json:"path_mappings"`
}

// handleSetPathMappings replaces the connection-level host<->platform
// path-mapping list (connection.PathMappings). When set, the rename/merge
// publisher rewrites an artist path's host prefix to the platform prefix before
// the UpdateArtistPath call so a split-mount peer receives a path it can resolve
// instead of one it rejects, silently coerces, or stores as nonsense (#2303 /
// #2380).
//
// POST /api/v1/connections/{id}/path-mappings
// Body: {"path_mappings": [{"host_prefix": "/music", "platform_prefix": "/data"}]}
//
// Valid for EVERY connection type since #2380: an Emby or Jellyfin container
// mounts the library under its own prefix exactly the way Lidarr does, and
// leaving them unmapped pushed host paths into their container namespace. A
// per-connection mutex (pathMappingsMu) serializes the read-modify-write; the
// existence gate runs BEFORE the per-id LoadOrStore so unknown ids never grow
// the mutex map.
func (r *Router) handleSetPathMappings(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing connection id"})
		return
	}

	raw, ok := parsePathMappingsBody(w, req)
	if !ok {
		return
	}

	mappings, err := sanitizePathMappings(raw)
	if err != nil {
		r.logger.Warn("sanitizing path mappings", "connection_id", id, "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid path mappings"})
		return
	}

	// Existence gate BEFORE allocating per-id serialization state, so an unknown
	// id returns without ever touching the mutex map. No type gate: path mappings
	// are valid for every connection type since #2380.
	if _, gerr := r.connectionService.GetByID(req.Context(), id); gerr != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}

	muIface, _ := r.pathMappingsMu.LoadOrStore(id, &sync.Mutex{})
	connMu := muIface.(*sync.Mutex)
	connMu.Lock()
	defer connMu.Unlock()

	existing, err := r.connectionService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}

	setPathMappings(existing, mappings)

	if err := r.connectionService.Update(req.Context(), existing); err != nil {
		r.logger.Error("updating path-mappings failed", "connection_id", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, toConnectionResponse(*existing))
}

// parsePathMappingsBody reads the request mappings from either a JSON body
// ({"path_mappings": [...]}) or an HTMX form post carrying parallel
// host_prefix / platform_prefix fields. The two encodings let API/curl callers
// send structured JSON while the settings form posts plain form-urlencoded
// pairs without any client-side array assembly. On a parse error it writes the
// 400 and returns ok=false. An empty/absent list is valid (it clears mappings).
func parsePathMappingsBody(w http.ResponseWriter, req *http.Request) ([]connection.PathMapping, bool) {
	ct := req.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	if strings.TrimSpace(strings.ToLower(ct)) == "application/json" {
		var body pathMappingsRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, 64*1024))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return nil, false
		}
		return body.PathMappings, true
	}

	// Form-encoded: parallel host_prefix / platform_prefix arrays. Length
	// mismatch is a malformed submission, not a half-mapping, so reject it here
	// rather than letting sanitize pad with empties.
	req.Body = http.MaxBytesReader(w, req.Body, 64*1024)
	if err := req.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid form body"})
		return nil, false
	}
	hosts := req.PostForm["host_prefix"]
	platforms := req.PostForm["platform_prefix"]
	if len(hosts) != len(platforms) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "host_prefix and platform_prefix counts must match"})
		return nil, false
	}
	out := make([]connection.PathMapping, 0, len(hosts))
	for i := range hosts {
		out = append(out, connection.PathMapping{HostPrefix: hosts[i], PlatformPrefix: platforms[i]})
	}
	return out, true
}

// sanitizePathMappings trims whitespace from each pair and drops fully-empty
// entries (so a stray blank row in the UI is ignored). A pair with exactly one
// side filled is rejected: a half-mapping would translate a prefix to "" (or
// from ""), silently corrupting every path it matched. A prefix that reduces to
// "" after MapArtistPath's own TrimRight("/") -- i.e. "" or a bare "/" -- is
// also rejected: MapArtistPath skips a "/" HostPrefix as a silent no-op, and a
// "/" PlatformPrefix would strip the host prefix entirely, so persisting either
// would echo back a saved-looking mapping that never behaves as configured. The
// returned slice is nil when no valid mappings remain, which clears the column.
func sanitizePathMappings(in []connection.PathMapping) ([]connection.PathMapping, error) {
	var out []connection.PathMapping
	for _, m := range in {
		host := strings.TrimSpace(m.HostPrefix)
		platform := strings.TrimSpace(m.PlatformPrefix)
		if host == "" && platform == "" {
			continue
		}
		if host == "" || platform == "" {
			return nil, errors.New("each path mapping needs both a host prefix and a platform prefix")
		}
		// Match MapArtistPath's TrimRight("/") so a prefix that is only
		// slashes ("/" or "//") is rejected here rather than silently ignored
		// at map time.
		if strings.TrimRight(host, "/") == "" || strings.TrimRight(platform, "/") == "" {
			return nil, errors.New("path mapping prefixes cannot be empty or the filesystem root")
		}
		out = append(out, connection.PathMapping{HostPrefix: host, PlatformPrefix: platform})
	}
	return out, nil
}

// handleInferPathMappings re-runs path-mapping inference for a connection and,
// when the connection currently has no mappings, applies the derived set (never
// overwriting an existing list -- #2329 B3). It returns the refreshed
// path-mapping card fragment with a read-only info line reporting the outcome,
// so the settings UI updates in place.
//
// POST /api/v1/connections/{id}/path-mappings/infer
//
// Valid for EVERY connection type since #2380 (Emby and Jellyfin need the same
// host->container translation Lidarr does). The existence gate runs BEFORE the
// per-id LoadOrStore so an unknown id never grows the mutex map, and the shared
// pathMappingsMu serializes the read-modify-write so a concurrent manual save
// and an infer cannot interleave.
func (r *Router) handleInferPathMappings(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing connection id"})
		return
	}

	// Existence + type gate. The resolved connection also drives the inference
	// enumeration below, which runs BEFORE the per-id lock: the ~10s Lidarr
	// GetArtists round-trip must not be held under pathMappingsMu, or it would
	// block every other path-mapping write on this connection for its full
	// duration (mirrors applyInferredPathMappingsIfEmpty, which enumerates
	// unlocked and locks only the fast re-check-and-persist).
	gate, gerr := r.connectionService.GetByID(req.Context(), id)
	if gerr != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}

	// Compute the inference outcome UNLOCKED. It reports the matched count and
	// derived-mapping count for the info line even when nothing is applied (the
	// list was already populated or the consensus floor emitted zero).
	mappings, matched, inferErr := r.inferPathMappings(req.Context(), gate)
	if inferErr != nil {
		r.logger.Info("path-mapping inference (manual) skipped", "connection_id", id, "error", inferErr)
		mappings, matched = nil, 0
	}

	// Now take the per-id lock ONLY for the fast re-check-and-persist, reusing
	// the precomputed mappings. Serializing just this window keeps a concurrent
	// manual save from being clobbered while never blocking it for the round-trip.
	muIface, _ := r.pathMappingsMu.LoadOrStore(id, &sync.Mutex{})
	connMu := muIface.(*sync.Mutex)
	connMu.Lock()
	defer connMu.Unlock()

	existing, err := r.connectionService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}

	// Apply only when the list is (still) empty under the lock (B3 precedence).
	// Track whether we actually wrote so the info line can distinguish
	// "applied N" from "inferred N but kept your existing mappings" (a non-empty
	// list is never overwritten -- a save that landed during the enumeration is
	// seen here and wins).
	applied := false
	if len(existing.GetPathMappings()) == 0 && len(mappings) > 0 {
		setPathMappings(existing, mappings)
		if updErr := r.connectionService.Update(req.Context(), existing); updErr != nil {
			r.logger.Error("persisting inferred path mappings (manual)", "connection_id", id, "error", updErr)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		applied = true
	}

	renderTempl(w, req, templates.ConnectionPathMappingsBlock(*existing, templates.PathInferResult{
		Show:     true,
		Inferred: len(mappings),
		Matched:  matched,
		Applied:  applied,
	}))
}

// setPathMappings replaces the connection-level path-mapping list on c. Since
// #2380 the field lives on Connection (not the Lidarr sub-config), so this is a
// thin pass-through kept for call-site readability and test reuse.
func setPathMappings(c *connection.Connection, mappings []connection.PathMapping) {
	c.SetPathMappings(mappings)
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
		client := emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), r.logger)
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
		client := jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), r.logger)
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
		client := emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), r.logger)
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
		client := jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), r.logger)
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
		client := emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), r.logger)
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
		client := jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), r.logger)
		settings, settingsErr := client.GetLibrarySettings(summaryCtx)
		if settingsErr != nil {
			r.logger.Error("reading jellyfin settings for summary", "connection_id", id, "error", settingsErr)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not read platform settings"})
			return
		}
		total := len(settings)
		managed := 0
		// needs_lockdata was hardcoded true, on the claim that Jellyfin ignores
		// MetadataSavers=[] so lockdata injection was the only NFO protection.
		// That claim is false (#2420): clearing the saver list does stop the
		// writes. Lockdata is only still needed where a saver remains ARMED, so
		// derive it per library instead of asserting it unconditionally --
		// telling an operator to work around a problem we have already fixed is
		// its own kind of lie.
		needsLockdata := false
		for _, s := range settings {
			if !s.HasConflicts {
				managed++
			}
			if s.NeedsLockdata {
				needsLockdata = true
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"total_libraries":   total,
			"managed_libraries": managed,
			"has_conflicts":     managed < total,
			"needs_lockdata":    needsLockdata,
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
