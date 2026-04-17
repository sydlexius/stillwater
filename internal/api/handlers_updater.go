package api

import (
	"encoding/json"
	"net/http"

	"github.com/sydlexius/stillwater/internal/updater"
)

// handleGetUpdateCheck queries GitHub for the latest release on the configured channel.
// GET /api/v1/updates/check
func (r *Router) handleGetUpdateCheck(w http.ResponseWriter, req *http.Request) {
	if r.updaterService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "updater service not available"})
		return
	}

	result, err := r.updaterService.Check(req.Context())
	if err != nil {
		r.logger.Error("update check failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "update check failed; see server logs for details"})
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// handleGetUpdateStatus returns the current state of the update lifecycle.
// GET /api/v1/updates/status
func (r *Router) handleGetUpdateStatus(w http.ResponseWriter, req *http.Request) {
	if r.updaterService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "updater service not available"})
		return
	}

	status := r.updaterService.Status()
	writeJSON(w, http.StatusOK, status)
}

// handlePostUpdateApply initiates an async binary update.
// Returns 409 when an update is already in progress, or 422 when running in Docker.
// POST /api/v1/updates/apply
func (r *Router) handlePostUpdateApply(w http.ResponseWriter, req *http.Request) {
	if r.updaterService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "updater service not available"})
		return
	}

	if r.updaterService.IsDocker() {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "binary update is not supported in Docker environments; re-pull the container image to upgrade",
		})
		return
	}

	if err := r.updaterService.Apply(req.Context()); err != nil {
		r.logger.Warn("update apply rejected", "error", err)
		writeJSON(w, http.StatusConflict, map[string]string{"error": "update already in progress"})
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "update started"})
}

// handleGetUpdateConfig returns the current updater configuration.
// GET /api/v1/updates/config
func (r *Router) handleGetUpdateConfig(w http.ResponseWriter, req *http.Request) {
	if r.updaterService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "updater service not available"})
		return
	}

	cfg, err := r.updaterService.GetConfig(req.Context())
	if err != nil {
		r.logger.Error("getting updater config", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, cfg)
}

// handlePutUpdateConfig saves the updater configuration.
// PUT /api/v1/updates/config
func (r *Router) handlePutUpdateConfig(w http.ResponseWriter, req *http.Request) {
	if r.updaterService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "updater service not available"})
		return
	}

	var body updater.Config
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	// Validate channel value before persisting.
	if body.Channel != updater.ChannelStable && body.Channel != updater.ChannelPrerelease {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "channel must be \"stable\" or \"prerelease\"",
		})
		return
	}

	if err := r.updaterService.SetConfig(req.Context(), body); err != nil {
		r.logger.Error("saving updater config", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	cfg, err := r.updaterService.GetConfig(req.Context())
	if err != nil {
		r.logger.Error("re-reading updater config after save", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, cfg)
}
