package api

import (
	"encoding/json"
	"net/http"
	"time"
)

func (r *Router) handleGetSettings(w http.ResponseWriter, req *http.Request) {
	rows, err := r.db.QueryContext(req.Context(), `SELECT key, value FROM settings ORDER BY key`)
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
		_, err := r.db.ExecContext(req.Context(), `
			INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
		`, k, v, now)
		if err != nil {
			r.logger.Error("upserting setting", "key", k, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}
