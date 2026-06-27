package api

import (
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/event"
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

	// If a specific log file is requested, read from disk instead of the ring buffer.
	if file := req.URL.Query().Get("file"); file != "" {
		entries, err := r.logManager.ReadLogFile(file, filter)
		if err != nil {
			r.logger.Warn("reading log file", "file", file, "error", err)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read log file"})
			return
		}
		if req.Header.Get("HX-Request") == "true" {
			r.renderLogEntries(w, entries)
			return
		}
		writeJSON(w, http.StatusOK, entries)
		return
	}

	// Default: read from the in-memory ring buffer (live view).
	if after := req.URL.Query().Get("after"); after != "" {
		t, err := time.Parse(time.RFC3339Nano, after)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid after: must be RFC3339 or RFC3339Nano timestamp"})
			return
		}
		filter.After = t
	}

	entries := rb.Entries(filter)

	if req.Header.Get("HX-Request") == "true" {
		r.renderLogEntries(w, entries)
		return
	}

	writeJSON(w, http.StatusOK, entries)
}

// handleLogsComponents returns the distinct component values currently present
// in the in-memory log ring buffer, sorted alphabetically. This is the
// vocabulary the next/ logs viewer renders as Component filter pills (#1338):
// the natural set of components the user can filter the live stream on (the
// stream's `scope` predicate is an exact component match). Administrator-gated
// at the route (mirroring /api/v1/logs/stream).
//
// GET /api/v1/logs/components -> {"components": ["api", "scanner", ...]}
func (r *Router) handleLogsComponents(w http.ResponseWriter, _ *http.Request) {
	if r.logManager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "logging manager not available"})
		return
	}
	rb := r.logManager.RingBuffer()
	if rb == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "log buffer not available"})
		return
	}
	// RingBuffer.Components always returns a non-nil slice (empty when the buffer
	// holds no components), so this serializes as a JSON array, never null.
	writeJSON(w, http.StatusOK, map[string][]string{"components": rb.Components()})
}

// handleListLogFiles returns the log files available for browsing.
// GET /api/v1/logs/files
func (r *Router) handleListLogFiles(w http.ResponseWriter, req *http.Request) {
	if r.logManager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "logging manager not available"})
		return
	}

	files, err := r.logManager.ListLogFiles()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list log files"})
		return
	}

	if files == nil {
		files = []logging.LogFileInfo{}
	}

	writeJSON(w, http.StatusOK, files)
}

// handleDeleteLogFiles deletes rotated log files (all except the current file).
// DELETE /api/v1/logs/files
func (r *Router) handleDeleteLogFiles(w http.ResponseWriter, req *http.Request) {
	if r.logManager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "logging manager not available"})
		return
	}
	deleted, bytesFreed, err := r.logManager.DeleteRotatedFiles()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to delete log files"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted":     deleted,
		"bytes_freed": bytesFreed,
	})
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
		w.Write([]byte(`<div class="flex items-center justify-center h-full text-gray-500 dark:text-gray-400 text-sm">Log buffer cleared.</div>`)) //nolint:errcheck // Best-effort write to HTTP response; client disconnect mid-write is not actionable
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}

// renderLogEntries writes log entries as HTML fragments for HTMX consumption.
func (r *Router) renderLogEntries(w http.ResponseWriter, entries []logging.LogEntry) {
	w.Header().Set("Content-Type", "text/html")

	if len(entries) == 0 {
		w.Write([]byte(`<div class="flex items-center justify-center h-full text-gray-500 dark:text-gray-400 text-sm">No log entries match the current filters.</div>`)) //nolint:errcheck // Best-effort write to HTTP response; client disconnect mid-write is not actionable
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

		// Prefer the component label (scanner, backup, watcher...) for the visible
		// column -- it is more human-readable than a filename. When no component is
		// set, fall back to the source filename without the line number. The full
		// file:line is kept in the tooltip so developers can still find the call site.
		display := entry.Component
		tooltip := entry.Source
		if display == "" {
			display = entry.Source
			if i := strings.LastIndex(display, ":"); i > 0 {
				display = display[:i]
			}
		}
		if display == "" {
			display = "-"
		}

		b.WriteString(`<div class="flex gap-2 text-xs font-mono py-0.5 border-b border-gray-800/30 log-line">`)
		fmt.Fprintf(&b, `<span class="text-gray-500 shrink-0 w-[11rem] whitespace-nowrap">%s</span>`, html.EscapeString(ts))
		fmt.Fprintf(&b, `<span class="px-1.5 rounded text-[10px] font-semibold uppercase shrink-0 w-12 text-center %s">%s</span>`,
			levelBadge, html.EscapeString(strings.ToUpper(entry.Level)))
		fmt.Fprintf(&b, `<span class="text-gray-400 shrink-0 w-32 truncate" title="%s">[%s]</span>`,
			html.EscapeString(tooltip), html.EscapeString(display))

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

	w.Write([]byte(b.String())) //nolint:errcheck // Best-effort write to HTTP response; client disconnect mid-write is not actionable
}

// logStreamBackfillLimit caps how many recent entries the stream replays from
// the ring buffer on connect (the ring holds up to DefaultRingBufferSize=2000).
const logStreamBackfillLimit = 500

// handleLogsStream serves the live log stream as Server-Sent Events. On connect
// it backfills recent ring-buffer entries (filtered, oldest-first, capped at
// logStreamBackfillLimit) then live-tails new records via the log broadcaster.
// Filters come from the query string: level (minimum severity), scope
// (component, exact match), and q (case-insensitive message substring). The
// browser EventSource's Last-Event-ID header (each frame's id is the entry's
// RFC3339Nano timestamp) acts as a resume cursor so a reconnect only replays
// entries newer than the last one seen.
//
// GET /api/v1/logs/stream
func (r *Router) handleLogsStream(w http.ResponseWriter, req *http.Request) {
	// SSE requires a flushable writer.
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming not supported"})
		return
	}

	if r.logManager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "logging manager not available"})
		return
	}
	rb := r.logManager.RingBuffer()
	lb := r.logManager.LogBroadcaster()
	if rb == nil || lb == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "log streaming not available"})
		return
	}

	// Parse and validate the filter before writing any stream headers so an
	// invalid request still gets a clean JSON 400.
	filter := logging.LogFilter{
		Search:    req.URL.Query().Get("q"),
		Component: req.URL.Query().Get("scope"),
	}
	if level := req.URL.Query().Get("level"); level != "" {
		if !logging.ValidLevel(level) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid level: must be one of trace, debug, info, warn, error"})
			return
		}
		filter.Level = level
	}

	// SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering

	// Clear the write deadline so this long-lived stream is not killed by the
	// server-level WriteTimeout.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		r.logger.Error("failed to clear write deadline for logs stream", "err", err)
	}

	// Subscribe BEFORE snapshotting the ring buffer so no record is lost in the
	// gap between backfill and live-tail. Records that arrive during backfill
	// queue on the subscription and are emitted after it; the newestTS cursor
	// returned below suppresses any that the snapshot already covered.
	sub := lb.Subscribe(filter)
	defer sub.Close()

	newestTS, ok := r.emitLogBackfill(w, flusher, req, rb, filter)
	if !ok {
		return // client disconnected during backfill
	}
	r.streamLogLines(w, flusher, req, sub, newestTS)
}

// emitLogBackfill writes the initial connection frame and replays recent
// ring-buffer entries (filtered, oldest-first) as logs.line events. A
// Last-Event-ID resume cursor narrows the replay to entries after the client's
// last-seen time. It returns the newest emitted timestamp (used to suppress
// duplicate live delivery) and false if a write failed (client disconnected).
func (r *Router) emitLogBackfill(w http.ResponseWriter, flusher http.Flusher, req *http.Request, rb *logging.RingBuffer, filter logging.LogFilter) (time.Time, bool) {
	backfill := filter
	backfill.Limit = logStreamBackfillLimit
	if cursor := lastEventIDFromRequest(req); cursor != "" {
		if t, err := time.Parse(time.RFC3339Nano, cursor); err == nil {
			backfill.After = t
		} else {
			// A malformed cursor must not silently replay the full window: log a
			// warning and degrade visibly. Streaming continues with the full
			// backfill, which is acceptable (the client simply re-sees recent
			// entries) but should never happen without a signal.
			r.logger.Warn("logs stream: unparsable Last-Event-ID cursor, replaying full backfill window", "cursor", cursor, "error", err)
		}
	}
	entries := rb.Entries(backfill)

	// Initial connection frame (no id, so it does not advance Last-Event-ID).
	if err := writeLogSSE(w, "", "connected", map[string]any{"replayed": len(entries)}, r.logger); err != nil {
		return time.Time{}, false
	}
	// Emit backfill oldest-first so the newest entry lands at the bottom, the
	// natural scroll direction for a log tail.
	var newestTS time.Time
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if err := writeLogSSE(w, e.Time.Format(time.RFC3339Nano), string(event.LogsLine), e, r.logger); err != nil {
			return time.Time{}, false
		}
		if e.Time.After(newestTS) {
			newestTS = e.Time
		}
	}
	flusher.Flush()
	return newestTS, true
}

// streamLogLines live-tails the subscription, writing each new entry as a
// logs.line event (suppressing any already covered by the backfill window),
// reporting buffer overflow as logs.throttled, and sending a heartbeat comment
// every 30 seconds. It returns when the client disconnects, a write fails, or
// the broadcaster closes the subscription.
func (r *Router) streamLogLines(w http.ResponseWriter, flusher http.Flusher, req *http.Request, sub *logging.Subscription, newestTS time.Time) {
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-req.Context().Done():
			return
		case <-sub.Throttle():
			// The subscriber buffer overflowed; report how many lines were shed
			// so the client can show a throttle banner.
			dropped := sub.DrainDropped()
			if dropped > 0 {
				if err := writeLogSSE(w, "", string(event.LogsThrottled),
					map[string]any{"dropped": dropped, "window": "live-buffer"}, r.logger); err != nil {
					return
				}
				flusher.Flush()
			}
		case e, ok := <-sub.Lines():
			if !ok {
				return // broadcaster closed the subscription
			}
			// Suppress entries already delivered in the backfill window. Because
			// the cursor is the entry timestamp, two distinct records sharing an
			// identical timestamp (one in backfill, one live) can suppress the
			// live one; clock resolution makes this rare and it is the inherent
			// cost of using the timestamp as the resume cursor.
			if !newestTS.IsZero() && !e.Time.After(newestTS) {
				continue
			}
			if err := writeLogSSE(w, e.Time.Format(time.RFC3339Nano), string(event.LogsLine), e, r.logger); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := w.Write([]byte(": heartbeat\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeLogSSE writes a single SSE frame for the log stream: an optional id line
// (omitted when id is empty so transport-only frames do not advance the
// client's Last-Event-ID), the event name, and a JSON data payload. A write
// error typically means the client disconnected; the caller returns on error.
func writeLogSSE(w http.ResponseWriter, id, eventType string, payload any, logger *slog.Logger) error {
	data, err := json.Marshal(payload)
	if err != nil {
		logger.Warn("logs sse marshal failed", "type", eventType, "error", err)
		return err
	}
	if id != "" {
		if _, err := w.Write([]byte("id: " + id + "\n")); err != nil {
			return err
		}
	}
	if _, err := w.Write([]byte("event: " + eventType + "\n")); err != nil {
		return err
	}
	if _, err := w.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err = w.Write([]byte("\n\n"))
	return err
}

// levelBadgeClass returns Tailwind classes for the log level badge.
func levelBadgeClass(level string) string {
	switch strings.ToLower(level) {
	case "trace":
		return "bg-purple-900/50 text-purple-300"
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
