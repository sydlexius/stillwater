package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/event"
)

// SSEEvent is a single server-sent event payload delivered to browser clients.
type SSEEvent struct {
	// Type is the SSE event name (e.g. "scan.completed", "rule.violation").
	Type string `json:"type"`
	// Title is a short human-readable summary for toast/notification display.
	Title string `json:"title"`
	// Message is the full notification body text.
	Message string `json:"message"`
	// Timestamp is when the event occurred.
	Timestamp time.Time `json:"timestamp"`
	// Data carries optional structured metadata about the event.
	Data map[string]any `json:"data,omitempty"`
}

// SSEClient represents a single connected SSE browser client.
type SSEClient struct {
	ch     chan SSEEvent
	userID string
}

// SSEHub manages connected SSE clients and fans out events from the event bus.
// It is safe for concurrent access from multiple goroutines.
type SSEHub struct {
	mu      sync.RWMutex
	clients map[*SSEClient]struct{}
	logger  *slog.Logger
}

// NewSSEHub creates a new SSE hub.
func NewSSEHub(logger *slog.Logger) *SSEHub {
	return &SSEHub{
		clients: make(map[*SSEClient]struct{}),
		logger:  logger,
	}
}

// Register adds a client to the hub and returns it. The caller must call
// Unregister when the client disconnects.
func (h *SSEHub) Register(userID string) *SSEClient {
	c := &SSEClient{
		// Buffer a few events so slow clients do not block the hub.
		ch:     make(chan SSEEvent, 16),
		userID: userID,
	}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	h.logger.Debug("sse client connected", "user_id", userID, "clients", h.ClientCount())
	return c
}

// Unregister removes a client from the hub and closes its channel.
func (h *SSEHub) Unregister(c *SSEClient) {
	h.mu.Lock()
	delete(h.clients, c)
	close(c.ch)
	h.mu.Unlock()
	h.logger.Debug("sse client disconnected", "user_id", c.userID, "clients", h.ClientCount())
}

// Broadcast sends an event to all connected clients. If a client's buffer is
// full the event is dropped for that client (non-blocking).
func (h *SSEHub) Broadcast(evt SSEEvent) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		select {
		case c.ch <- evt:
		default:
			h.logger.Warn("sse client buffer full, dropping event",
				"user_id", c.userID, "type", evt.Type)
		}
	}
}

// ClientCount returns the number of connected SSE clients.
func (h *SSEHub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// SubscribeToEventBus registers event bus handlers that convert internal events
// into SSE events and broadcast them to all connected clients.
func (h *SSEHub) SubscribeToEventBus(bus *event.Bus) {
	// Map internal event types to human-readable notification messages.
	// Each mapping has a buildMsg function that constructs a useful message
	// from the actual fields published in the event data. If buildMsg is nil
	// or returns an empty string, the title is used as the message.
	type eventMapping struct {
		eventType event.Type
		title     string
		buildMsg  func(data map[string]any) string
	}

	// strVal safely extracts a string value from the event data map.
	strVal := func(data map[string]any, key string) string {
		if v, ok := data[key]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
		return ""
	}

	// fmtInt safely formats an integer-like value from the event data map.
	fmtInt := func(data map[string]any, key string) string {
		if v, ok := data[key]; ok {
			switch n := v.(type) {
			case int:
				return fmt.Sprintf("%d", n)
			case int64:
				return fmt.Sprintf("%d", n)
			case float64:
				return fmt.Sprintf("%d", int64(n))
			}
		}
		return ""
	}

	mappings := []eventMapping{
		{event.ScanCompleted, "Scan completed", func(data map[string]any) string {
			status := strVal(data, "status")
			newArtists := fmtInt(data, "new_artists")
			totalDirs := fmtInt(data, "total_directories")
			if status != "" && newArtists != "" && totalDirs != "" {
				return "Scan " + status + ": " + newArtists + " new artists from " + totalDirs + " directories"
			}
			if status != "" {
				return "Scan " + status
			}
			return ""
		}},
		{event.RuleViolation, "New rule violation", func(data map[string]any) string {
			return strVal(data, "message")
		}},
		{event.BulkCompleted, "Bulk operation completed", func(data map[string]any) string {
			opType := strVal(data, "type")
			status := strVal(data, "status")
			if opType != "" && status != "" {
				return "Bulk " + opType + " " + status
			}
			if status != "" {
				return "Bulk operation " + status
			}
			return ""
		}},
		{event.ArtistNew, "New artist discovered", nil},
		{event.ArtistUpdated, "Artist updated", nil},
		{event.MetadataFixed, "Metadata fixed", func(data map[string]any) string {
			return strVal(data, "message")
		}},
	}

	for _, m := range mappings {
		m := m // capture loop variable
		bus.Subscribe(m.eventType, func(e event.Event) {
			msg := ""
			if m.buildMsg != nil {
				msg = m.buildMsg(e.Data)
			}
			if msg == "" {
				msg = m.title
			}
			h.Broadcast(SSEEvent{
				Type:      string(e.Type),
				Title:     m.title,
				Message:   msg,
				Timestamp: e.Timestamp,
				Data:      e.Data,
			})
		})
	}
}

// handleSSEStream serves the SSE endpoint. It keeps the connection open,
// sending events as they arrive and a heartbeat comment every 30 seconds.
//
// GET /api/v1/events/stream
func (r *Router) handleSSEStream(w http.ResponseWriter, req *http.Request) {
	// Verify the response writer supports flushing (required for SSE).
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "streaming not supported",
		})
		return
	}

	if r.sseHub == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "SSE not available",
		})
		return
	}

	// Get user ID from auth context for per-client tracking.
	userID := userIDFromRequest(req)

	// Set SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering

	// Clear the write deadline for this SSE stream so it is not subject to the
	// server-level http.Server.WriteTimeout. This allows the connection to stay
	// open indefinitely while we periodically send events/heartbeats.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		r.logger.Error("failed to clear write deadline for SSE stream", "err", err)
	}

	// Register this client with the hub.
	client := r.sseHub.Register(userID)

	// Ensure we unregister on disconnect.
	defer r.sseHub.Unregister(client)

	// Send initial connection confirmation.
	if err := writeSSEEvent(w, "connected", SSEEvent{
		Type:      "connected",
		Title:     "Connected",
		Message:   "SSE stream established",
		Timestamp: time.Now().UTC(),
	}, r.logger); err != nil {
		return
	}
	flusher.Flush()

	// Heartbeat ticker to keep the connection alive.
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-req.Context().Done():
			// Client disconnected.
			return
		case evt, ok := <-client.ch:
			if !ok {
				// Channel closed (hub shut down).
				return
			}
			if err := writeSSEEvent(w, evt.Type, evt, r.logger); err != nil {
				// Write failed -- client likely disconnected.
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			// Send a comment line as keepalive.
			_, err := w.Write([]byte(": heartbeat\n\n"))
			if err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeSSEEvent writes a single SSE event to the response writer.
// Returns an error if JSON marshaling or writing fails. Write errors
// typically indicate the client has disconnected.
func writeSSEEvent(w http.ResponseWriter, eventType string, evt SSEEvent, logger *slog.Logger) error {
	data, err := json.Marshal(evt)
	if err != nil {
		logger.Warn("sse event marshal failed", "type", eventType, "error", err)
		return err
	}
	// SSE format: "event: <type>\ndata: <json>\n\n"
	if _, err := w.Write([]byte("event: " + eventType + "\n")); err != nil {
		return err
	}
	if _, err := w.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	if _, err := w.Write([]byte("\n\n")); err != nil {
		return err
	}
	return nil
}

// userIDFromRequest extracts the user ID from the request context.
// Returns "anonymous" if no user is authenticated (should not happen
// behind auth middleware, but defensive).
func userIDFromRequest(req *http.Request) string {
	if id := middleware.UserIDFromContext(req.Context()); id != "" {
		return id
	}
	return "anonymous"
}
