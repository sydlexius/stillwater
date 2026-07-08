package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/filesystem"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/internal/updater"
	"github.com/sydlexius/stillwater/internal/version"
	"github.com/sydlexius/stillwater/web/components"
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
	ctx := req.Context()
	if err := r.platformService.SetActive(ctx, id); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// R5 (#2306): couple the nfo_exists rule to the newly-active profile so the
	// NFO-generation contract matches the platform, and inform the user of the
	// change (never silent). Sets an HX-Trigger header, so it must run before
	// writeJSON writes the status line.
	r.syncNFORuleToActiveProfile(ctx, w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "active"})
}

// syncNFORuleToActiveProfile toggles the nfo_exists rule to match the currently
// active platform profile (R5, #2306): Plex (NFOEnabled=false) disables it, every
// other profile enables it (auto). The publisher/fixer write-gate already reads
// the active profile at write time, so this keeps the *rule* state -- and the
// user's mental model -- in sync. When the state actually changes it sets an
// HX-Trigger ("nfoRuleToggled") so the UI surfaces it (modal when disabled, toast
// when re-enabled). It is best-effort and fail-open: a missing rule service, a
// rule lookup/update error, or an unresolved profile never blocks activation --
// an unresolved profile keeps NFO enabled (the standing contract), and a rule
// error simply leaves the rule in its current state.
func (r *Router) syncNFORuleToActiveProfile(ctx context.Context, w http.ResponseWriter) {
	if r.ruleService == nil {
		return
	}
	prof, profErr := r.platformService.GetActive(ctx)
	want := platform.NFOWriteAllowed(prof, profErr) // true => rule should be enabled
	rl, err := r.ruleService.GetByID(ctx, rule.RuleNFOExists)
	if err != nil {
		r.logger.WarnContext(ctx, "nfo_exists rule lookup failed during profile activation; leaving rule unchanged", "error", err)
		return
	}
	if rl.Enabled == want {
		return // already in the right state: no churn, no popup
	}
	rl.Enabled = want
	if want {
		// Re-enabling restores the default automation so the contract self-heals.
		rl.AutomationMode = rule.AutomationModeAuto
	}
	if err := r.ruleService.Update(ctx, rl); err != nil {
		r.logger.WarnContext(ctx, "failed toggling nfo_exists rule on profile activation", "error", err, "enabled", want)
		return
	}
	profileName := "the active profile"
	if prof != nil && prof.Name != "" {
		profileName = prof.Name
	}
	r.logger.InfoContext(ctx, "nfo_exists rule toggled to match active platform profile", "enabled", want, "profile", profileName)

	state := "enabled"
	if !want {
		state = "disabled"
	}
	// map[string]any keeps a stable shape; marshal cannot fail for these types.
	payload, _ := json.Marshal(map[string]any{
		"nfoRuleToggled": map[string]string{"state": state, "profile": profileName},
	})
	w.Header().Set("HX-Trigger", string(payload))
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

	// M55 #1757 PR-5: the promoted settings screen is the single-scroll
	// about:preferences-style page (formerly next/). It renders every section in
	// one column with an eagerly-rendered Users section, so loadUsers=true always
	// (unlike the retired tabbed chrome, which loaded Users only on its active
	// tab). tab is irrelevant to the scroll-spy chrome, so pass the General
	// default for data.ActiveTab.
	data, ok := r.buildSettingsData(req, userID, true)
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// The promoted page adds an <h2> group-divider tier (Essentials / Data /
	// Integrations / System) between the page <h1> and the section cards, so the
	// shared section CARD titles render one level deeper (<h3>) to keep a
	// non-skipping heading outline. Thread that base level through the render
	// context.
	req = req.WithContext(components.WithHeadingLevel(req.Context(), 3))
	renderTempl(w, req, templates.SettingsPage(r.assetsFor(req), data))
}

// buildSettingsData aggregates every SettingsData field the settings screen
// needs. It backs handleSettingsPage so the data assembly lives in exactly one
// place (issue #1339 AC: reuse the data aggregation, no duplication). The caller
// has already verified the request is authenticated with the administrator role.
//
// data.ActiveTab is fixed to the General tab: the promoted settings screen
// (#1757 PR-5) renders one long scroll and tracks position via scroll-spy, so
// the tab selection is vestigial. loadUsers gates the users+invites query --
// the promoted page passes true because the Users section is always present on
// its single-scroll page.
//
// It returns ok=false only when the platform list (the one hard dependency)
// cannot be read; the caller then surfaces a 500. Every other subsection
// degrades gracefully (logs + renders empty) rather than failing the page.
//
//nolint:gocognit // Top-level settings page aggregator: platforms list, providers list, integrations, language prefs, update config; each subsection has its own error branch with a degrade-or-bail decision and merging them would require shared error sentinels that obscure the per-section policy.
func (r *Router) buildSettingsData(req *http.Request, userID string, loadUsers bool) (templates.SettingsData, bool) {
	profiles, err := r.platformService.List(req.Context())
	if err != nil {
		r.logger.Error("listing platforms for settings page", "error", err)
		return templates.SettingsData{}, false
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

	multiUserEnabled := r.getBoolSetting(req.Context(), "multi_user.enabled", false)

	// Populate users and invites only when the caller asks for them (stable: the
	// Users tab is active; next/: always, since the section is always rendered),
	// to avoid unnecessary DB queries on every settings page load.
	var usersTabData templates.UsersTabData
	usersTabData.MultiUserEnabled = multiUserEnabled
	usersTabData.CallerID = userID
	if multiUserEnabled && loadUsers {
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
		ActiveTab:               templates.TabGeneral,
		Libraries:               libs,
		Profiles:                profiles,
		ActiveProfile:           active,
		ProviderKeys:            providerKeys,
		Priorities:              priorities,
		Connections:             conns,
		Webhooks:                webhooks,
		WebSearchProviders:      webSearchProviders,
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
		MaintIntervalHours:      r.getIntSetting(req.Context(), "db_maintenance.interval_hours", 24),
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
	// Operational settings (#1746, #1753). Display the EFFECTIVE value, not the
	// persisted settings-table value. The scanner + rule-pipeline services
	// already incorporate the SW_* env overrides applied at startup (env-wins,
	// AC4) and every UI save updates them synchronously, so the live getters
	// reflect exactly what is in force. Reading the persisted DB value (the
	// get*Setting helpers) would show a stale, non-effective value whenever an
	// env override is active.
	// Env-pin flags (env-wins, AC4): when the SW_* var is set the loader/boot
	// overlay ignores any persisted value, so the template renders the control
	// read-only and the displayed value is the effective env value.
	data.ArtistWorkersEnvPinned = strings.TrimSpace(os.Getenv("SW_RULE_ENGINE_ARTIST_WORKERS")) != ""
	data.ScannerExclusionsEnvPinned = strings.TrimSpace(os.Getenv("SW_SCANNER_EXCLUSIONS")) != ""
	data.ScannerMtimeEnvPinned = strings.TrimSpace(os.Getenv("SW_SCANNER_MTIME_FAST_PATH")) != ""
	data.BackupIntervalEnvPinned = strings.TrimSpace(os.Getenv("SW_BACKUP_INTERVAL")) != ""

	data.ArtistWorkers = 2
	if r.pipeline != nil {
		data.ArtistWorkers = r.pipeline.ArtistWorkers()
	}
	data.ScannerExclusions = ""
	data.ScannerMtimeFastPath = true
	if r.scannerService != nil {
		data.ScannerExclusions = strings.Join(r.scannerService.Exclusions(), ", ")
		data.ScannerMtimeFastPath = r.scannerService.MtimeFastPath()
	}
	// backup.interval_hours is persist-only (the scheduler binds once at boot,
	// so the Router holds no live value). When SW_BACKUP_INTERVAL pins it, show
	// that effective value (env-wins); otherwise show the saved/pending value,
	// which the restart-required banner explains is applied on next restart.
	//
	// A present-but-INVALID SW_BACKUP_INTERVAL (non-numeric or <=0) is ignored
	// by the config loader AND blocks the boot overlay from applying any
	// persisted value, so the value actually in force is the config default.
	// Showing the persisted value here would mislead, so display the default
	// (24) and warn on the bad env var instead.
	switch {
	case data.BackupIntervalEnvPinned:
		if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("SW_BACKUP_INTERVAL"))); err == nil && n > 0 {
			data.BackupIntervalHours = n
		} else {
			r.logger.Warn("ignoring invalid SW_BACKUP_INTERVAL; showing config default",
				"value", os.Getenv("SW_BACKUP_INTERVAL"), "default", 24)
			data.BackupIntervalHours = 24
		}
	default:
		data.BackupIntervalHours = r.getIntSetting(req.Context(), "backup.interval_hours", 24)
	}
	// Load the metadata_vocab configuration so the Tag Sources card is
	// pre-filled with the current exclude patterns and count caps. Degrades
	// gracefully to the no-op default on any error (consistent with how
	// loadVocabConfig works -- it never returns nil).
	data.VocabConfig = r.loadVocabConfig(req.Context())
	return data, true
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
