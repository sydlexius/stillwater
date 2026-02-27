package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/logging"
)

func (r *Router) handleGetLogging(w http.ResponseWriter, req *http.Request) {
	if r.logManager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "logging manager not available"})
		return
	}
	writeJSON(w, http.StatusOK, r.logManager.Config())
}

func (r *Router) handleUpdateLogging(w http.ResponseWriter, req *http.Request) {
	if r.logManager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "logging manager not available"})
		return
	}

	var cfg logging.Config

	ct := req.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		if err := json.NewDecoder(req.Body).Decode(&cfg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
	} else {
		// Form-encoded (default for HTMX forms)
		if err := req.ParseForm(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid form data"})
			return
		}
		cfg.Level = req.FormValue("level")
		cfg.Format = req.FormValue("format")
		cfg.FilePath = req.FormValue("file_path")
		if v := req.FormValue("file_max_size_mb"); v != "" {
			cfg.FileMaxSizeMB, _ = strconv.Atoi(v)
		}
		if v := req.FormValue("file_max_files"); v != "" {
			cfg.FileMaxFiles, _ = strconv.Atoi(v)
		}
		if v := req.FormValue("file_max_age_days"); v != "" {
			cfg.FileMaxAgeDays, _ = strconv.Atoi(v)
		}
	}

	if cfg.Level != "" && !logging.ValidLevel(cfg.Level) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid level; must be debug, info, warn, or error"})
		return
	}
	if cfg.Format != "" && !logging.ValidFormat(cfg.Format) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid format; must be text or json"})
		return
	}

	// Merge with current config: only overwrite fields that are provided
	current := r.logManager.Config()
	if cfg.Level == "" {
		cfg.Level = current.Level
	}
	if cfg.Format == "" {
		cfg.Format = current.Format
	}
	if cfg.FileMaxSizeMB == 0 {
		cfg.FileMaxSizeMB = current.FileMaxSizeMB
	}
	if cfg.FileMaxFiles == 0 {
		cfg.FileMaxFiles = current.FileMaxFiles
	}
	if cfg.FileMaxAgeDays == 0 {
		cfg.FileMaxAgeDays = current.FileMaxAgeDays
	}

	// Persist to settings table
	now := time.Now().UTC().Format(time.RFC3339)
	settings := map[string]string{
		"logging.level":             cfg.Level,
		"logging.format":            cfg.Format,
		"logging.file_path":         cfg.FilePath,
		"logging.file_max_size_mb":  strconv.Itoa(cfg.FileMaxSizeMB),
		"logging.file_max_files":    strconv.Itoa(cfg.FileMaxFiles),
		"logging.file_max_age_days": strconv.Itoa(cfg.FileMaxAgeDays),
	}
	for k, v := range settings {
		_, err := r.db.ExecContext(req.Context(), //nolint:gosec // static query
			`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			k, v, now)
		if err != nil {
			r.logger.Error("persisting logging setting", "key", k, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to persist setting"})
			return
		}
	}

	// Apply at runtime
	r.logManager.Reconfigure(cfg)
	r.logger.Info("logging reconfigured", "config", cfg.String())

	if req.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<span class="text-sm text-green-600 dark:text-green-400">Logging settings updated.</span>`)) //nolint:errcheck
		return
	}

	writeJSON(w, http.StatusOK, cfg)
}
