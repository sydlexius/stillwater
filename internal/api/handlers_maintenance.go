package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

func (r *Router) handleMaintenanceStatus(w http.ResponseWriter, req *http.Request) {
	if r.maintenanceService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "maintenance service not available"})
		return
	}

	status, err := r.maintenanceService.Status(req.Context())
	if err != nil {
		r.logger.Error("getting maintenance status", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	if req.Header.Get("HX-Request") == "true" {
		r.renderMaintenanceStatus(w, status)
		return
	}

	writeJSON(w, http.StatusOK, status)
}

func (r *Router) handleMaintenanceOptimize(w http.ResponseWriter, req *http.Request) {
	if r.maintenanceService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "maintenance service not available"})
		return
	}

	ctx, cancel := context.WithTimeout(req.Context(), 60*time.Second)
	defer cancel()

	if err := r.maintenanceService.Optimize(ctx); err != nil {
		r.logger.Error("optimize failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "optimize failed: " + err.Error()})
		return
	}

	if req.Header.Get("HX-Request") == "true" {
		status, _ := r.maintenanceService.Status(req.Context())
		r.renderMaintenanceStatus(w, status)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "optimized"})
}

func (r *Router) handleMaintenanceVacuum(w http.ResponseWriter, req *http.Request) {
	if r.maintenanceService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "maintenance service not available"})
		return
	}

	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Minute)
	defer cancel()

	if err := r.maintenanceService.Vacuum(ctx); err != nil {
		r.logger.Error("vacuum failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "vacuum failed: " + err.Error()})
		return
	}

	if req.Header.Get("HX-Request") == "true" {
		status, _ := r.maintenanceService.Status(req.Context())
		r.renderMaintenanceStatus(w, status)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "vacuumed"})
}

func (r *Router) handleMaintenanceSchedule(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Enabled       bool `json:"enabled"`
		IntervalHours int  `json:"interval_hours"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if body.IntervalHours < 1 {
		body.IntervalHours = 24
	}

	now := time.Now().UTC().Format(time.RFC3339)
	enabledStr := "false"
	if body.Enabled {
		enabledStr = "true"
	}

	for k, v := range map[string]string{
		"db_maintenance.enabled":        enabledStr,
		"db_maintenance.interval_hours": strconv.Itoa(body.IntervalHours),
	} {
		_, err := r.db.ExecContext(req.Context(), //nolint:gosec // static query
			`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			k, v, now)
		if err != nil {
			r.logger.Error("persisting maintenance setting", "key", k, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to persist setting"})
			return
		}
	}

	if req.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<span class="text-sm text-green-600 dark:text-green-400">Schedule updated.</span>`)) //nolint:errcheck
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (r *Router) renderMaintenanceStatus(w http.ResponseWriter, status interface{}) {
	w.Header().Set("Content-Type", "text/html")

	type st struct {
		DBFileSize       int64  `json:"db_file_size"`
		WALFileSize      int64  `json:"wal_file_size"`
		PageCount        int64  `json:"page_count"`
		PageSize         int64  `json:"page_size"`
		LastOptimizeAt   string `json:"last_optimize_at,omitempty"`
		ScheduleEnabled  bool   `json:"schedule_enabled"`
		ScheduleInterval int    `json:"schedule_interval_hours"`
	}

	data, _ := json.Marshal(status)
	var s st
	json.Unmarshal(data, &s) //nolint:errcheck

	lastOpt := "Never"
	if s.LastOptimizeAt != "" {
		if t, err := time.Parse(time.RFC3339, s.LastOptimizeAt); err == nil {
			lastOpt = t.Format("2006-01-02 15:04:05 UTC")
		} else {
			lastOpt = s.LastOptimizeAt
		}
	}

	html := fmt.Sprintf(
		`<div class="grid grid-cols-2 gap-4 text-sm">`+
			`<div><span class="text-gray-500 dark:text-gray-400">Database size</span><p class="font-medium">%s</p></div>`+
			`<div><span class="text-gray-500 dark:text-gray-400">WAL size</span><p class="font-medium">%s</p></div>`+
			`<div><span class="text-gray-500 dark:text-gray-400">Pages</span><p class="font-medium">%s (%s each)</p></div>`+
			`<div><span class="text-gray-500 dark:text-gray-400">Last optimized</span><p class="font-medium">%s</p></div>`+
			`</div>`,
		formatBytes(s.DBFileSize),
		formatBytes(s.WALFileSize),
		strconv.FormatInt(s.PageCount, 10),
		formatBytes(s.PageSize),
		lastOpt,
	)
	w.Write([]byte(html)) //nolint:errcheck
}
