package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/provider"
)

// validator validates a setting value and returns the canonical form to persist.
// If the value is invalid the returned error message is surfaced directly to the
// caller; keep it user-readable and free of internal package details.
type validator func(v string) (canonical string, err error)

// settingValidators maps setting keys to their validation functions.
// To add a new validated setting: add one entry here.
// Keys absent from the map are accepted without validation (pass-through).
var settingValidators = map[string]validator{
	"backup_retention_count":             validatePositiveInt("backup_retention_count"),
	"backup_max_age_days":                validateNonNegativeInt("backup_max_age_days"),
	"cache.image.max_size_mb":            validateNonNegativeInt("cache.image.max_size_mb"),
	"images.backdrop.target_count":       validateIntRange("images.backdrop.target_count", 1, 10),
	"provider.name_similarity_threshold": validateIntRange("provider.name_similarity_threshold", 0, 100),
	"rule_schedule.interval_minutes":     validateRuleScheduleMinutes,
	"musicbrainz.contributions":          validateEnum("musicbrainz.contributions", "disabled", "web_form", "api"),
	"auth.method":                        validateEnum("auth.method", "local", "emby", "jellyfin"),
	"server.base_path":                   validateBasePath,
	"auth.providers.local.enabled":       validateLocalAuthEnabled,
}

// validatePositiveInt returns a validator that accepts integers >= 1.
func validatePositiveInt(key string) validator {
	return func(v string) (string, error) {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return "", fmt.Errorf("%s must be a positive integer", key)
		}
		return v, nil
	}
}

// validateNonNegativeInt returns a validator that accepts integers >= 0.
func validateNonNegativeInt(key string) validator {
	return func(v string) (string, error) {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return "", fmt.Errorf("%s must be zero or a positive integer", key)
		}
		return v, nil
	}
}

// validateIntRange returns a validator that accepts integers in [lo, hi].
func validateIntRange(key string, lo, hi int) validator {
	return func(v string) (string, error) {
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			return "", fmt.Errorf("%s must be between %d and %d", key, lo, hi)
		}
		return v, nil
	}
}

// validateEnum returns a validator that accepts only the listed literal values.
func validateEnum(key string, allowed ...string) validator {
	return func(v string) (string, error) {
		for _, a := range allowed {
			if v == a {
				return v, nil
			}
		}
		return "", fmt.Errorf("%s must be %s", key, strings.Join(allowed, ", "))
	}
}

// validateRuleScheduleMinutes accepts 0 (disabled) or any value >= 5.
func validateRuleScheduleMinutes(v string) (string, error) {
	n, err := strconv.Atoi(v)
	if err != nil || (n != 0 && n < 5) {
		return "", errors.New("rule_schedule.interval_minutes must be 0 (disabled) or >= 5")
	}
	return v, nil
}

// validateBasePath validates the server.base_path setting.
//
// Rules:
//   - "/" (root) is the canonical "no prefix" value and is always valid.
//   - An empty string is normalised to "/".
//   - Any other value must start with "/" and must NOT end with "/".
//   - The value must not start with "//" or "/\".
//   - Allowed characters: letters, digits, hyphen, underscore, slash.
//
// We do NOT enforce here that the env override is unset: an admin who edits
// the YAML config out-of-band still expects the saved override to take effect
// on the next process restart that lacks SW_BASE_PATH. The UI already hides
// the editable input when the env override is active, so the only way to reach
// this validator with the env set is a direct API call, which we treat as
// "save the override anyway; env still wins at runtime."
func validateBasePath(v string) (string, error) {
	bp := strings.TrimSpace(v)
	if bp == "" {
		return "/", nil
	}
	if bp == "/" {
		return "/", nil
	}
	if !strings.HasPrefix(bp, "/") {
		return "", errors.New("server.base_path must start with \"/\"")
	}
	// Mirror the loader (cmd/stillwater/main.go isValidPersistedBasePath) and
	// the client (web/templates/settings.templ saveBasePath): a second character
	// of "/" or "\" is rejected. The charset check below would already reject
	// backslash, but "//foo" passes that check and would otherwise persist a
	// value the loader then refuses to apply on next restart, leaving the user
	// with a successful save and a restart banner for a base path that is
	// silently ignored.
	if len(bp) >= 2 && (bp[1] == '/' || bp[1] == '\\') {
		return "", errors.New("server.base_path must not start with \"//\" or \"/\\\\\"")
	}
	if strings.HasSuffix(bp, "/") {
		return "", errors.New("server.base_path must not end with \"/\"")
	}
	for _, c := range bp {
		ok := (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '/'
		if !ok {
			return "", errors.New("server.base_path may only contain letters, digits, hyphens, underscores, and slashes")
		}
	}
	return bp, nil
}

// validateLocalAuthEnabled rejects any attempt to disable local authentication.
// Local auth provides break-glass access when all federated providers are
// misconfigured. The value is normalised (trimmed, lowercased) before the check
// to guard against "FALSE", " false ", and similar variants.
func validateLocalAuthEnabled(v string) (string, error) {
	normalized := strings.TrimSpace(strings.ToLower(v))
	switch normalized {
	case "true", "1":
		return "true", nil
	case "false", "0", "":
		return "", errors.New("local authentication cannot be disabled; it provides break-glass access if all other providers are misconfigured")
	default:
		return "", errors.New("auth.providers.local.enabled must be \"true\"")
	}
}

// handleGetSettings returns all application settings as a key-value map.
// GET /api/v1/settings
func (r *Router) handleGetSettings(w http.ResponseWriter, req *http.Request) {
	rows, err := r.db.QueryContext(req.Context(), `SELECT key, value FROM settings ORDER BY key`)
	if err != nil {
		r.logger.Error("listing settings", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	defer rows.Close() //nolint:errcheck // Close error not actionable on cleanup

	settings := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			r.logger.Error("scanning setting", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		settings[k] = v
	}
	if err := rows.Err(); err != nil {
		r.logger.Error("iterating settings", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

// handleUpdateSettings upserts one or more application settings.
// PUT /api/v1/settings
//
// Validation is handled by settingValidators: each key present in the request
// body is looked up in the registry and, if a validator is found, the value is
// validated and potentially normalised. Keys absent from the registry are
// accepted without validation. All validations run before any write occurs.
func (r *Router) handleUpdateSettings(w http.ResponseWriter, req *http.Request) {
	var body map[string]string
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	// onboarding.baseline_choice is a request-time signal from the OOBE
	// wizard, not a persisted setting. Translate it to the derived
	// foreign_files.baseline_completed flag in body so the generic
	// validate-then-upsert path below handles persistence and error
	// propagation uniformly with every other setting. Reject unexpected
	// values explicitly rather than silently dropping them (#1142 / #1698
	// review feedback). The input is normalised (trimmed, lowercased)
	// before the switch so case variations and stray whitespace from
	// non-OOBE callers don't reject -- mirrors the pattern used by
	// validateLocalAuthEnabled.
	if choice, ok := body["onboarding.baseline_choice"]; ok {
		delete(body, "onboarding.baseline_choice")
		switch strings.TrimSpace(strings.ToLower(choice)) {
		case "yes":
			body["foreign_files.baseline_completed"] = "true"
		case "no":
			body["foreign_files.baseline_completed"] = ""
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": `onboarding.baseline_choice must be "yes" or "no"`})
			return
		}
	}

	// Validate all keys up front; normalise values in-place.
	for k, v := range body {
		fn, ok := settingValidators[k]
		if !ok {
			continue
		}
		canonical, err := fn(v)
		if err != nil {
			if k == "auth.providers.local.enabled" {
				r.logger.Warn("rejecting settings update", "key", k, "value", v)
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		body[k] = canonical
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for k, v := range body {
		_, err := r.db.ExecContext(req.Context(),
			`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			k, v, now)
		if err != nil {
			r.logger.Error("upserting setting", "key", k, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	}

	// Clear legacy hours key when minutes is explicitly set, so the UI and
	// startup code don't mix the two representations.
	if _, ok := body["rule_schedule.interval_minutes"]; ok {
		_, _ = r.db.ExecContext(req.Context(), `DELETE FROM settings WHERE key = ?`, "rule_schedule.interval_hours")
	}

	// Apply backup settings to the service immediately so the live service
	// reflects the new values without requiring a restart.
	if v, ok := body["backup_retention_count"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			r.backupService.SetRetention(n)
		}
	}
	if v, ok := body["backup_max_age_days"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			r.backupService.SetMaxAgeDays(n)
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// getBoolSetting reads a boolean setting from the key-value table.
// Returns the fallback value if the key does not exist or cannot be parsed.
// Logs a warning for genuine DB errors (i.e. anything other than a missing row).
func (r *Router) getBoolSetting(ctx context.Context, key string, fallback bool) bool {
	var v string
	err := r.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			r.logger.Warn("reading bool setting", "key", key, "error", err)
		}
		return fallback
	}
	return v == "true" || v == "1"
}

// getIntSetting reads an integer setting from the key-value table.
// Returns the fallback value if the key does not exist or cannot be parsed.
// Logs a warning for genuine DB errors (i.e. anything other than a missing row).
// Logs a warning when a stored value is not a valid integer.
func (r *Router) getIntSetting(ctx context.Context, key string, fallback int) int {
	var v string
	err := r.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			r.logger.Warn("reading int setting", "key", key, "error", err)
		}
		return fallback
	}
	if v == "" {
		return fallback
	}
	n, err2 := strconv.Atoi(v)
	if err2 != nil {
		r.logger.Warn("int setting value is not a valid integer", "key", key, "stored_value", v, "fallback", fallback)
		return fallback
	}
	return n
}

// getNameSimilarityThreshold reads the name similarity threshold via the
// provider SettingsService, which applies clamping for corrupt values.
// Falls back to the default if the service returns an error.
func (r *Router) getNameSimilarityThreshold(ctx context.Context) int {
	threshold, err := r.providerSettings.GetNameSimilarityThreshold(ctx)
	if err != nil {
		r.logger.Warn("reading name similarity threshold, using default",
			"error", err,
		)
		return provider.DefaultNameSimilarityThreshold
	}
	return threshold
}

// ruleScheduleMinutes returns the configured schedule interval in minutes,
// applying a legacy fallback from interval_hours when the minutes key is absent.
func (r *Router) ruleScheduleMinutes(ctx context.Context) int {
	if mins := r.getIntSetting(ctx, "rule_schedule.interval_minutes", 0); mins != 0 {
		return mins
	}
	if legacyHours := r.getIntSetting(ctx, "rule_schedule.interval_hours", 0); legacyHours > 0 {
		return legacyHours * 60
	}
	return 0
}

// getStringSetting reads a string setting from the key-value table.
// Returns the fallback value if the key does not exist or the stored value is
// empty. Logs a warning for genuine DB errors (i.e. anything other than a
// missing row).
func (r *Router) getStringSetting(ctx context.Context, key string, fallback string) string {
	var v string
	err := r.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			r.logger.Warn("reading string setting", "key", key, "error", err)
		}
		return fallback
	}
	if v == "" {
		return fallback
	}
	return v
}
