package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/sydlexius/stillwater/internal/provider"
)

// handleGetSettings returns all application settings as a key-value map.
// GET /api/v1/settings
func (r *Router) handleGetSettings(w http.ResponseWriter, req *http.Request) {
	rows, err := r.db.QueryContext(req.Context(), `SELECT key, value FROM settings ORDER BY key`) //nolint:gosec // G701: static query, no user input
	if err != nil {
		r.logger.Error("listing settings", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	defer rows.Close() //nolint:errcheck

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
func (r *Router) handleUpdateSettings(w http.ResponseWriter, req *http.Request) {
	var body map[string]string
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	// Validate backup settings before persisting
	if v, ok := body["backup_retention_count"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "backup_retention_count must be a positive integer"})
			return
		}
	}
	if v, ok := body["backup_max_age_days"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "backup_max_age_days must be zero or a positive integer"})
			return
		}
	}
	if v, ok := body["cache.image.max_size_mb"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cache.image.max_size_mb must be zero or a positive integer"})
			return
		}
	}
	if v, ok := body["provider.name_similarity_threshold"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 100 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provider.name_similarity_threshold must be between 0 and 100"})
			return
		}
		_ = n // validated, will be stored as string by the generic upsert below
	}
	if v, ok := body["rule_schedule.interval_minutes"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || (n != 0 && n < 5) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "rule_schedule.interval_minutes must be 0 (disabled) or >= 5",
			})
			return
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for k, v := range body {
		_, err := r.db.ExecContext(req.Context(), //nolint:gosec // G701: static query with parameterized values
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

	// Apply backup settings to the service immediately
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
func (r *Router) getBoolSetting(ctx context.Context, key string, fallback bool) bool {
	var v string
	err := r.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return fallback
	}
	return v == "true" || v == "1"
}

// getIntSetting reads an integer setting from the key-value table.
// Returns the fallback value if the key does not exist or cannot be parsed.
func (r *Router) getIntSetting(ctx context.Context, key string, fallback int) int {
	var v string
	err := r.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil || v == "" {
		return fallback
	}
	n, err2 := strconv.Atoi(v)
	if err2 != nil {
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
// Returns the fallback value if the key does not exist.
func (r *Router) getStringSetting(ctx context.Context, key string, fallback string) string {
	var v string
	err := r.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil || v == "" {
		return fallback
	}
	return v
}
