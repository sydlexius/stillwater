package api

import (
	"encoding/json"
	"net/http"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/web/templates"
)

func (r *Router) handleListPlatforms(w http.ResponseWriter, req *http.Request) {
	profiles, err := r.platformService.List(req.Context())
	if err != nil {
		r.logger.Error("listing platforms", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, profiles)
}

func (r *Router) handleGetPlatform(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	profile, err := r.platformService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "platform profile not found"})
		return
	}
	writeJSON(w, http.StatusOK, profile)
}

func (r *Router) handleCreatePlatform(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Name        string               `json:"name"`
		NFOEnabled  bool                 `json:"nfo_enabled"`
		NFOFormat   string               `json:"nfo_format"`
		ImageNaming platform.ImageNaming `json:"image_naming"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if body.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	if body.NFOFormat == "" {
		body.NFOFormat = "kodi"
	}

	p := &platform.Profile{
		Name:        body.Name,
		NFOEnabled:  body.NFOEnabled,
		NFOFormat:   body.NFOFormat,
		ImageNaming: body.ImageNaming,
	}
	if err := r.platformService.Create(req.Context(), p); err != nil {
		r.logger.Error("creating platform", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (r *Router) handleUpdatePlatform(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	existing, err := r.platformService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "platform profile not found"})
		return
	}

	var body struct {
		Name        string               `json:"name"`
		NFOEnabled  bool                 `json:"nfo_enabled"`
		NFOFormat   string               `json:"nfo_format"`
		ImageNaming platform.ImageNaming `json:"image_naming"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if body.Name != "" {
		existing.Name = body.Name
	}
	existing.NFOEnabled = body.NFOEnabled
	if body.NFOFormat != "" {
		existing.NFOFormat = body.NFOFormat
	}
	existing.ImageNaming = body.ImageNaming

	if err := r.platformService.Update(req.Context(), existing); err != nil {
		r.logger.Error("updating platform", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, existing)
}

func (r *Router) handleDeletePlatform(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if err := r.platformService.Delete(req.Context(), id); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (r *Router) handleSetActivePlatform(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if err := r.platformService.SetActive(req.Context(), id); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "active"})
}

// handleSettingsPage renders the settings HTML page.
// GET /settings
func (r *Router) handleSettingsPage(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		renderTempl(w, req, templates.LoginPage(r.assets()))
		return
	}

	profiles, err := r.platformService.List(req.Context())
	if err != nil {
		r.logger.Error("listing platforms for settings page", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	active, err := r.platformService.GetActive(req.Context())
	if err != nil {
		r.logger.Error("getting active platform for settings page", "error", err)
	}

	providerKeys, err := r.providerSettings.ListProviderKeyStatuses(req.Context())
	if err != nil {
		r.logger.Error("listing provider key statuses for settings page", "error", err)
	}

	priorities, err := r.providerSettings.GetPriorities(req.Context())
	if err != nil {
		r.logger.Error("getting provider priorities for settings page", "error", err)
	}

	conns, err := r.connectionService.List(req.Context())
	if err != nil {
		r.logger.Error("listing connections for settings page", "error", err)
	}

	webhooks, err := r.webhookService.List(req.Context())
	if err != nil {
		r.logger.Error("listing webhooks for settings page", "error", err)
	}

	webSearchProviders, err := r.providerSettings.ListWebSearchStatuses(req.Context())
	if err != nil {
		r.logger.Error("listing web search statuses for settings page", "error", err)
	}

	rules, err := r.ruleService.List(req.Context())
	if err != nil {
		r.logger.Warn("fetching rules for settings page", "error", err)
	}

	tab := req.URL.Query().Get("tab")
	switch tab {
	case "general", "providers", "connections", "notifications", "rules", "maintenance":
		// Valid tab.
	default:
		tab = "general"
	}

	data := templates.SettingsData{
		ActiveTab:          tab,
		Profiles:           profiles,
		ActiveProfile:      active,
		ProviderKeys:       providerKeys,
		Priorities:         priorities,
		Connections:        conns,
		Webhooks:           webhooks,
		WebSearchProviders: webSearchProviders,
		AutoFetchImages:    r.getBoolSetting(req.Context(), "auto_fetch_images", false),
		Rules:              rules,
	}
	renderTempl(w, req, templates.SettingsPage(r.assets(), data))
}
