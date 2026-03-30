package api

import (
	"encoding/json"
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
	type eventMapping struct {
		eventType event.Type
		title     string
		msgKey    string // key in event.Data to use as message body, empty = use title
	}

	mappings := []eventMapping{
		{event.ScanCompleted, "Scan completed", "summary"},
		{event.RuleViolation, "New rule violation", "message"},
		{event.BulkCompleted, "Bulk operation completed", "summary"},
		{event.ArtistNew, "New artist discovered", "name"},
		{event.ArtistUpdated, "Artist updated", "name"},
		{event.MetadataFixed, "Metadata fixed", "message"},
	}

	for _, m := range mappings {
		m := m // capture loop variable
		bus.Subscribe(m.eventType, func(e event.Event) {
			msg := m.title
			if m.msgKey != "" {
				if v, ok := e.Data[m.msgKey]; ok {
					if s, ok := v.(string); ok && s != "" {
						msg = s
					}
				}
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

	// Register this client with the hub.
	client := r.sseHub.Register(userID)

	// Ensure we unregister on disconnect.
	defer r.sseHub.Unregister(client)

	// Send initial connection confirmation.
	writeSSEEvent(w, "connected", SSEEvent{
		Type:      "connected",
		Title:     "Connected",
		Message:   "SSE stream established",
		Timestamp: time.Now().UTC(),
	})
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
			writeSSEEvent(w, evt.Type, evt)
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
func writeSSEEvent(w http.ResponseWriter, eventType string, evt SSEEvent) {
	data, err := json.Marshal(evt)
	if err != nil {
		return
	}
	// SSE format: "event: <type>\ndata: <json>\n\n"
	if _, err := w.Write([]byte("event: " + eventType + "\n")); err != nil {
		return
	}
	if _, err := w.Write([]byte("data: ")); err != nil {
		return
	}
	if _, err := w.Write(data); err != nil {
		return
	}
	_, _ = w.Write([]byte("\n\n"))
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
