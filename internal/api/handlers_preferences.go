package api

import (
	"net/http"

	"github.com/sydlexius/stillwater/internal/api/middleware"
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

// handleGetPreferences returns all preferences for the authenticated user,
// merged with defaults so every known key is always present.
// GET /api/v1/preferences
func (r *Router) handleGetPreferences(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	// Start with defaults.
	prefs := make(map[string]string, len(preferenceDefaults))
	for k, def := range preferenceDefaults {
		prefs[k] = def.defaultValue
	}

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
		if _, known := preferenceDefaults[k]; known {
			prefs[k] = v
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
	if !known {
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
	valid := false
	for _, allowed := range def.allowedValues {
		if body.Value == allowed {
			valid = true
			break
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
