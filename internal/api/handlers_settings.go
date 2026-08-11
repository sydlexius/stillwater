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

	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/settingsvalidate"
)

// csvToSlice splits an already-canonical CSV setting value into a trimmed,
// empty-dropped slice for handing to scanner.SetExclusions.
func csvToSlice(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
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

// HasSettingValidator reports whether key is validated by PUT /api/v1/settings.
//
// A thin forwarder to settingsvalidate.Has, kept because the cmd/stillwater
// drift guard added in #3007 asserts against this name and the question it
// answers is about THIS endpoint's contract, not the registry's storage. New
// callers outside internal/api should use settingsvalidate.Has directly.
func HasSettingValidator(key string) bool {
	return settingsvalidate.Has(key)
}

// handleUpdateSettings upserts one or more application settings.
// PUT /api/v1/settings
//
// Validation is handled by the settingsvalidate registry: each key present in the request
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
		canonical, ok, err := settingsvalidate.Validate(k, v)
		if !ok {
			// Deliberately fail-open: many legitimate settings are free-form
			// pass-through and have no validator. Rejecting unknown keys
			// outright is the stricter fix, but it breaks any caller writing
			// an unlisted key, so it needs its own assessment rather than
			// riding along here (#3004). Log at Debug so the gap is at least
			// observable without adding a line per key to every save.
			r.logger.Debug("setting stored without validation", "key", k)
			continue
		}
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

	// Push the just-persisted settings that have a live service into it so the
	// change takes effect without a restart.
	r.applyLiveSettingSideEffects(body)

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// applyLiveSettingSideEffects applies the subset of just-persisted settings
// that have a live service so the change takes effect on the next scan / rule
// pass / backup run without a restart. backup.interval_hours is intentionally
// absent: the backup scheduler binds once at boot and the UI carries a
// restart-required affordance.
func (r *Router) applyLiveSettingSideEffects(body map[string]string) {
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
	if v, ok := body["scanner.exclusions"]; ok && !r.opsSettingEnvPinned("scanner.exclusions", "SW_SCANNER_EXCLUSIONS") {
		if r.scannerService == nil {
			r.logger.Warn("persisted but not applied live: scanner service unavailable", "key", "scanner.exclusions")
		} else {
			r.scannerService.SetExclusions(csvToSlice(v))
		}
	}
	if v, ok := body["scanner.mtime_fast_path"]; ok && !r.opsSettingEnvPinned("scanner.mtime_fast_path", "SW_SCANNER_MTIME_FAST_PATH") {
		if r.scannerService == nil {
			r.logger.Warn("persisted but not applied live: scanner service unavailable", "key", "scanner.mtime_fast_path")
		} else {
			r.scannerService.SetMtimeFastPath(v == "true" || v == "1")
		}
	}
	if v, ok := body["rule_engine.artist_workers"]; ok && !r.opsSettingEnvPinned("rule_engine.artist_workers", "SW_RULE_ENGINE_ARTIST_WORKERS") {
		if r.pipeline == nil {
			r.logger.Warn("persisted but not applied live: rule pipeline unavailable", "key", "rule_engine.artist_workers")
		} else if n, err := strconv.Atoi(v); err == nil && n > 0 {
			r.pipeline.SetArtistWorkers(n)
		}
	}
}

// opsSettingEnvPinned reports whether an operational setting is pinned by its
// SW_* environment variable. When pinned, env-wins precedence (AC4) means a UI
// save must NOT apply the value live -- doing so would override env at runtime
// only for the value to revert on the next restart. The UI renders these
// controls read-only, so a save arriving for a pinned key is unexpected; it is
// warn-logged and skipped (defense in depth, not the primary guard).
func (r *Router) opsSettingEnvPinned(key, envVar string) bool {
	if strings.TrimSpace(os.Getenv(envVar)) == "" {
		return false
	}
	r.logger.Warn("ignoring live-apply for env-pinned ops setting (env-wins)",
		"key", key, "env_var", envVar)
	return true
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
