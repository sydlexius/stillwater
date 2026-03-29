package api

import (
	"fmt"
	"html"
	"net/http"
	"sort"
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
		// Include date and time so entries are unambiguous across days.
		ts := entry.Time.Format("2006-01-02 15:04:05.000")
		levelBadge := levelBadgeClass(entry.Level)

		// Show source file:line when available, otherwise fall back to component.
		source := entry.Source
		if source == "" {
			source = entry.Component
		}
		if source == "" {
			source = "-"
		}

		b.WriteString(`<div class="flex gap-2 text-xs font-mono py-0.5 border-b border-gray-800/30 log-line">`)
		fmt.Fprintf(&b, `<span class="text-gray-500 shrink-0 w-[9rem]">%s</span>`, html.EscapeString(ts))
		fmt.Fprintf(&b, `<span class="px-1.5 rounded text-[10px] font-semibold uppercase shrink-0 w-12 text-center %s">%s</span>`,
			levelBadge, html.EscapeString(strings.ToUpper(entry.Level)))
		fmt.Fprintf(&b, `<span class="text-gray-400 shrink-0 w-32 truncate" title="%s">[%s]</span>`,
			html.EscapeString(source), html.EscapeString(source))

		// Message text.
		fmt.Fprintf(&b, `<span class="text-gray-200 break-all">%s`, html.EscapeString(entry.Message))

		// Render all attributes as inline key=value pairs so HTTP request
		// entries show method, path, status, duration, and any other attrs
		// from non-HTTP log entries are also visible.
		if len(entry.Attrs) > 0 {
			// Sort keys for deterministic rendering across polls.
			keys := make([]string, 0, len(entry.Attrs))
			for k := range entry.Attrs {
				keys = append(keys, k)
			}
			sort.Strings(keys)

			b.WriteString(` <span class="text-gray-500">`)
			for i, k := range keys {
				if i > 0 {
					b.WriteString(" ")
				}
				fmt.Fprintf(&b, `%s=%s`, html.EscapeString(k), html.EscapeString(fmt.Sprintf("%v", entry.Attrs[k])))
			}
			b.WriteString(`</span>`)
		}

		b.WriteString("</span></div>\n")
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
