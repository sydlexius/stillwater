package api

import (
	"encoding/json"
	"net/http"

	"github.com/sydlexius/stillwater/internal/scraper"
)

// handleGetScraperConfig returns the effective scraper configuration for the global scope.
func (r *Router) handleGetScraperConfig(w http.ResponseWriter, req *http.Request) {
	cfg, err := r.scraperService.GetConfig(req.Context(), scraper.ScopeGlobal)
	if err != nil {
		r.logger.Error("getting scraper config", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to load scraper config")
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// handleUpdateScraperConfig updates the global scraper configuration.
func (r *Router) handleUpdateScraperConfig(w http.ResponseWriter, req *http.Request) {
	var cfg scraper.ScraperConfig
	if err := json.NewDecoder(req.Body).Decode(&cfg); err != nil {
		writeError(w, req, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := scraper.ValidateConfig(&cfg); err != nil {
		writeError(w, req, http.StatusBadRequest, err.Error())
		return
	}

	if err := r.scraperService.SaveConfig(req.Context(), scraper.ScopeGlobal, &cfg, nil); err != nil {
		r.logger.Error("saving scraper config", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to save scraper config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// handleGetConnectionScraperConfig returns the merged scraper config for a connection scope.
func (r *Router) handleGetConnectionScraperConfig(w http.ResponseWriter, req *http.Request) {
	connID := req.PathValue("id")

	// Validate connection exists
	if _, err := r.connectionService.GetByID(req.Context(), connID); err != nil {
		writeError(w, req, http.StatusNotFound, "connection not found")
		return
	}

	cfg, err := r.scraperService.GetConfig(req.Context(), connID)
	if err != nil {
		r.logger.Error("getting connection scraper config", "connection", connID, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to load scraper config")
		return
	}

	// Also return the raw config so the UI can distinguish inherited vs overridden
	raw, overrides, err2 := r.scraperService.GetRawConfig(req.Context(), connID)
	if err2 != nil {
		r.logger.Error("getting raw scraper config", "connection", connID, "error", err2)
		writeError(w, req, http.StatusInternalServerError, "failed to load raw scraper config")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"config":    cfg,
		"raw":       raw,
		"overrides": overrides,
	})
}

// handleUpdateConnectionScraperConfig updates the scraper config overrides for a connection.
func (r *Router) handleUpdateConnectionScraperConfig(w http.ResponseWriter, req *http.Request) {
	connID := req.PathValue("id")

	// Validate connection exists
	if _, err := r.connectionService.GetByID(req.Context(), connID); err != nil {
		writeError(w, req, http.StatusNotFound, "connection not found")
		return
	}

	var body struct {
		Config    scraper.ScraperConfig `json:"config"`
		Overrides *scraper.Overrides    `json:"overrides"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeError(w, req, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := scraper.ValidateConfig(&body.Config); err != nil {
		writeError(w, req, http.StatusBadRequest, err.Error())
		return
	}

	if err := r.scraperService.SaveConfig(req.Context(), connID, &body.Config, body.Overrides); err != nil {
		r.logger.Error("saving connection scraper config", "connection", connID, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to save scraper config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// handleResetConnectionScraperConfig deletes the connection's scraper overrides,
// reverting it to inherit from the global config.
func (r *Router) handleResetConnectionScraperConfig(w http.ResponseWriter, req *http.Request) {
	connID := req.PathValue("id")

	// Validate connection exists
	if _, err := r.connectionService.GetByID(req.Context(), connID); err != nil {
		writeError(w, req, http.StatusNotFound, "connection not found")
		return
	}

	if err := r.scraperService.ResetConfig(req.Context(), connID); err != nil {
		r.logger.Error("resetting connection scraper config", "connection", connID, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to reset scraper config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
}

// handleListScraperProviders returns all providers with their field capabilities
// and current API key status.
func (r *Router) handleListScraperProviders(w http.ResponseWriter, req *http.Request) {
	caps := scraper.ProviderCapabilities()

	// Enrich with current API key status
	for i := range caps {
		hasKey, err := r.providerSettings.HasAPIKey(req.Context(), caps[i].Provider)
		if err != nil {
			r.logger.Error("checking provider key", "provider", caps[i].Provider, "error", err)
			continue
		}
		caps[i].HasKey = hasKey
	}

	writeJSON(w, http.StatusOK, map[string]any{"providers": caps})
}
