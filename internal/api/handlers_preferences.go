package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/event"
	"github.com/sydlexius/stillwater/internal/langpref"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/provider/tagdict"
	"github.com/sydlexius/stillwater/web/templates"
)

// Preference key constants. Use these instead of raw strings when referencing
// preference keys in Go code to catch typos at compile time.
const (
	PrefTheme                    = "theme"
	PrefSidebarState             = "sidebar_state"
	PrefContentWidth             = "content_width"
	PrefFontFamily               = "font_family"
	PrefFontSize                 = "font_size"
	PrefLetterSpacing            = "letter_spacing"
	PrefThumbnailSize            = "thumbnail_size"
	PrefReducedMotion            = "reduced_motion"
	PrefLiteMode                 = "lite_mode"
	PrefLanguage                 = "language"
	PrefMetadataLanguages        = "metadata_languages"
	PrefMetadataNameRomanization = "metadata_name_romanization_fallback"
	PrefNotificationEnabled      = "notification_enabled"
	PrefAutoFetchImages          = "auto_fetch_images"
	PrefBgOpacity                = "bg_opacity"
	PrefPageSize                 = "page_size"

	// M55 #1774: new preference keys introduced by the preferences flyout drawer.
	// density controls layout density (compact/comfortable/spacious); mono_font
	// selects the monospace font for kbd badges, IDs, and timestamps; kbd_hints
	// governs keyboard-shortcut hint visibility.
	PrefDensity  = "density"
	PrefMonoFont = "mono_font"
	PrefKbdHints = "kbd_hints"

	// PrefArtistDetailSectionOrder and PrefArtistDetailHiddenSections are the
	// per-user artist-detail layout preferences (M55 #1336/#1339). Each stores a
	// JSON array of section identifiers. They are not in preferenceDefaults
	// because their value is a free-form ordered/visibility list rather than a
	// fixed set of strings; validateSectionList enforces their shape.
	PrefArtistDetailSectionOrder   = "artist_detail_section_order"
	PrefArtistDetailHiddenSections = "artist_detail_hidden_sections"

	// PrefSuppressConfirmPrefix is the prefix for per-action confirm suppression
	// preferences. Keys have the form "suppress_confirm_{action}" and accept
	// "true" or "false". These are not listed in preferenceDefaults because they
	// are created dynamically by the UI as the user opts out of specific dialogs.
	PrefSuppressConfirmPrefix = "suppress_confirm_"

	// PageSizeDefault is the default number of items per page.
	// PageSizeMin and PageSizeMax define the allowed range for the page_size preference.
	PageSizeDefault = 50
	PageSizeMin     = 10
	PageSizeMax     = 500
)

// preferenceDef describes a valid preference key, its default value, and allowed values.
type preferenceDef struct {
	defaultValue  string
	allowedValues []string
}

// preferenceDefaults defines every supported preference key with its default and valid values.
var preferenceDefaults = map[string]preferenceDef{
	PrefTheme:               {defaultValue: "dark", allowedValues: []string{"dark", "light", "system"}},
	PrefSidebarState:        {defaultValue: "full", allowedValues: []string{"full", "icon-only", "hidden"}},
	PrefContentWidth:        {defaultValue: "narrow", allowedValues: []string{"narrow", "wide"}},
	PrefFontFamily:          {defaultValue: "inter", allowedValues: []string{"system", "inter", "atkinson"}},
	PrefFontSize:            {defaultValue: "medium", allowedValues: []string{"small", "medium", "large", "x-large", "xx-large"}},
	PrefLetterSpacing:       {defaultValue: "normal", allowedValues: []string{"normal", "wide", "extra-wide"}},
	PrefThumbnailSize:       {defaultValue: "medium", allowedValues: []string{"small", "medium", "large"}},
	PrefReducedMotion:       {defaultValue: "system", allowedValues: []string{"system", "on", "off"}},
	PrefLiteMode:            {defaultValue: "off", allowedValues: []string{"off", "on", "auto"}},
	PrefLanguage:            {defaultValue: "en", allowedValues: []string{"en"}},
	PrefNotificationEnabled: {defaultValue: "true", allowedValues: []string{"true", "false"}},
	PrefAutoFetchImages:     {defaultValue: "false", allowedValues: []string{"true", "false"}},
	// M55 #1774: preferences flyout drawer keys.
	PrefDensity:                  {defaultValue: "comfortable", allowedValues: []string{"compact", "comfortable", "spacious"}},
	PrefMonoFont:                 {defaultValue: "jetbrains", allowedValues: []string{"system", "jetbrains", "cascadia"}},
	PrefKbdHints:                 {defaultValue: "show", allowedValues: []string{"show", "hide"}},
	PrefMetadataNameRomanization: {defaultValue: "true", allowedValues: []string{"true", "false"}},
}

func init() {
	// Validate at startup that every default value is in its allowed values list.
	// This catches typos in the preferenceDefaults map immediately.
	for key, def := range preferenceDefaults {
		found := false
		for _, v := range def.allowedValues {
			if v == def.defaultValue {
				found = true
				break
			}
		}
		if !found {
			panic("preference " + key + ": default value " + def.defaultValue + " not in allowed values")
		}
	}
}

// isPageSizeKey reports whether key is the page_size preference key.
// page_size is validated as an integer in [PageSizeMin, PageSizeMax] and is
// not listed in preferenceDefaults because its allowed values form a range
// rather than a fixed set of strings.
func isPageSizeKey(key string) bool {
	return key == PrefPageSize
}

// isBgOpacityKey reports whether key is the bg_opacity preference key.
// bg_opacity is validated as an integer in [BgOpacityMin, BgOpacityMax].
func isBgOpacityKey(key string) bool {
	return key == PrefBgOpacity
}

// BgOpacityDefault is the default background opacity percentage.
// BgOpacityMin and BgOpacityMax define the allowed range for the bg_opacity preference.
const (
	BgOpacityDefault = 85
	BgOpacityMin     = 20
	BgOpacityMax     = 100
)

// artistDetailLayoutMaxEntries and artistDetailLayoutMaxEntryLen bound an
// artist-detail layout preference so a single preference row can never hold an
// unbounded blob (each entry is a short section identifier).
const (
	artistDetailLayoutMaxEntries  = 50
	artistDetailLayoutMaxEntryLen = 64
)

// isArtistDetailLayoutKey reports whether key is one of the artist-detail layout
// preferences, which store a JSON array of section identifiers.
func isArtistDetailLayoutKey(key string) bool {
	return key == PrefArtistDetailSectionOrder || key == PrefArtistDetailHiddenSections
}

// validateSectionList parses value as a JSON array of non-empty section
// identifiers and returns its canonical compact JSON form (a JSON null or
// missing array normalizes to "[]"). It enforces upper bounds on the count and
// length of entries. ok is false for malformed input.
func validateSectionList(value string) (string, bool) {
	var ids []string
	if err := json.Unmarshal([]byte(value), &ids); err != nil {
		return "", false
	}
	if len(ids) > artistDetailLayoutMaxEntries {
		return "", false
	}
	for _, id := range ids {
		if id == "" || len(id) > artistDetailLayoutMaxEntryLen {
			return "", false
		}
	}
	if ids == nil {
		ids = []string{}
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// validateScalarPref validates a string-valued preference for the PATCH merge
// path, mirroring the per-key PUT validation for known keys, suppress_confirm_*,
// page_size, and bg_opacity. It returns the canonical stored form. ok is false
// for unknown keys or invalid values. metadata_languages is intentionally not
// handled here: its reset-to-default (empty-array deletes the row) semantics
// live on the dedicated PUT endpoint.
func validateScalarPref(key, value string) (string, bool) {
	switch {
	case isSuppressConfirmKey(key):
		if value == "true" || value == "false" {
			return value, true
		}
		return "", false
	case isPageSizeKey(key):
		n, err := strconv.Atoi(value)
		if err != nil || n < PageSizeMin || n > PageSizeMax {
			return "", false
		}
		return strconv.Itoa(n), true
	case isBgOpacityKey(key):
		n, err := strconv.Atoi(value)
		if err != nil || n < BgOpacityMin || n > BgOpacityMax {
			return "", false
		}
		return strconv.Itoa(n), true
	default:
		def, known := preferenceDefaults[key]
		if !known {
			return "", false
		}
		for _, allowed := range def.allowedValues {
			if value == allowed {
				return value, true
			}
		}
		return "", false
	}
}

// canonicalLayoutValue returns the canonical JSON-array form of a stored
// artist-detail layout value, falling back to "[]" (and logging) when the
// stored row is malformed.
func (r *Router) canonicalLayoutValue(userID, key, value string) string {
	if canonical, ok := validateSectionList(value); ok {
		return canonical
	}
	r.logger.Warn("stored artist-detail layout preference malformed; returning []",
		"user_id", userID, "key", key, "raw_value", value)
	return "[]"
}

// normalizePatchPref validates and normalizes a single PATCH preference entry,
// returning the canonical string to store. Array-valued layout keys take a JSON
// array; every other supported key takes a JSON string validated against the
// same rules as the PUT-per-key endpoint. ok is false for unknown keys or
// invalid values.
func normalizePatchPref(key string, raw json.RawMessage) (string, bool) {
	if isArtistDetailLayoutKey(key) {
		return validateSectionList(string(raw))
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return validateScalarPref(key, s)
}

// normalizeBoolPref returns "true" or "false". If raw is already one of those
// it is returned unchanged; otherwise fallback is returned. Callers that have
// access to a structured logger should log a warning when the returned value
// differs from raw so unexpected DB values are observable.
func normalizeBoolPref(raw, fallback string) string {
	switch raw {
	case "true", "false":
		return raw
	default:
		return fallback
	}
}

// normalizeBgOpacity parses a raw bg_opacity string and returns the canonical
// decimal form when it is a valid integer in [BgOpacityMin, BgOpacityMax].
func normalizeBgOpacity(raw string) string {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return strconv.Itoa(BgOpacityDefault)
	}
	if n < BgOpacityMin || n > BgOpacityMax {
		return strconv.Itoa(BgOpacityDefault)
	}
	return strconv.Itoa(n)
}

// normalizePageSize parses a raw page_size string and returns the canonical
// decimal form when it is a valid integer in [PageSizeMin, PageSizeMax].
// Invalid or out-of-range values fall back to PageSizeDefault, matching the
// same strategy used by getUserPageSize on the read path.
func normalizePageSize(raw string) string {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return strconv.Itoa(PageSizeDefault)
	}
	if n < PageSizeMin || n > PageSizeMax {
		return strconv.Itoa(PageSizeDefault)
	}
	return strconv.Itoa(n)
}

// normalizeMetadataLanguages validates and re-encodes a stored metadata_languages
// value. Invalid or empty values fall back to MetadataLanguagesDefault. This is
// the read-path counterpart to validateMetadataLanguages (used on write).
//
// Implementation lives in the langpref package; this thin wrapper is kept so
// existing api-package call sites continue to compile. Prefer
// langpref.NormalizeJSON in new code.
func normalizeMetadataLanguages(raw string) string {
	return langpref.NormalizeJSON(raw)
}

// isMetadataLanguagesKey reports whether key is the metadata_languages preference key.
// metadata_languages is stored as a JSON array of BCP 47 language tags and is
// not listed in preferenceDefaults because its validation is structural (valid
// JSON array of language-tag strings) rather than a fixed set.
func isMetadataLanguagesKey(key string) bool {
	return key == PrefMetadataLanguages
}

// MetadataLanguagesDefault is the default value for the metadata_languages
// preference when no user preference is stored.
//
// Deprecated: use langpref.DefaultJSON in new code.
const MetadataLanguagesDefault = langpref.DefaultJSON

// MetadataLanguagesMaxEntries limits how many language tags a user can store.
//
// Deprecated: use langpref.MaxEntries in new code.
const MetadataLanguagesMaxEntries = langpref.MaxEntries

// validateMetadataLanguages checks that raw is a valid JSON array of BCP 47
// language tags and returns the canonical JSON encoding together with whether
// the value is valid.
//
// Implementation lives in the langpref package; this thin wrapper is kept so
// existing api-package call sites continue to compile. Prefer
// langpref.ValidateJSON in new code.
func validateMetadataLanguages(raw string) (string, bool) {
	canonical, _, ok := langpref.ValidateJSON(raw)
	if !ok {
		return "", false
	}
	return canonical, true
}

// parseMetadataLanguages parses a stored metadata_languages JSON string into
// a slice of language tags. Returns nil on parse failure.
//
// Implementation lives in the langpref package; this thin wrapper is kept so
// existing api-package call sites continue to compile. Prefer langpref.ParseJSON
// in new code.
func parseMetadataLanguages(raw string) []string {
	return langpref.ParseJSON(raw)
}

// injectMetadataLanguages loads the user's metadata_languages preference from
// the database and injects it into the context via provider.WithMetadataLanguages.
// It also injects the metadata_name_romanization_fallback boolean preference via
// provider.WithNameRomanizationFallback. If the user has no stored preference,
// defaults are used (["en"] for languages, true for romanization fallback).
// This allows all providers downstream to read language preferences from the context.
//
// It additionally loads the metadata_vocab setting (application-level, not
// per-user) and injects it via tagdict.WithMetadataVocab so that both the
// orchestrator and the scraper-executor can apply tag filtering without a
// direct DB dependency. When the setting is absent the default config is
// injected (empty exclude list, zero caps -- a no-op until the user sets it).
func (r *Router) injectMetadataLanguages(ctx context.Context) context.Context {
	// Inject the metadata_vocab setting first, before the per-user early
	// return below. metadata_vocab is an application-level setting, so it must
	// reach every context -- including anonymous and background/system
	// contexts (e.g. scheduled refreshes) that carry no user ID. loadVocabConfig
	// returns the default no-op config when the setting has not been persisted.
	ctx = tagdict.WithMetadataVocab(ctx, r.loadVocabConfig(ctx))

	userID := middleware.UserIDFromContext(ctx)
	if userID == "" {
		return ctx
	}
	// Delegate to the langpref repository, which is the single source of
	// truth for validation, normalization, and the default fallback.
	repo := langpref.NewRepository(r.db)
	langs, err := repo.Get(ctx, userID)
	if err != nil {
		// A Get error only surfaces for unexpected database failures
		// (missing rows and malformed values both return the default
		// without error). Log and fall back to the default rather than
		// failing the request.
		r.logger.Warn("querying metadata_languages preference, using default",
			"user_id", userID, "error", err)
		langs = langpref.DefaultTags()
	}
	ctx = provider.WithMetadataLanguages(ctx, langs)

	// Inject the romanization fallback preference. Default is true (enabled)
	// to match the existing shipped behavior for users who have not set it.
	romanization := r.getUserBoolPreference(ctx, PrefMetadataNameRomanization, true)
	ctx = provider.WithNameRomanizationFallback(ctx, romanization)

	return ctx
}

// loadVocabConfig reads the metadata_vocab JSON blob from the settings table.
// Returns the default no-op config when the setting is absent or cannot be
// parsed, so callers always get a usable non-nil value.
func (r *Router) loadVocabConfig(ctx context.Context) *tagdict.VocabConfig {
	raw := r.getStringSetting(ctx, SettingMetadataVocab, "")
	if raw == "" {
		return tagdict.DefaultVocabConfig()
	}
	cfg, err := tagdict.ParseVocabConfig(raw)
	if err != nil {
		r.logger.Warn("parsing metadata_vocab setting, using default", "error", err)
		return tagdict.DefaultVocabConfig()
	}
	return cfg
}

// isSuppressConfirmKey reports whether key is a valid per-action confirm
// suppression preference (prefix "suppress_confirm_" followed by at least one
// character that is a lowercase letter, digit, or underscore).
func isSuppressConfirmKey(key string) bool {
	if !strings.HasPrefix(key, PrefSuppressConfirmPrefix) {
		return false
	}
	action := key[len(PrefSuppressConfirmPrefix):]
	if action == "" {
		return false
	}
	for _, c := range action {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// handleGetPreference returns a single preference for the authenticated user.
// For fixed preference keys the value is merged with the compiled default.
// For suppress_confirm_* keys a missing row returns "false".
// For page_size a missing row returns the string representation of PageSizeDefault.
// GET /api/v1/preferences/{key}
func (r *Router) handleGetPreference(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	key, ok := RequirePathParam(w, req, "key")
	if !ok {
		return
	}

	def, known := preferenceDefaults[key]
	suppressKey := isSuppressConfirmKey(key)
	pageSizeKey := isPageSizeKey(key)
	bgOpacityKey := isBgOpacityKey(key)
	metaLangKey := isMetadataLanguagesKey(key)
	layoutKey := isArtistDetailLayoutKey(key)
	if !known && !suppressKey && !pageSizeKey && !bgOpacityKey && !metaLangKey && !layoutKey {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown preference key"})
		return
	}

	var value string
	err := r.db.QueryRowContext(req.Context(),
		`SELECT value FROM user_preferences WHERE user_id = ? AND key = ?`, userID, key).Scan(&value)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			r.logger.Error("querying user preference", "key", key, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		// No stored row -- use default.
		switch {
		case suppressKey:
			value = "false"
		case pageSizeKey:
			value = strconv.Itoa(PageSizeDefault)
		case bgOpacityKey:
			value = strconv.Itoa(BgOpacityDefault)
		case metaLangKey:
			value = MetadataLanguagesDefault
		case layoutKey:
			// No stored row: an empty array means "use the renderer's default
			// section order / nothing hidden".
			value = "[]"
		case key == PrefAutoFetchImages:
			// Fall back to the app-level setting so the API reflects the same
			// default behavior as the artist image search handler.
			value = strconv.FormatBool(r.getBoolSetting(req.Context(), "auto_fetch_images", false))
		default:
			value = def.defaultValue
		}
	}

	// Canonicalize page_size so stale or manually edited DB rows always
	// return a clean decimal string in range.
	if pageSizeKey {
		if normalized := normalizePageSize(value); normalized != value {
			r.logger.Warn("stored page_size normalized on read",
				"user_id", userID, "raw_value", value, "normalized", normalized)
			value = normalized
		}
	}

	// Canonicalize bg_opacity to a valid integer in range.
	if bgOpacityKey {
		if normalized := normalizeBgOpacity(value); normalized != value {
			r.logger.Warn("stored bg_opacity normalized on read",
				"user_id", userID, "raw_value", value, "normalized", normalized)
			value = normalized
		}
	}

	// Canonicalize metadata_languages so malformed or manually edited DB rows
	// always return a valid JSON array of BCP 47 tags.
	if metaLangKey {
		if normalized := normalizeMetadataLanguages(value); normalized != value {
			r.logger.Warn("stored metadata_languages normalized on read",
				"user_id", userID, "raw_value", value, "normalized", normalized)
			value = normalized
		}
	}

	// Canonicalize artist-detail layout arrays so malformed or manually edited
	// DB rows always return a valid JSON array (falling back to []).
	if layoutKey {
		value = r.canonicalLayoutValue(userID, key, value)
	}

	// Canonicalize boolean preferences so malformed DB values always return
	// "true" or "false" regardless of how the value was stored.
	if known {
		if len(def.allowedValues) == 2 && def.allowedValues[0] == "true" && def.allowedValues[1] == "false" {
			// auto_fetch_images uses the app-level setting as its fallback so that
			// a malformed stored row is consistent with the no-row path above.
			boolFallback := def.defaultValue
			if key == PrefAutoFetchImages {
				boolFallback = strconv.FormatBool(r.getBoolSetting(req.Context(), "auto_fetch_images", false))
			}
			if normalized := normalizeBoolPref(value, boolFallback); normalized != value {
				r.logger.Warn("stored boolean preference normalized on read",
					"user_id", userID, "key", key, "raw_value", value, "normalized", normalized)
				value = normalized
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"key": key, "value": value})
}

// handleGetPreferences returns all preferences for the authenticated user,
// merged with defaults so every known key is always present.
// GET /api/v1/preferences
//
//nolint:gocognit // Defaults merge then per-row overlay with per-key normalization (boolean coercion with app-level fallback for auto_fetch_images, page_size clamp, bg_opacity clamp, metadata_languages re-validation); each normalizer is keyed on a different preference family so the if-ladder is the dispatch and a generic normalizer would have to thread per-family parameters through.
func (r *Router) handleGetPreferences(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	// Start with defaults (fixed keys + page_size + bg_opacity + metadata_languages).
	prefs := make(map[string]string, len(preferenceDefaults)+3)
	for k, def := range preferenceDefaults {
		prefs[k] = def.defaultValue
	}
	prefs[PrefPageSize] = strconv.Itoa(PageSizeDefault)
	prefs[PrefBgOpacity] = strconv.Itoa(BgOpacityDefault)
	prefs[PrefMetadataLanguages] = MetadataLanguagesDefault
	// Use the app-level setting as the default for auto_fetch_images so the
	// preference page reflects the effective behavior when no per-user row exists.
	prefs[PrefAutoFetchImages] = strconv.FormatBool(r.getBoolSetting(req.Context(), "auto_fetch_images", false))

	// Overlay with stored values.
	rows, err := r.db.QueryContext(req.Context(),
		`SELECT key, value FROM user_preferences WHERE user_id = ?`, userID)
	if err != nil {
		r.logger.Error("querying user preferences", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			r.logger.Error("scanning user preference", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		// Only include known keys (ignore stale rows from removed preferences).
		// page_size, bg_opacity, and metadata_languages are valid but not in preferenceDefaults.
		_, known := preferenceDefaults[k]
		if known {
			// Boolean preferences need normalization in case of manual DB edits.
			def := preferenceDefaults[k]
			if len(def.allowedValues) == 2 && def.allowedValues[0] == "true" && def.allowedValues[1] == "false" {
				// auto_fetch_images uses the app-level value already in prefs[k]
				// as its fallback so a malformed row doesn't override the effective
				// default with the compiled "false".
				boolFallback := def.defaultValue
				if k == PrefAutoFetchImages {
					boolFallback = prefs[k]
				}
				normalized := normalizeBoolPref(v, boolFallback)
				if normalized != v {
					r.logger.Warn("stored boolean preference normalized on read",
						"user_id", userID, "key", k, "raw_value", v, "normalized", normalized)
				}
				prefs[k] = normalized
			} else {
				prefs[k] = v
			}
		} else if isPageSizeKey(k) {
			normalized := normalizePageSize(v)
			if normalized != v {
				r.logger.Warn("stored page_size normalized on read",
					"user_id", userID, "raw_value", v, "normalized", normalized)
			}
			prefs[k] = normalized
		} else if isBgOpacityKey(k) {
			normalized := normalizeBgOpacity(v)
			if normalized != v {
				r.logger.Warn("stored bg_opacity normalized on read",
					"user_id", userID, "raw_value", v, "normalized", normalized)
			}
			prefs[k] = normalized
		} else if isMetadataLanguagesKey(k) {
			normalized := normalizeMetadataLanguages(v)
			if normalized != v {
				r.logger.Warn("stored metadata_languages normalized on read",
					"user_id", userID, "raw_value", v, "normalized", normalized)
			}
			prefs[k] = normalized
		} else if isArtistDetailLayoutKey(k) {
			if canonical, ok := validateSectionList(v); ok {
				prefs[k] = canonical
			} else {
				r.logger.Warn("stored artist-detail layout preference malformed; omitting on read",
					"user_id", userID, "key", k, "raw_value", v)
			}
		}
	}
	if err := rows.Err(); err != nil {
		r.logger.Error("iterating user preferences", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, prefs)
}

// handleUpdatePreference upserts a single preference for the authenticated user.
// PUT /api/v1/preferences/{key}
func (r *Router) handleUpdatePreference(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	key, ok := RequirePathParam(w, req, "key")
	if !ok {
		return
	}

	def, known := preferenceDefaults[key]
	suppressKey := isSuppressConfirmKey(key)
	pageSizeKey := isPageSizeKey(key)
	bgOpacityKey := isBgOpacityKey(key)
	metaLangKey := isMetadataLanguagesKey(key)
	layoutKey := isArtistDetailLayoutKey(key)
	if !known && !suppressKey && !pageSizeKey && !bgOpacityKey && !metaLangKey && !layoutKey {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown preference key"})
		return
	}

	var body struct {
		Value string `json:"value"`
	}
	if !DecodeJSON(w, req, &body) {
		return
	}

	// Validate value against allowed list.
	// suppress_confirm_* keys only accept "true" or "false".
	// page_size must be an integer in [PageSizeMin, PageSizeMax].
	// bg_opacity must be an integer in [BgOpacityMin, BgOpacityMax].
	// metadata_languages must be a JSON array of valid language tags.
	var valid bool
	switch {
	case suppressKey:
		valid = body.Value == "true" || body.Value == "false"
	case pageSizeKey:
		n, err := strconv.Atoi(body.Value)
		valid = err == nil && n >= PageSizeMin && n <= PageSizeMax
		if valid {
			// Normalize to canonical decimal so "+10" or "010" is stored as "10".
			body.Value = strconv.Itoa(n)
		}
	case bgOpacityKey:
		n, err := strconv.Atoi(body.Value)
		valid = err == nil && n >= BgOpacityMin && n <= BgOpacityMax
		if valid {
			body.Value = strconv.Itoa(n)
		}
	case metaLangKey:
		// An empty array is the explicit "reset to default" signal from
		// the UI: delete the row so the read path falls back to
		// langpref.DefaultTags() rather than silently substituting "en"
		// and losing the user's intent. See issue #1138.
		if trimmed := strings.TrimSpace(body.Value); trimmed == "[]" {
			repo := langpref.NewRepository(r.db)
			if err := repo.Delete(req.Context(), userID); err != nil {
				r.logger.Error("deleting metadata_languages preference", "user_id", userID, "error", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
			// Echo the empty array the client sent so the UI can stay in
			// its visible "using default" state for the rest of the
			// session. GetRaw will return DefaultJSON on a later page
			// load -- that is a deliberate ambiguity: we do not remember
			// the difference between "never set" and "explicitly cleared"
			// across reloads, and the render path handles either case.
			writeJSON(w, http.StatusOK, map[string]string{"key": key, "value": "[]"})
			return
		}
		canonical, ok := validateMetadataLanguages(body.Value)
		valid = ok
		if valid {
			body.Value = canonical
		}
	case layoutKey:
		canonical, ok := validateSectionList(body.Value)
		valid = ok
		if valid {
			body.Value = canonical
		}
	default:
		for _, allowed := range def.allowedValues {
			if body.Value == allowed {
				valid = true
				break
			}
		}
	}
	if !valid {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid value for preference " + key})
		return
	}

	_, err := r.db.ExecContext(req.Context(),
		`INSERT INTO user_preferences (user_id, key, value, updated_at)
		 VALUES (?, ?, ?, datetime('now'))
		 ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		userID, key, body.Value)
	if err != nil {
		r.logger.Error("upserting user preference", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"key": key, "value": body.Value})
}

// handlePatchPreferences merges a partial set of preferences for the
// authenticated user in a single request. PATCH /api/v1/preferences
//
// The body is a JSON object mapping preference keys to values: scalar
// preferences take a JSON string (e.g. {"theme":"dark"}); the artist-detail
// layout keys take a JSON array of section ids (e.g.
// {"artist_detail_section_order":["bio","artwork"]}). Only the keys present in
// the body are written; absent keys are left untouched (merge semantics). Every
// entry is validated before any write, so an unknown key or invalid value
// rejects the whole request atomically with no partial application. The
// response echoes the canonical stored form of the keys that were written.
func (r *Router) handlePatchPreferences(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var body map[string]json.RawMessage
	if !DecodeJSON(w, req, &body) {
		return
	}

	// Validate + normalize every entry before writing anything so the merge is
	// all-or-nothing: a single bad key never leaves a partially applied change.
	normalized := make(map[string]string, len(body))
	for key, raw := range body {
		val, ok := normalizePatchPref(key, raw)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid value or unknown key: " + key})
			return
		}
		normalized[key] = val
	}

	if len(normalized) > 0 {
		tx, err := r.db.BeginTx(req.Context(), nil)
		if err != nil {
			r.logger.Error("begin tx for preferences patch", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		defer func() { _ = tx.Rollback() }()

		for key, val := range normalized {
			if _, err := tx.ExecContext(req.Context(),
				`INSERT INTO user_preferences (user_id, key, value, updated_at)
				 VALUES (?, ?, ?, datetime('now'))
				 ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
				userID, key, val); err != nil {
				r.logger.Error("upserting preference in patch", "key", key, "error", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
		}

		if err := tx.Commit(); err != nil {
			r.logger.Error("commit preferences patch", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		// Announce the change so other open tabs can refetch or toast. Only
		// emitted when a write actually landed (an empty PATCH is a no-op).
		// The hub is in-process, so delivery is effectively immediate.
		if r.eventBus != nil {
			// settings.changed is broadcast to every connected client, so the
			// payload must not carry the actor's user id (that would leak who
			// changed a per-user preference to all other users). A bare
			// section + timestamp is enough for other tabs to refetch.
			r.eventBus.Publish(event.Event{
				Type: event.SettingsChanged,
				Data: map[string]any{
					"sectionId": "preferences",
					"ts":        time.Now().UTC().Format(time.RFC3339),
				},
			})
		}
	}

	writeJSON(w, http.StatusOK, normalized)
}

// handleUserPreferencesPage renders the user preferences page (accessible to
// all authenticated users). It loads all preference values from the database
// and falls back to compiled defaults for any missing keys.
// GET /preferences
func (r *Router) handleUserPreferencesPage(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		r.renderLoginPage(w, req)
		return
	}

	ctx := req.Context()

	// Load all stored preferences for this user.
	rows, err := r.db.QueryContext(ctx,
		`SELECT key, value FROM user_preferences WHERE user_id = ?`, userID)
	if err != nil {
		r.logger.Error("querying user preferences for page", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	stored := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			r.logger.Error("scanning user preference", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		stored[k] = v
	}
	if err := rows.Err(); err != nil {
		r.logger.Error("iterating user preferences", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	pref := func(key string) string {
		if v, ok := stored[key]; ok {
			return v
		}
		if def, ok := preferenceDefaults[key]; ok {
			return def.defaultValue
		}
		return ""
	}

	// Parse page_size as integer; fall back to default.
	pageSize := PageSizeDefault
	if v, ok := stored[PrefPageSize]; ok {
		if n, err2 := strconv.Atoi(v); err2 == nil && n >= PageSizeMin && n <= PageSizeMax {
			pageSize = n
		} else {
			r.logger.Warn("stored page_size invalid for preferences page, using default",
				"user_id", userID, "raw_value", v)
		}
	}

	// Parse bg_opacity as integer; fall back to default.
	bgOpacity := strconv.Itoa(BgOpacityDefault)
	if v, ok := stored[PrefBgOpacity]; ok {
		normalized := normalizeBgOpacity(v)
		if normalized != v {
			r.logger.Warn("stored bg_opacity invalid for preferences page, using default",
				"user_id", userID, "raw_value", v, "normalized", normalized)
		}
		bgOpacity = normalized
	}

	// Determine auto_fetch_images with the app-level setting as the fallback so
	// the toggle reflects the effective behavior when no per-user row exists.
	legacyAutoFetch := strconv.FormatBool(r.getBoolSetting(ctx, "auto_fetch_images", false))
	autoFetchImages := legacyAutoFetch
	if v, ok := stored[PrefAutoFetchImages]; ok {
		normalized := normalizeBoolPref(v, legacyAutoFetch)
		if normalized != v {
			r.logger.Warn("stored auto_fetch_images normalized for preferences page",
				"user_id", userID, "raw_value", v, "normalized", normalized)
		}
		autoFetchImages = normalized
	}

	prefs := templates.PreferencesData{
		Theme:             pref(PrefTheme),
		ThumbnailSize:     pref(PrefThumbnailSize),
		SidebarState:      pref(PrefSidebarState),
		ContentWidth:      pref(PrefContentWidth),
		ReducedMotion:     pref(PrefReducedMotion),
		Language:          pref(PrefLanguage),
		FontFamily:        pref(PrefFontFamily),
		LetterSpacing:     pref(PrefLetterSpacing),
		FontSize:          pref(PrefFontSize),
		LiteMode:          pref(PrefLiteMode),
		PageSize:          pageSize,
		AutoFetchImages:   autoFetchImages,
		BackgroundOpacity: bgOpacity,
		// M55 #1774 flyout drawer keys.
		Density:             pref(PrefDensity),
		MonoFont:            pref(PrefMonoFont),
		KbdHints:            pref(PrefKbdHints),
		NotificationEnabled: normalizeBoolPref(pref(PrefNotificationEnabled), preferenceDefaults[PrefNotificationEnabled].defaultValue),
		// Artist detail layout: parse stored JSON arrays (nil = use default order).
		ArtistDetailSectionOrder:   parseSectionList(stored[PrefArtistDetailSectionOrder]),
		ArtistDetailHiddenSections: parseSectionList(stored[PrefArtistDetailHiddenSections]),
	}

	renderTempl(w, req, templates.UserPreferencesPage(r.assetsFor(req), prefs))
}

// getUserBoolPreference reads a boolean user preference from the user_preferences
// table. Returns the fallback value if no row exists for the current user.
func (r *Router) getUserBoolPreference(ctx context.Context, key string, fallback bool) bool {
	userID := middleware.UserIDFromContext(ctx)
	if userID == "" {
		return fallback
	}
	var v string
	err := r.db.QueryRowContext(ctx,
		`SELECT value FROM user_preferences WHERE user_id = ? AND key = ?`, userID, key).Scan(&v)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			r.logger.Error("querying boolean user preference",
				"user_id", userID, "key", key, "error", err)
		}
		return fallback
	}
	switch v {
	case "true":
		return true
	case "false":
		return false
	default:
		r.logger.Warn("stored boolean user preference invalid, using fallback",
			"user_id", userID, "key", key, "raw_value", v)
		return fallback
	}
}

// getUserStringPreference reads a string user preference from the
// user_preferences table. Returns fallback when no row exists for the current
// user or when the user is unauthenticated. Mirrors getUserBoolPreference.
func (r *Router) getUserStringPreference(ctx context.Context, key, fallback string) string {
	userID := middleware.UserIDFromContext(ctx)
	if userID == "" {
		return fallback
	}
	var v string
	err := r.db.QueryRowContext(ctx,
		`SELECT value FROM user_preferences WHERE user_id = ? AND key = ?`, userID, key).Scan(&v)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			r.logger.Error("querying string user preference",
				"user_id", userID, "key", key, "error", err)
		}
		return fallback
	}
	return v
}

// parseSectionList parses a stored JSON-array-of-strings preference (e.g.
// artist_detail_section_order). Returns nil on empty/invalid input so callers
// fall back to the default order. Unknown ids are kept as-is; the renderer
// ignores any id it does not recognize.
func parseSectionList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	// Normalize any empty array (e.g. "[ ]", "[\n]") to nil, not just the exact
	// "[]" fast-path above, so the nil-on-empty contract holds for all whitespace
	// variants and callers reliably fall back to the default order.
	if len(out) == 0 {
		return nil
	}
	return out
}

// getUserPageSize reads the page_size preference for the given user from the
// database. If no preference is stored or the stored value is out of range,
// PageSizeDefault is returned. The query param value (queryParam) takes
// precedence when it is non-zero, allowing API callers to override the user
// preference on a per-request basis.
func (r *Router) getUserPageSize(ctx context.Context, userID string, queryParam int) int {
	// Explicit query parameter overrides the stored preference.
	if queryParam > 0 {
		if queryParam < PageSizeMin {
			return PageSizeMin
		}
		if queryParam > PageSizeMax {
			return PageSizeMax
		}
		return queryParam
	}

	if userID == "" || r.db == nil {
		return PageSizeDefault
	}

	var raw string
	err := r.db.QueryRowContext(ctx,
		`SELECT value FROM user_preferences WHERE user_id = ? AND key = ?`, userID, PrefPageSize).Scan(&raw)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			r.logger.Warn("querying user page size preference", "user_id", userID, "error", err)
		}
		return PageSizeDefault
	}
	n, parseErr := strconv.Atoi(raw)
	if parseErr != nil {
		r.logger.Warn("stored page_size is not a valid integer, using default",
			"user_id", userID, "raw_value", raw, "error", parseErr)
		return PageSizeDefault
	}
	if n < PageSizeMin || n > PageSizeMax {
		r.logger.Warn("stored page_size is out of range, using default",
			"user_id", userID, "value", n, "min", PageSizeMin, "max", PageSizeMax)
		return PageSizeDefault
	}
	return n
}
