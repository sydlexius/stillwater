package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/filesystem"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/updater"
	"github.com/sydlexius/stillwater/internal/version"
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
	case "general", "providers", "connections", "libraries", "automation", "rules",
		"users", "auth_providers", "maintenance", "logs", "updates":
		return section
	default:
		return "general"
	}
}

// handleSettingsPage renders the settings HTML page.
// GET /settings
//
//nolint:gocognit // Top-level settings page aggregator: auth gate, platforms list, providers list, integrations, language prefs, update config, log config; each subsection has its own error branch with a degrade-or-bail decision and merging them would require shared error sentinels that obscure the per-section policy.
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
	for i := range libs {
		lib := &libs[i]
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

	// Seed canonical defaults for tracked auth-provider keys before reading
	// them, so that values matching the code default still produce a real
	// row in the settings table (and therefore survive an export/import
	// round trip). Without this, a user who views the page, sees Operator
	// already selected, and moves on never fires a DB write -- the key
	// stays absent, the export carries nothing, and the target instance
	// renders its own default which may differ from the source's (#1188).
	r.seedAuthProviderDefaults(req.Context())

	authProvidersData := templates.AuthProvidersData{
		BasePath:              r.basePath,
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
		OIDCDisplayName:       r.getStringSetting(req.Context(), "auth.providers.oidc.display_name", ""),
		OIDCLogoURL:           r.getStringSetting(req.Context(), "auth.providers.oidc.logo_url", ""),
	}

	// Load metadata language preferences for the Providers tab.
	//
	// When the user has no stored row (never set, or explicitly reset via
	// the Clear button -- see issue #1138), we intentionally pass nil to
	// the template rather than the default ["en"]. The template's
	// metadataLanguagesJSON() emits "[]" for empty slices, which the
	// Providers tab JS interprets as the "using default: English" empty
	// state. Coercing to ["en"] here would silently re-render an English
	// pill after a user's explicit reset, contradicting their action.
	//
	// Downstream metadata lookups are not affected: injectMetadataLanguages
	// reads the preference independently via langpref.Repository.Get, which
	// still falls back to langpref.DefaultTags() on the no-row case.
	var metadataLangs []string
	if userID != "" {
		var rawLangs string
		langErr := r.db.QueryRowContext(req.Context(),
			`SELECT value FROM user_preferences WHERE user_id = ? AND key = ?`,
			userID, PrefMetadataLanguages).Scan(&rawLangs)
		switch {
		case langErr == nil:
			metadataLangs = parseMetadataLanguages(normalizeMetadataLanguages(rawLangs))
		case !errors.Is(langErr, sql.ErrNoRows):
			r.logger.Warn("querying metadata_languages for settings page",
				"user_id", userID, "error", langErr)
		}
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
		ShowPlatformDebug:       r.getBoolSetting(req.Context(), "show_platform_debug", false),
		BasePath:                r.basePath,
		BasePathEnvOverride:     r.basePathFromEnv,
		TLS:                     r.tlsStatus,
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
		MetadataLanguages:       metadataLangs,
		Users:                   usersTabData,
		AuthProviders:           authProvidersData,
		Updates:                 r.buildUpdatesTabData(req.Context()),
	}
	// Inject the romanization-fallback preference separately so that adding this
	// field does not force gofmt to realign the entire struct literal above.
	data.NameRomanizationFallback = r.getUserBoolPreference(req.Context(), PrefMetadataNameRomanization, true)
	// Load the metadata_vocab configuration so the Tag Sources card is
	// pre-filled with the current exclude patterns and count caps. Degrades
	// gracefully to the no-op default on any error (consistent with how
	// loadVocabConfig works -- it never returns nil).
	data.VocabConfig = r.loadVocabConfig(req.Context())
	renderTempl(w, req, templates.SettingsPage(r.assetsFor(req), data))
}

// buildUpdatesTabData assembles the UpdatesTabData for the Settings > Updates tab.
// It reads the last check result from the updater service when available.
// If the updater service is nil (e.g. in tests), it returns sensible defaults.
func (r *Router) buildUpdatesTabData(ctx context.Context) templates.UpdatesTabData {
	data := templates.UpdatesTabData{
		CurrentVersion:     version.Version,
		Channel:            "stable",
		Enabled:            true,
		CheckIntervalHours: updater.DefaultCheckIntervalHours,
	}

	if r.updaterService == nil {
		return data
	}

	data.IsDocker = r.updaterService.IsDocker()

	cfg, err := r.updaterService.GetConfig(ctx)
	if err != nil {
		// Elevated to Error: rendering with the in-code defaults makes the
		// tab look indistinguishable from a healthy "stable + auto-check off"
		// install, and a Save click would silently overwrite the user's real
		// configuration with those defaults. The template surfaces LoadFailed
		// to users; the Error log surfaces the same condition to operators.
		r.logger.Error("loading updater config for settings page", "error", err)
		data.LoadFailed = true
	} else {
		data.Channel = string(cfg.Channel)
		data.Enabled = cfg.Enabled
		data.AutoCheck = cfg.AutoCheck
		data.AutoUpdate = cfg.AutoUpdate
		data.CheckIntervalHours = cfg.CheckIntervalHours
		if !cfg.LastAutoApplied.IsZero() {
			data.LastAutoApplied = cfg.LastAutoApplied.UTC().Format(time.RFC3339)
		}
		data.LastAutoAppliedVersion = cfg.LastAutoAppliedVersion
		data.SkippedVersions = cfg.SkippedVersions
	}

	status := r.updaterService.Status()
	if status.LastChecked != "" {
		data.LastChecked = status.LastChecked
	}
	data.UpdateAvailable = status.UpdateAvailable
	data.LatestVersion = status.Latest
	data.ReleaseURL = status.ReleaseURL
	// Surface the post-Apply restart-required state on first render so users
	// who navigate away and back to the Updates tab see the persistent
	// "restart to finish" banner without needing the JS hydrate to run.
	data.RestartRequired = status.RestartRequired
	data.PendingVersion = status.PendingVersion

	return data
}

// authProviderDefaults is the canonical list of auth.providers.* settings
// keys that the Settings > Auth Providers page reads, paired with their
// code defaults. seedAuthProviderDefaults inserts a row at this default
// for any key that has no row yet, so a "user looked at the page and
// accepted the defaults" instance round-trips faithfully through
// export/import (#1188).
//
// Boolean-shaped keys are stored as the strings "true" / "false" to match
// the parsing in getBoolSetting (which treats "true" or "1" as true).
var authProviderDefaults = []struct {
	Key     string
	Default string
}{
	// Local provider is always on; persist that fact so disabling it on the
	// source can be exported (the toggle is currently disabled in the UI but
	// the storage shape is symmetric).
	{"auth.providers.local.enabled", "true"},

	// Emby provider.
	{"auth.providers.emby.enabled", "false"},
	{"auth.providers.emby.auto_provision", "false"},
	{"auth.providers.emby.guard_rail", "admin"},
	{"auth.providers.emby.default_role", "operator"},

	// Jellyfin provider.
	{"auth.providers.jellyfin.enabled", "false"},
	{"auth.providers.jellyfin.auto_provision", "false"},
	{"auth.providers.jellyfin.guard_rail", "admin"},
	{"auth.providers.jellyfin.default_role", "operator"},

	// OIDC provider. Optional URL/string fields are NOT seeded with empty
	// strings: an "unset" OIDC issuer URL must remain absent (not present-
	// but-empty) so the export omits it cleanly. Only the role default is
	// seeded because it matches the same UI-default-masks-absent-row hazard
	// as the Emby/Jellyfin selects.
	{"auth.providers.oidc.enabled", "false"},
	{"auth.providers.oidc.auto_provision", "false"},
	{"auth.providers.oidc.default_role", "operator"},
}

// seedAuthProviderDefaults writes each canonical default to the settings
// table only when the key has no row yet. INSERT OR IGNORE keeps the call
// idempotent across page renders -- a key that has been changed (or even
// re-set to the default explicitly) keeps its existing row.
//
// Errors are logged but not returned; the page must still render even if
// the seed fails (e.g. because the DB is briefly read-only during a
// maintenance window). The follow-up render will read the in-memory
// fallbacks via getStringSetting / getBoolSetting, which preserves the
// pre-#1188 behavior.
func (r *Router) seedAuthProviderDefaults(ctx context.Context) {
	now := time.Now().UTC().Format(time.RFC3339)
	for _, d := range authProviderDefaults {
		if _, err := r.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO settings (key, value, updated_at) VALUES (?, ?, ?)`,
			d.Key, d.Default, now,
		); err != nil {
			r.logger.Warn("seeding auth provider default", "key", d.Key, "error", err)
		}
	}
}
