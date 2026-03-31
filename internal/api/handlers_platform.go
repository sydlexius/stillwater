package api

import (
	"net/http"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/filesystem"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/web/templates"
)

// handleListPlatforms returns all platform naming profiles.
// GET /api/v1/platforms
func (r *Router) handleListPlatforms(w http.ResponseWriter, req *http.Request) {
	profiles, err := r.platformService.List(req.Context())
	if err != nil {
		r.logger.Error("listing platforms", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, profiles)
}

// handleGetPlatform returns a single platform profile by ID.
// GET /api/v1/platforms/{id}
func (r *Router) handleGetPlatform(w http.ResponseWriter, req *http.Request) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	profile, err := r.platformService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "platform profile not found"})
		return
	}
	writeJSON(w, http.StatusOK, profile)
}

// handleCreatePlatform creates a new platform naming profile.
// POST /api/v1/platforms
func (r *Router) handleCreatePlatform(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Name        string               `json:"name"`
		NFOEnabled  bool                 `json:"nfo_enabled"`
		NFOFormat   string               `json:"nfo_format"`
		ImageNaming platform.ImageNaming `json:"image_naming"`
		UseSymlinks bool                 `json:"use_symlinks"`
	}
	if !DecodeJSON(w, req, &body) {
		return
	}
	if body.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	if body.NFOFormat == "" {
		body.NFOFormat = "kodi"
	}

	if errs := platform.ValidateImageNaming(body.ImageNaming); errs != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid image naming", "details": errs})
		return
	}

	p := &platform.Profile{
		Name:        body.Name,
		NFOEnabled:  body.NFOEnabled,
		NFOFormat:   body.NFOFormat,
		ImageNaming: body.ImageNaming,
		UseSymlinks: body.UseSymlinks,
	}
	if err := r.platformService.Create(req.Context(), p); err != nil {
		r.logger.Error("creating platform", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// handleUpdatePlatform partially updates an existing platform profile.
// PUT /api/v1/platforms/{id}
func (r *Router) handleUpdatePlatform(w http.ResponseWriter, req *http.Request) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	existing, err := r.platformService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "platform profile not found"})
		return
	}

	var body struct {
		Name        *string               `json:"name"`
		NFOEnabled  *bool                 `json:"nfo_enabled"`
		NFOFormat   *string               `json:"nfo_format"`
		ImageNaming *platform.ImageNaming `json:"image_naming"`
		UseSymlinks *bool                 `json:"use_symlinks"`
	}
	if !DecodeJSON(w, req, &body) {
		return
	}

	if body.ImageNaming != nil {
		if errs := platform.ValidateImageNaming(*body.ImageNaming); errs != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid image naming", "details": errs})
			return
		}
	}

	// Treat empty strings as "not provided" so they don't trigger
	// the builtin guard or overwrite with blank values.
	if body.Name != nil && *body.Name == "" {
		body.Name = nil
	}
	if body.NFOFormat != nil && *body.NFOFormat == "" {
		body.NFOFormat = nil
	}

	// Prevent renaming built-in profiles.
	if existing.IsBuiltin && body.Name != nil && *body.Name != existing.Name {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot rename a built-in profile"})
		return
	}

	// Prevent symlink changes on non-editable profiles.
	if body.UseSymlinks != nil && !existing.Editable() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot change symlink setting on a built-in profile"})
		return
	}

	if body.Name != nil {
		existing.Name = *body.Name
	}
	if body.NFOEnabled != nil {
		existing.NFOEnabled = *body.NFOEnabled
	}
	if body.NFOFormat != nil {
		existing.NFOFormat = *body.NFOFormat
	}
	if body.ImageNaming != nil {
		existing.ImageNaming = *body.ImageNaming
	}
	if body.UseSymlinks != nil {
		existing.UseSymlinks = *body.UseSymlinks
	}

	if err := r.platformService.Update(req.Context(), existing); err != nil {
		r.logger.Error("updating platform", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, existing)
}

// handleDeletePlatform removes a platform profile by ID.
// DELETE /api/v1/platforms/{id}
func (r *Router) handleDeletePlatform(w http.ResponseWriter, req *http.Request) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	if err := r.platformService.Delete(req.Context(), id); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleSetActivePlatform sets a platform profile as the active one.
// POST /api/v1/platforms/{id}/activate
func (r *Router) handleSetActivePlatform(w http.ResponseWriter, req *http.Request) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	if err := r.platformService.SetActive(req.Context(), id); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "active"})
}

// normalizeSettingsSection maps a raw section string to a valid settings tab
// name. Unknown values fall back to "general". This keeps the validation logic
// in one place so handleSettingsPage and handleSettingsSectionPage stay in sync.
func normalizeSettingsSection(section string) string {
	switch section {
	case "general", "appearance", "providers", "connections", "libraries", "automation", "rules",
		"users", "authentication", "maintenance", "logs":
		return section
	default:
		return "general"
	}
}

// handleSettingsPage renders the settings HTML page.
// GET /settings
func (r *Router) handleSettingsPage(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		r.renderLoginPage(w, req)
		return
	}

	// Settings page is admin-only. Operators get a 403 page instead of
	// raw JSON so the browser renders something meaningful.
	if middleware.RoleFromContext(req.Context()) != "administrator" {
		http.Error(w, "Forbidden: administrator role required", http.StatusForbidden)
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

	apiTokens, err := r.authService.ListAPITokens(req.Context(), userID)
	if err != nil {
		r.logger.Warn("listing api tokens for settings page", "error", err)
	}

	var libs []library.Library
	if r.libraryService != nil {
		libs, err = r.libraryService.List(req.Context())
		if err != nil {
			r.logger.Error("listing libraries for settings page", "error", err)
		}
		r.populateFSNotifySupported(libs)
	}

	// Probe symlink support against the first library path with a filesystem,
	// and check whether any local library (with a filesystem path) exists.
	symlinkSupported := false
	hasLocalLibrary := false
	for _, lib := range libs {
		if lib.Path != "" {
			if !hasLocalLibrary {
				hasLocalLibrary = true
			}
			if !symlinkSupported {
				symlinkSupported = filesystem.ProbeSymlinkSupport(lib.Path)
			}
		}
	}

	tab := normalizeSettingsSection(req.URL.Query().Get("tab"))

	multiUserEnabled := r.getBoolSetting(req.Context(), "multi_user.enabled", false)

	// Populate users and invites only when the users tab is active to avoid
	// unnecessary DB queries on every settings page load.
	var usersTabData templates.UsersTabData
	usersTabData.MultiUserEnabled = multiUserEnabled
	if multiUserEnabled && tab == "users" {
		if users, err := r.authService.ListUsers(req.Context()); err == nil {
			usersTabData.Users = users
		} else {
			r.logger.Warn("listing users for settings page", "error", err)
			usersTabData.LoadError = "Failed to load users. Please refresh the page."
		}
		if invites, err := r.authService.ListPendingInvites(req.Context()); err == nil {
			usersTabData.Invites = invites
		} else {
			r.logger.Warn("listing invites for settings page", "error", err)
			if usersTabData.LoadError == "" {
				usersTabData.LoadError = "Failed to load invites. Please refresh the page."
			}
		}
	}

	authProvidersData := templates.AuthProvidersData{
		LocalEnabled:          r.getBoolSetting(req.Context(), "auth.providers.local.enabled", true),
		EmbyEnabled:           r.getBoolSetting(req.Context(), "auth.providers.emby.enabled", false),
		EmbyAutoProvision:     r.getBoolSetting(req.Context(), "auth.providers.emby.auto_provision", false),
		EmbyGuardRail:         r.getStringSetting(req.Context(), "auth.providers.emby.guard_rail", "admin"),
		EmbyDefaultRole:       r.getStringSetting(req.Context(), "auth.providers.emby.default_role", "operator"),
		EmbyServerURL:         r.getStringSetting(req.Context(), "auth.providers.emby.server_url", r.getStringSetting(req.Context(), "auth.server_url", "")),
		JellyfinEnabled:       r.getBoolSetting(req.Context(), "auth.providers.jellyfin.enabled", false),
		JellyfinAutoProvision: r.getBoolSetting(req.Context(), "auth.providers.jellyfin.auto_provision", false),
		JellyfinGuardRail:     r.getStringSetting(req.Context(), "auth.providers.jellyfin.guard_rail", "admin"),
		JellyfinDefaultRole:   r.getStringSetting(req.Context(), "auth.providers.jellyfin.default_role", "operator"),
		JellyfinServerURL:     r.getStringSetting(req.Context(), "auth.providers.jellyfin.server_url", r.getStringSetting(req.Context(), "auth.server_url", "")),
		OIDCEnabled:           r.getBoolSetting(req.Context(), "auth.providers.oidc.enabled", false),
		OIDCIssuerURL:         r.getStringSetting(req.Context(), "auth.providers.oidc.issuer_url", ""),
		OIDCClientID:          r.getStringSetting(req.Context(), "auth.providers.oidc.client_id", ""),
		OIDCAutoProvision:     r.getBoolSetting(req.Context(), "auth.providers.oidc.auto_provision", false),
		OIDCAdminGroups:       r.getStringSetting(req.Context(), "auth.providers.oidc.admin_groups", ""),
		OIDCUserGroups:        r.getStringSetting(req.Context(), "auth.providers.oidc.user_groups", ""),
		OIDCDefaultRole:       r.getStringSetting(req.Context(), "auth.providers.oidc.default_role", "operator"),
	}

	data := templates.SettingsData{
		ActiveTab:               tab,
		Libraries:               libs,
		Profiles:                profiles,
		ActiveProfile:           active,
		ProviderKeys:            providerKeys,
		Priorities:              priorities,
		Connections:             conns,
		Webhooks:                webhooks,
		WebSearchProviders:      webSearchProviders,
		AutoFetchImages:         r.getBoolSetting(req.Context(), "auto_fetch_images", false),
		SymlinkSupported:        symlinkSupported,
		Rules:                   rules,
		HasLocalLibrary:         hasLocalLibrary,
		BadgeEnabled:            r.getBoolSetting(req.Context(), "notif_badge_enabled", true),
		BadgeSeverityError:      r.getBoolSetting(req.Context(), "notif_badge_severity_error", true),
		BadgeSeverityWarning:    r.getBoolSetting(req.Context(), "notif_badge_severity_warning", true),
		BadgeSeverityInfo:       r.getBoolSetting(req.Context(), "notif_badge_severity_info", false),
		APITokens:               apiTokens,
		RuleScheduleMinutes:     r.ruleScheduleMinutes(req.Context()),
		BackupRetention:         r.getIntSetting(req.Context(), "backup_retention_count", r.backupService.Retention()),
		BackupMaxAgeDays:        r.getIntSetting(req.Context(), "backup_max_age_days", r.backupService.MaxAgeDays()),
		CacheMaxSizeMB:          r.getStringSetting(req.Context(), "cache.image.max_size_mb", "0"),
		NameSimilarityThreshold: r.getNameSimilarityThreshold(req.Context()),
		Users:                   usersTabData,
		AuthProviders:           authProvidersData,
	}
	renderTempl(w, req, templates.SettingsPage(r.assetsFor(req), data))
}

// handleSettingsSectionPage handles /settings/{section} for direct section linking.
// It maps the path segment to the ?tab= query parameter and delegates to handleSettingsPage.
// GET /settings/{section}
func (r *Router) handleSettingsSectionPage(w http.ResponseWriter, req *http.Request) {
	section := normalizeSettingsSection(req.PathValue("section"))
	q := req.URL.Query()
	q.Set("tab", section)
	req.URL.RawQuery = q.Encode()
	r.handleSettingsPage(w, req)
}
