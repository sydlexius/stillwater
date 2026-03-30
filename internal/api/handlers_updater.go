package api

import (
	"encoding/json"
	"net/http"
	"runtime"
	"strconv"
	"strings"

	"github.com/sydlexius/stillwater/internal/updater"
	"github.com/sydlexius/stillwater/internal/version"
)

// handleUpdateStatus returns the current version, cached update availability,
// and whether the process is running inside a container.
//
// GET /api/v1/updates/status
func (r *Router) handleUpdateStatus(w http.ResponseWriter, req *http.Request) {
	inContainer := updater.InContainer()

	type statusResponse struct {
		CurrentVersion string              `json:"current_version"`
		Commit         string              `json:"commit"`
		InContainer    bool                `json:"in_container"`
		UpdaterEnabled bool                `json:"updater_enabled"`
		Channel        updater.Channel     `json:"channel"`
		CachedUpdate   *updater.UpdateInfo `json:"cached_update,omitempty"`
		CheckedAt      string              `json:"checked_at,omitempty"`
	}

	resp := statusResponse{
		CurrentVersion: version.Version,
		Commit:         version.Commit,
		InContainer:    inContainer,
		UpdaterEnabled: r.updateChecker != nil,
	}

	if r.updateChecker != nil {
		resp.Channel = r.updateChecker.Channel()
		cached, checkedAt := r.updateChecker.CachedResult()
		resp.CachedUpdate = cached
		if !checkedAt.IsZero() {
			resp.CheckedAt = checkedAt.Format("2006-01-02T15:04:05Z")
		}
	} else {
		resp.Channel = updater.Channel(
			r.getStringSetting(req.Context(), "updater.channel", string(updater.ChannelLatest)),
		)
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleCheckUpdate triggers an immediate update check against GitHub Releases
// and returns the result.
//
// GET /api/v1/updates/check
func (r *Router) handleCheckUpdate(w http.ResponseWriter, req *http.Request) {
	if r.updateChecker == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]string{"error": "update checker is disabled"})
		return
	}

	info, err := r.updateChecker.Check(req.Context())
	if err != nil {
		r.logger.Error("update check failed", "error", err)
		writeJSON(w, http.StatusBadGateway,
			map[string]string{"error": "update check failed"})
		return
	}

	writeJSON(w, http.StatusOK, info)
}

// handleApplyUpdate downloads and installs the latest available update.
// Returns 409 Conflict when running inside a container (updates are managed by
// the container runtime).  Returns 404 when no update is available.
//
// POST /api/v1/updates/apply
func (r *Router) handleApplyUpdate(w http.ResponseWriter, req *http.Request) {
	if updater.InContainer() {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "binary updates are not supported in container environments; pull the latest image to update",
		})
		return
	}

	if r.updateChecker == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]string{"error": "update checker is disabled"})
		return
	}

	// Use cached result if available, otherwise run a fresh check.
	cached, _ := r.updateChecker.CachedResult()
	if cached == nil || !cached.Available {
		info, err := r.updateChecker.Check(req.Context())
		if err != nil {
			r.logger.Error("update check before apply failed", "error", err)
			writeJSON(w, http.StatusBadGateway,
				map[string]string{"error": "update check failed"})
			return
		}
		cached = info
	}

	if !cached.Available {
		writeJSON(w, http.StatusNotFound,
			map[string]string{"error": "no update available"})
		return
	}

	asset := selectAsset(cached.Assets)
	if asset == nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "no suitable asset found for the current platform",
		})
		return
	}

	inst := updater.NewInstaller(nil, r.logger)
	if err := inst.Install(req.Context(), *asset); err != nil {
		r.logger.Error("update install failed", "error", err, "asset", asset.Name)
		writeJSON(w, http.StatusInternalServerError,
			map[string]string{"error": "install failed; the previous binary has been restored"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "installed",
		"version": cached.Latest,
	})
}

// handleGetUpdateConfig returns the current updater configuration from the DB
// settings table.
//
// GET /api/v1/updates/config
func (r *Router) handleGetUpdateConfig(w http.ResponseWriter, req *http.Request) {
	type configResponse struct {
		Enabled       bool            `json:"enabled"`
		Channel       updater.Channel `json:"channel"`
		AutoUpdate    bool            `json:"auto_update"`
		CheckInterval int             `json:"check_interval_hours"`
	}

	resp := configResponse{
		Enabled:       r.getBoolSetting(req.Context(), "updater.enabled", false),
		Channel:       updater.Channel(r.getStringSetting(req.Context(), "updater.channel", string(updater.ChannelLatest))),
		AutoUpdate:    r.getBoolSetting(req.Context(), "updater.auto_update", false),
		CheckInterval: r.getIntSetting(req.Context(), "updater.check_interval_hours", 24),
	}

	writeJSON(w, http.StatusOK, resp)
}

// handlePutUpdateConfig updates updater settings in the DB settings table.
//
// PUT /api/v1/updates/config
func (r *Router) handlePutUpdateConfig(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Enabled       *bool   `json:"enabled"`
		Channel       *string `json:"channel"`
		AutoUpdate    *bool   `json:"auto_update"`
		CheckInterval *int    `json:"check_interval_hours"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	settings := make(map[string]string)

	if body.Enabled != nil {
		v := "false"
		if *body.Enabled {
			v = "true"
		}
		settings["updater.enabled"] = v
	}

	if body.Channel != nil {
		ch := updater.Channel(*body.Channel)
		switch ch {
		case updater.ChannelLatest, updater.ChannelBeta, updater.ChannelDev:
			// valid
		default:
			writeJSON(w, http.StatusBadRequest,
				map[string]string{"error": "channel must be one of: latest, beta, dev"})
			return
		}
		settings["updater.channel"] = string(ch)
		if r.updateChecker != nil {
			r.updateChecker.SetChannel(ch)
		}
	}

	if body.AutoUpdate != nil {
		v := "false"
		if *body.AutoUpdate {
			v = "true"
		}
		settings["updater.auto_update"] = v
	}

	if body.CheckInterval != nil {
		if *body.CheckInterval < 1 {
			writeJSON(w, http.StatusBadRequest,
				map[string]string{"error": "check_interval_hours must be at least 1"})
			return
		}
		settings["updater.check_interval_hours"] = strconv.Itoa(*body.CheckInterval)
	}

	if len(settings) == 0 {
		writeJSON(w, http.StatusBadRequest,
			map[string]string{"error": "no valid fields provided"})
		return
	}

	if err := r.upsertSettings(req.Context(), settings); err != nil {
		r.logger.Error("persisting updater config", "error", err)
		writeJSON(w, http.StatusInternalServerError,
			map[string]string{"error": "failed to save settings"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// selectAsset picks the most appropriate binary asset from a release.
// It prefers assets whose name contains both the OS and architecture of the
// running process, then falls back to the first available asset.
func selectAsset(assets []updater.AssetInfo) *updater.AssetInfo {
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	for i := range assets {
		name := strings.ToLower(assets[i].Name)
		if strings.Contains(name, goos) && strings.Contains(name, goarch) {
			return &assets[i]
		}
	}
	for i := range assets {
		if assets[i].DownloadURL != "" {
			return &assets[i]
		}
	}
	return nil
}

