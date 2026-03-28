package api

import (
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/logging"
)

// handleGetLogs returns recent log entries from the in-memory ring buffer.
// Supports both JSON (API clients) and HTML fragment (HTMX polling) responses.
// GET /api/v1/logs
func (r *Router) handleGetLogs(w http.ResponseWriter, req *http.Request) {
	if r.logManager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "logging manager not available"})
		return
	}

	rb := r.logManager.RingBuffer()
	if rb == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "log buffer not available"})
		return
	}

	filter := logging.LogFilter{
		Search:    req.URL.Query().Get("search"),
		Component: req.URL.Query().Get("component"),
	}

	if level := req.URL.Query().Get("level"); level != "" {
		if !logging.ValidLevel(level) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid level: must be one of debug, info, warn, error"})
			return
		}
		filter.Level = level
	}

	if after := req.URL.Query().Get("after"); after != "" {
		t, err := time.Parse(time.RFC3339Nano, after)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid after: must be RFC3339 or RFC3339Nano timestamp"})
			return
		}
		filter.After = t
	}

	if limitStr := req.URL.Query().Get("limit"); limitStr != "" {
		n, err := strconv.Atoi(limitStr)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid limit: must be an integer"})
			return
		}
		if n < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid limit: must be non-negative"})
			return
		}
		if n > 500 {
			n = 500
		}
		filter.Limit = n
	}

	entries := rb.Entries(filter)

	if req.Header.Get("HX-Request") == "true" {
		r.renderLogEntries(w, entries)
		return
	}

	writeJSON(w, http.StatusOK, entries)
}

// handleClearLogs clears all entries from the in-memory ring buffer.
// DELETE /api/v1/logs
func (r *Router) handleClearLogs(w http.ResponseWriter, req *http.Request) {
	if r.logManager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "logging manager not available"})
		return
	}

	rb := r.logManager.RingBuffer()
	if rb == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "log buffer not available"})
		return
	}

	rb.Clear()

	if req.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<div class="flex items-center justify-center h-full text-gray-500 dark:text-gray-400 text-sm">Log buffer cleared.</div>`)) //nolint:errcheck
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}

// renderLogEntries writes log entries as HTML fragments for HTMX consumption.
func (r *Router) renderLogEntries(w http.ResponseWriter, entries []logging.LogEntry) {
	w.Header().Set("Content-Type", "text/html")

	if len(entries) == 0 {
		w.Write([]byte(`<div class="flex items-center justify-center h-full text-gray-500 dark:text-gray-400 text-sm">No log entries match the current filters.</div>`)) //nolint:errcheck
		return
	}

	var b strings.Builder

	// Entries come newest-first from the buffer, but we render oldest-first
	// so the newest entries appear at the bottom (natural scroll direction).
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		ts := entry.Time.Format("15:04:05.000")
		levelBadge := levelBadgeClass(entry.Level)
		comp := entry.Component
		if comp == "" {
			comp = "-"
		}

		b.WriteString(`<div class="flex gap-2 text-xs font-mono py-0.5 border-b border-gray-800/30 log-line">`)
		fmt.Fprintf(&b, `<span class="text-gray-500 shrink-0 w-[5.5rem]">%s</span>`, html.EscapeString(ts))
		fmt.Fprintf(&b, `<span class="px-1.5 rounded text-[10px] font-semibold uppercase shrink-0 w-12 text-center %s">%s</span>`,
			levelBadge, html.EscapeString(strings.ToUpper(entry.Level)))
		fmt.Fprintf(&b, `<span class="text-gray-400 shrink-0 w-24 truncate" title="%s">[%s]</span>`,
			html.EscapeString(comp), html.EscapeString(comp))
		fmt.Fprintf(&b, `<span class="text-gray-200 break-all">%s</span>`, html.EscapeString(entry.Message))
		b.WriteString("</div>\n")
	}

	w.Write([]byte(b.String())) //nolint:errcheck
}

// levelBadgeClass returns Tailwind classes for the log level badge.
func levelBadgeClass(level string) string {
	switch strings.ToLower(level) {
	case "debug":
		return "bg-gray-700 text-gray-300"
	case "info":
		return "bg-blue-900/50 text-blue-300"
	case "warn":
		return "bg-amber-900/50 text-amber-300"
	case "error":
		return "bg-red-900/50 text-red-300"
	default:
		return "bg-gray-700 text-gray-300"
	}
}
