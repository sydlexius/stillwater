package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/web/templates"
)

// Preference key constants. Use these instead of raw strings when referencing
// preference keys in Go code to catch typos at compile time.
const (
	PrefTheme               = "theme"
	PrefGlassIntensity      = "glass_intensity"
	PrefSidebarState        = "sidebar_state"
	PrefContentWidth        = "content_width"
	PrefFontFamily          = "font_family"
	PrefFontSize            = "font_size"
	PrefLetterSpacing       = "letter_spacing"
	PrefThumbnailSize       = "thumbnail_size"
	PrefReducedMotion       = "reduced_motion"
	PrefLiteMode            = "lite_mode"
	PrefLanguage            = "language"
	PrefNotificationEnabled = "notification_enabled"
	PrefPageSize            = "page_size"

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
	PrefGlassIntensity:      {defaultValue: "medium", allowedValues: []string{"light", "medium", "heavy"}},
	PrefSidebarState:        {defaultValue: "full", allowedValues: []string{"full", "icon-only", "hidden"}},
	PrefContentWidth:        {defaultValue: "narrow", allowedValues: []string{"narrow", "wide"}},
	PrefFontFamily:          {defaultValue: "inter", allowedValues: []string{"system", "inter", "atkinson"}},
	PrefFontSize:            {defaultValue: "medium", allowedValues: []string{"small", "medium", "large"}},
	PrefLetterSpacing:       {defaultValue: "normal", allowedValues: []string{"normal", "wide", "extra-wide"}},
	PrefThumbnailSize:       {defaultValue: "medium", allowedValues: []string{"small", "medium", "large"}},
	PrefReducedMotion:       {defaultValue: "system", allowedValues: []string{"system", "on", "off"}},
	PrefLiteMode:            {defaultValue: "off", allowedValues: []string{"off", "on", "auto"}},
	PrefLanguage:            {defaultValue: "en", allowedValues: []string{"en"}},
	PrefNotificationEnabled: {defaultValue: "true", allowedValues: []string{"true", "false"}},
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

// normalizePageSize parses a raw page_size string, clamps it to
// [PageSizeMin, PageSizeMax], and returns the canonical decimal form.
// If raw is not a valid integer, PageSizeDefault is returned.
func normalizePageSize(raw string) string {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return strconv.Itoa(PageSizeDefault)
	}
	if n < PageSizeMin {
		n = PageSizeMin
	} else if n > PageSizeMax {
		n = PageSizeMax
	}
	return strconv.Itoa(n)
}

// isSuppressConfirmKey reports whether key is a valid per-action confirm
// suppression preference (prefix "suppress_confirm_" followed by at least one
// character that is a lowercase letter, digit, or underscore).
func isSuppressConfirmKey(key string) bool {
	if !strings.HasPrefix(key, PrefSuppressConfirmPrefix) {
		return false
	}
	action := key[len(PrefSuppressConfirmPrefix):]
	if len(action) == 0 {
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
	if !known && !suppressKey && !pageSizeKey {
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

	writeJSON(w, http.StatusOK, map[string]string{"key": key, "value": value})
}

// handleGetPreferences returns all preferences for the authenticated user,
// merged with defaults so every known key is always present.
// GET /api/v1/preferences
func (r *Router) handleGetPreferences(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	// Start with defaults (fixed keys + page_size).
	prefs := make(map[string]string, len(preferenceDefaults)+1)
	for k, def := range preferenceDefaults {
		prefs[k] = def.defaultValue
	}
	prefs[PrefPageSize] = strconv.Itoa(PageSizeDefault)

	// Overlay with stored values.
	rows, err := r.db.QueryContext(req.Context(),
		`SELECT key, value FROM user_preferences WHERE user_id = ?`, userID)
	if err != nil {
		r.logger.Error("querying user preferences", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	defer rows.Close() //nolint:errcheck

	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			r.logger.Error("scanning user preference", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		// Only include known keys (ignore stale rows from removed preferences).
		// page_size is valid but not in preferenceDefaults (range-based, not enum).
		_, known := preferenceDefaults[k]
		if known {
			prefs[k] = v
		} else if isPageSizeKey(k) {
			normalized := normalizePageSize(v)
			if normalized != v {
				r.logger.Warn("stored page_size normalized on read",
					"user_id", userID, "raw_value", v, "normalized", normalized)
			}
			prefs[k] = normalized
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
	if !known && !suppressKey && !pageSizeKey {
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
	defer rows.Close() //nolint:errcheck

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
		}
	}

	prefs := templates.AppearancePrefsData{
		Theme:          pref(PrefTheme),
		GlassIntensity: pref(PrefGlassIntensity),
		ThumbnailSize:  pref(PrefThumbnailSize),
		SidebarState:   pref(PrefSidebarState),
		ContentWidth:   pref(PrefContentWidth),
		ReducedMotion:  pref(PrefReducedMotion),
		Language:       pref(PrefLanguage),
		FontFamily:     pref(PrefFontFamily),
		LetterSpacing:  pref(PrefLetterSpacing),
		FontSize:       pref(PrefFontSize),
		LiteMode:       pref(PrefLiteMode),
		PageSize:       pageSize,
	}

	renderTempl(w, req, templates.UserPreferencesPage(r.assetsFor(req), prefs))
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

	if userID == "" {
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
