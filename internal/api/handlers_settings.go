package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

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

func (r *Router) handleUpdateSettings(w http.ResponseWriter, req *http.Request) {
	var body map[string]string
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
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
