package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/event"
)

// Replay buffer defaults: the hub retains recent broadcasts so a client that
// reconnects with a Last-Event-ID header can be caught up without losing
// events. Bounded by whichever limit is hit first (count or age).
const (
	defaultSSEBufferMaxEvents = 1000
	defaultSSEBufferTTL       = 5 * time.Minute
)

// SSEEvent is a single server-sent event payload delivered to browser clients.
type SSEEvent struct {
	// ID is the monotonic event id assigned by the hub on broadcast. It is
	// emitted as the SSE `id:` field so the browser EventSource echoes it back
	// as the Last-Event-ID header on reconnect, driving replay. Empty for
	// transport-only frames (the initial "connected" event) that must not
	// advance the client's last-seen id.
	ID string `json:"id,omitempty"`
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

// bufferedSSEEvent is one entry in the hub's replay ring buffer: the broadcast
// event plus the id and wall-clock time used for replay and TTL eviction.
type bufferedSSEEvent struct {
	id  uint64
	at  time.Time
	evt SSEEvent
}

// SSEHub manages connected SSE clients and fans out events from the event bus.
// It is safe for concurrent access from multiple goroutines.
type SSEHub struct {
	mu      sync.RWMutex
	clients map[*SSEClient]struct{}
	logger  *slog.Logger

	// nextID is the monotonic counter stamped onto each broadcast event.
	nextID uint64
	// buffer is the replay ring. Entries from head to the end are "live"
	// (within the count + TTL bounds); entries before head are dead and
	// reclaimed on the next compaction.
	buffer []bufferedSSEEvent
	head   int
	// bufMax / bufTTL bound the live window; now is injectable for tests.
	bufMax int
	bufTTL time.Duration
	now    func() time.Time
}

// NewSSEHub creates a new SSE hub.
func NewSSEHub(logger *slog.Logger) *SSEHub {
	return &SSEHub{
		clients: make(map[*SSEClient]struct{}),
		logger:  logger,
		bufMax:  defaultSSEBufferMaxEvents,
		bufTTL:  defaultSSEBufferTTL,
		now:     func() time.Time { return time.Now().UTC() },
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

// Broadcast assigns the event a monotonic id, records it in the replay buffer,
// and fans it out to all connected clients. If a client's buffer is full the
// event is dropped for that client (non-blocking) -- but it stays in the replay
// buffer, so a client that reconnects can still recover it via Last-Event-ID.
func (h *SSEHub) Broadcast(evt SSEEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.nextID++
	evt.ID = strconv.FormatUint(h.nextID, 10)
	h.recordEvent(evt, h.nextID, h.now())

	for c := range h.clients {
		select {
		case c.ch <- evt:
		default:
			h.logger.Warn("sse client buffer full, dropping event",
				"user_id", c.userID, "type", evt.Type)
		}
	}
}

// recordEvent appends e to the replay buffer and evicts entries older than
// bufTTL or beyond bufMax. head marks the oldest live entry; the backing slice
// is only physically compacted once the dead prefix grows to dominate it, which
// keeps Broadcast amortized O(1) rather than reallocating on every event.
// Callers must hold h.mu.
func (h *SSEHub) recordEvent(e SSEEvent, id uint64, at time.Time) {
	h.buffer = append(h.buffer, bufferedSSEEvent{id: id, at: at, evt: e})

	// TTL eviction: advance past entries older than the retention window.
	for h.head < len(h.buffer) && at.Sub(h.buffer[h.head].at) > h.bufTTL {
		h.head++
	}
	// Count eviction: keep at most bufMax live entries.
	if live := len(h.buffer) - h.head; live > h.bufMax {
		h.head += live - h.bufMax
	}
	// Compact when the dead prefix is at least as large as the live tail,
	// bounding the backing array to roughly 2*bufMax and letting the GC
	// reclaim evicted events.
	if h.head > 0 && h.head >= len(h.buffer)-h.head {
		h.buffer = append([]bufferedSSEEvent(nil), h.buffer[h.head:]...)
		h.head = 0
	}
}

// Replay returns the buffered events a reconnecting client missed, given the
// Last-Event-ID it last saw. boundary is the highest id the hub has assigned so
// far; the caller uses it to suppress duplicate live delivery of events that
// replay already covered. complete is false when the requested id is no longer
// recoverable from the buffer (evicted, never issued, or unparsable) -- the
// client should then refetch derived state instead of trusting replay.
func (h *SSEHub) Replay(lastEventID string) (events []SSEEvent, boundary uint64, complete bool) {
	id, err := strconv.ParseUint(lastEventID, 10, 64)
	if err != nil {
		return nil, 0, false
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	boundary = h.nextID
	// Apply the TTL cutoff at read time as well: head only advances on
	// broadcast (recordEvent), so during an idle period a reconnect could
	// otherwise be handed events older than the retention window.
	start := h.head
	now := h.now()
	for start < len(h.buffer) && now.Sub(h.buffer[start].at) > h.bufTTL {
		start++
	}
	live := h.buffer[start:]
	if len(live) == 0 {
		// Nothing live: the client is current only if it already saw the
		// latest id we assigned (anything older fell outside the window).
		return nil, boundary, id == h.nextID
	}

	oldest := live[0].id
	newest := live[len(live)-1].id
	switch {
	case id > newest:
		// Client claims an id we never issued (server restart / counter
		// reset): treat as loss.
		return nil, boundary, false
	case id+1 < oldest:
		// The event after the client's last-seen id was evicted: gap.
		return nil, boundary, false
	}

	for i := range live {
		if live[i].id > id {
			events = append(events, live[i].evt)
		}
	}
	return events, boundary, true
}

// ClientCount returns the number of connected SSE clients.
func (h *SSEHub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// sseEventMapping pairs an internal event type with the title and message
// builder used to project it into an SSEEvent. Promoted from a local
// struct so the named buildMsg helpers below can declare their argument
// type without re-binding to a closure.
type sseEventMapping struct {
	eventType event.Type
	title     string
	buildMsg  func(data map[string]any) string
}

// strVal safely extracts a string value from an event data map. Promoted
// from a closure inside SubscribeToEventBus so the file-level buildMsg
// helpers can share it; this is the only shape used by every consumer of
// event.Event.Data in this file.
func strVal(data map[string]any, key string) string {
	if v, ok := data[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// fmtInt safely formats an integer-like value from an event data map.
// Promoted from a closure for the same reason as strVal above. JSON
// round-trips through float64, so the float branch covers events that
// have already passed through the SSE marshal/unmarshal hop.
func fmtInt(data map[string]any, key string) string {
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

// buildScanCompletedMsg formats a scan-completed event into a human-readable
// summary. Extracted from an inline closure to keep SubscribeToEventBus
// under the gocognit threshold (was 34, lint cap is 30).
func buildScanCompletedMsg(data map[string]any) string {
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
}

// buildRuleViolationMsg lifts the single-field projection out so the
// mappings slice in SubscribeToEventBus stays flat.
func buildRuleViolationMsg(data map[string]any) string {
	return strVal(data, "message")
}

// buildBulkCompletedMsg formats a bulk-operation summary. Extracted to
// drop gocognit complexity.
func buildBulkCompletedMsg(data map[string]any) string {
	opType := strVal(data, "type")
	status := strVal(data, "status")
	if opType != "" && status != "" {
		return "Bulk " + opType + " " + status
	}
	if status != "" {
		return "Bulk operation " + status
	}
	return ""
}

// buildMetadataFixedMsg lifts the single-field projection out.
func buildMetadataFixedMsg(data map[string]any) string {
	return strVal(data, "message")
}

// buildConflictChangedMsg formats the banner-state transition into a
// human-readable line for the toast fallback.
func buildConflictChangedMsg(data map[string]any) string {
	state := strVal(data, "banner_state")
	if state == "" {
		return ""
	}
	return "Conflict banner: " + state
}

// buildOperationProgressMsg exposes the label string for clients that
// render OperationProgress as a plain toast. The pill consumer reads the
// structured Data fields directly and ignores this message.
func buildOperationProgressMsg(data map[string]any) string {
	return strVal(data, "label")
}

// buildConnectionPushFailedMsg names the connection, the short error
// class, and (when present) the originating artist so the operator can
// tell a single failure apart from a fan-out. Extracted to drop gocognit
// complexity in SubscribeToEventBus.
func buildConnectionPushFailedMsg(data map[string]any) string {
	conn := strVal(data, "connection")
	class := strVal(data, "error_class")
	artistName := strVal(data, "artist_name")
	var base string
	switch {
	case conn != "" && class != "":
		base = conn + ": " + class
	case conn != "":
		base = conn + ": push failed"
	default:
		return "A platform push failed"
	}
	if artistName != "" {
		return base + " (artist: " + artistName + ")"
	}
	return base
}

// buildActivityRecentMsg surfaces the human-readable text of a recent-activity
// item for the next dashboard's live rail. The rail consumes the structured
// Data directly; this message is the plain-toast fallback.
func buildActivityRecentMsg(data map[string]any) string {
	return strVal(data, "text")
}

// SubscribeToEventBus registers event bus handlers that convert internal events
// into SSE events and broadcast them to all connected clients.
//
// The mappings slice is flat by design: every per-event message builder
// lives at file scope (buildScanCompletedMsg etc.) so this function stays
// under the gocognit lint threshold. Adding a new event type means
// declaring a build*Msg helper and appending one entry below.
func (h *SSEHub) SubscribeToEventBus(bus *event.Bus) {
	mappings := []sseEventMapping{
		{event.ScanCompleted, "Scan completed", buildScanCompletedMsg},
		{event.RuleViolation, "New rule violation", buildRuleViolationMsg},
		{event.BulkCompleted, "Bulk operation completed", buildBulkCompletedMsg},
		{event.ArtistNew, "New artist discovered", nil},
		{event.ArtistUpdated, "Artist updated", nil},
		{event.MetadataFixed, "Metadata fixed", buildMetadataFixedMsg},
		{event.ConflictChanged, "Conflict state changed", buildConflictChangedMsg},
		// OperationProgress carries its own structured fields (op_id, label,
		// processed, total, status) for the ProgressPill renderer. The
		// Title/Message text is only used as a fallback for clients that
		// surface unknown events as plain toasts.
		{event.OperationProgress, "Operation progress", buildOperationProgressMsg},
		// ConnectionPushFailed Data carries the structured connection +
		// error_class + artist_name fields; the raw transport error is
		// deliberately NOT in Data. See internal/publish/notifier.go.
		{event.ConnectionPushFailed, "Platform push failed", buildConnectionPushFailedMsg},
		// M55 next-channel events. These are low-volume cross-tab / dashboard
		// signals; their consumers read the structured Data, so the Title is
		// only a plain-toast fallback. logs.line / logs.throttled are NOT here
		// (emitted on the dedicated logs stream in #1338).
		{event.ActivityRecent, "Recent activity", buildActivityRecentMsg},
		{event.SettingsChanged, "Settings changed", nil},
		{event.DashboardActionResolved, "Action resolved", nil},
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

// emitActionResolved re-emits the dashboard action-resolved signal on the SSE
// bus so the action-queue badge updates across all open tabs. The originating
// handler still sets the "dashboard:action-resolved" HX-Trigger header for the
// same-tab HTMX update; this is the cross-tab counterpart. Safe to call with a
// nil bus (no-op).
func (r *Router) emitActionResolved() {
	if r.eventBus == nil {
		return
	}
	r.eventBus.Publish(event.Event{Type: event.DashboardActionResolved})
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

	// Register this client with the hub. Register BEFORE computing replay so
	// that any event broadcast during replay still lands on the live channel
	// (no gap); the replay boundary then suppresses the duplicate.
	client := r.sseHub.Register(userID)

	// Ensure we unregister on disconnect.
	defer r.sseHub.Unregister(client)

	// Replay any events missed since the client's last-seen id. The browser
	// EventSource sends Last-Event-ID automatically on reconnect; a query
	// param fallback keeps the endpoint testable and usable by non-browser
	// clients. boundary dedupes the live channel below; bufferLoss tells the
	// client replay could not bridge the gap so it should refetch state.
	var replay []SSEEvent
	var boundary uint64
	bufferLoss := false
	if lastEventID := lastEventIDFromRequest(req); lastEventID != "" {
		var complete bool
		replay, boundary, complete = r.sseHub.Replay(lastEventID)
		bufferLoss = !complete
	}

	// Send initial connection confirmation. The connected frame carries no
	// `id:` (empty SSEEvent.ID) so it never advances the client's last-seen id.
	if err := writeSSEEvent(w, "connected", SSEEvent{
		Type:      "connected",
		Title:     "Connected",
		Message:   "SSE stream established",
		Timestamp: time.Now().UTC(),
		Data: map[string]any{
			"replayed":   len(replay),
			"bufferLoss": bufferLoss,
		},
	}, r.logger); err != nil {
		return
	}
	// Replay missed events; each retains its original id and type.
	for _, evt := range replay {
		if err := writeSSEEvent(w, evt.Type, evt, r.logger); err != nil {
			return
		}
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
			// Skip events already delivered via replay (id <= boundary). This
			// dedupes the window between Register and Replay, where an event
			// can land on both the live channel and the replay set.
			if boundary > 0 && evt.ID != "" {
				if eid, err := strconv.ParseUint(evt.ID, 10, 64); err == nil && eid <= boundary {
					continue
				}
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
	// SSE format: "id: <id>\nevent: <type>\ndata: <json>\n\n". The id line is
	// emitted only when the event carries one, so transport-only frames (the
	// connected handshake) do not advance the browser's Last-Event-ID.
	if evt.ID != "" {
		if _, err := w.Write([]byte("id: " + evt.ID + "\n")); err != nil {
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
	if _, err := w.Write([]byte("\n\n")); err != nil {
		return err
	}
	return nil
}

// lastEventIDFromRequest returns the client's last-seen SSE event id. Browsers
// send it via the Last-Event-ID header on automatic reconnect; the
// last_event_id query param is a fallback for non-browser clients and tests.
func lastEventIDFromRequest(req *http.Request) string {
	if id := req.Header.Get("Last-Event-ID"); id != "" {
		return id
	}
	return req.URL.Query().Get("last_event_id")
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
