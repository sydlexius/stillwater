package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/sydlexius/stillwater/internal/updater"
)

// handlePostUpdateCheck queries GitHub for the latest release on the configured
// channel. POST because the call mutates server-side updater state (last_checked
// timestamp, cached latest/release_url/update_available fields) and is therefore
// CSRF-protected per the usual unsafe-method middleware path.
// POST /api/v1/updates/check
func (r *Router) handlePostUpdateCheck(w http.ResponseWriter, req *http.Request) {
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

	// Honor the Enabled kill switch: when the operator has explicitly
	// disabled the updater, manual Apply is rejected too. The schema
	// description for `enabled` advertises this contract and the UI Apply
	// button is disabled when Enabled=false; this is the server-side
	// enforcement so a direct API call cannot bypass the toggle.
	//
	// Fail closed on a config-read error: the previous form ("err == nil
	// && !cfg.Enabled") fell through on read failure and applied the
	// update anyway, defeating the kill switch whenever the settings
	// query erred for any reason. Surface a 500 instead so the operator
	// sees the read failure and the update does not slip past the gate.
	cfg, err := r.updaterService.GetConfig(req.Context())
	if err != nil {
		r.logger.Error("reading updater config before apply", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "failed to read updater configuration",
		})
		return
	}
	if !cfg.Enabled {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "updater is disabled; enable it under Settings > Updates before applying",
		})
		return
	}

	// Detach from the request context so the async goroutine is not canceled
	// when the HTTP response is sent and the request context is canceled.
	if err := r.updaterService.Apply(context.WithoutCancel(req.Context())); err != nil {
		if errors.Is(err, updater.ErrAlreadyRunning) {
			r.logger.Warn("update apply rejected", "error", err)
			writeJSON(w, http.StatusConflict, map[string]string{"error": "update already in progress"})
			return
		}
		if errors.Is(err, updater.ErrRestartRequired) {
			r.logger.Warn("update apply rejected", "error", err)
			writeJSON(w, http.StatusConflict, map[string]string{"error": "restart required before applying another update"})
			return
		}
		r.logger.Error("update apply failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "update apply failed"})
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

	// Decode into a pointer-fielded shadow so a missing `enabled` field can be
	// distinguished from an explicit `false`. The OpenAPI schema documents the
	// kill switch as defaulting to true on PUT (so a client that PUTs only
	// `channel` does not silently turn updates off); decoding directly into
	// updater.Config would assign Go's zero value and quietly flip Enabled to
	// false on every partial write.
	var raw struct {
		Channel            updater.Channel `json:"channel"`
		Enabled            *bool           `json:"enabled,omitempty"`
		AutoCheck          *bool           `json:"auto_check,omitempty"`
		CheckIntervalHours int             `json:"check_interval_hours"`
	}
	if err := json.NewDecoder(req.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	body := updater.Config{
		Channel:            raw.Channel,
		Enabled:            true,
		AutoCheck:          false,
		CheckIntervalHours: raw.CheckIntervalHours,
	}
	if raw.Enabled != nil {
		body.Enabled = *raw.Enabled
	}
	if raw.AutoCheck != nil {
		body.AutoCheck = *raw.AutoCheck
	}

	// Validate channel value before persisting.
	if body.Channel != updater.ChannelStable &&
		body.Channel != updater.ChannelPrerelease &&
		body.Channel != updater.ChannelNightly {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "channel must be \"stable\", \"prerelease\", or \"nightly\"",
		})
		return
	}

	// Validate check interval. Zero is coerced to the default at the service
	// layer (matches GetConfig defaulting) so API clients that omit the field
	// receive sane behavior; an explicit negative value is rejected here so
	// the UI gets an actionable 400 rather than a 500 from the service layer.
	if body.CheckIntervalHours < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "check_interval_hours must be at least 1",
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
