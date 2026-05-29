package api

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/event"
)

func TestSSEHub_RegisterUnregister(t *testing.T) {
	t.Parallel()
	hub := NewSSEHub(slog.Default())

	c1 := hub.Register("user-1")
	c2 := hub.Register("user-2")

	if hub.ClientCount() != 2 {
		t.Errorf("expected 2 clients, got %d", hub.ClientCount())
	}

	hub.Unregister(c1)
	if hub.ClientCount() != 1 {
		t.Errorf("expected 1 client after unregister, got %d", hub.ClientCount())
	}

	hub.Unregister(c2)
	if hub.ClientCount() != 0 {
		t.Errorf("expected 0 clients after unregister, got %d", hub.ClientCount())
	}
}

func TestSSEHub_Broadcast(t *testing.T) {
	t.Parallel()
	hub := NewSSEHub(slog.Default())

	c1 := hub.Register("user-1")
	c2 := hub.Register("user-2")
	defer hub.Unregister(c1)
	defer hub.Unregister(c2)

	evt := SSEEvent{
		Type:      "scan.completed",
		Title:     "Scan completed",
		Message:   "Library scan finished with 42 artists",
		Timestamp: time.Now().UTC(),
	}

	hub.Broadcast(evt)

	// Both clients should receive the event.
	select {
	case got := <-c1.ch:
		if got.Type != "scan.completed" {
			t.Errorf("c1 got type %q, want scan.completed", got.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("c1 did not receive event within timeout")
	}

	select {
	case got := <-c2.ch:
		if got.Type != "scan.completed" {
			t.Errorf("c2 got type %q, want scan.completed", got.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("c2 did not receive event within timeout")
	}
}

func TestSSEHub_BroadcastDropsWhenBufferFull(t *testing.T) {
	t.Parallel()
	hub := NewSSEHub(slog.Default())
	c := hub.Register("user-1")
	defer hub.Unregister(c)

	// Fill the client buffer (capacity 16).
	for i := 0; i < 16; i++ {
		hub.Broadcast(SSEEvent{Type: "test", Message: "fill"})
	}

	// This should not block -- it should be dropped.
	hub.Broadcast(SSEEvent{Type: "test", Message: "overflow"})

	// Drain and verify we got exactly 16.
	count := 0
	for {
		select {
		case <-c.ch:
			count++
		default:
			goto done
		}
	}
done:
	if count != 16 {
		t.Errorf("expected 16 events (buffer capacity), got %d", count)
	}
}

func TestSSEHub_SubscribeToEventBus(t *testing.T) {
	t.Parallel()
	logger := slog.Default()
	hub := NewSSEHub(logger)
	bus := event.NewBus(logger, 64)

	hub.SubscribeToEventBus(bus)

	c := hub.Register("user-1")
	defer hub.Unregister(c)

	// Start the bus.
	go bus.Start()
	defer bus.Stop()

	// Publish a scan completed event with the same data shape the scanner uses.
	bus.Publish(event.Event{
		Type: event.ScanCompleted,
		Data: map[string]any{
			"scan_id":           "abc-123",
			"status":            "completed",
			"total_directories": 50,
			"new_artists":       3,
		},
	})

	// Wait for the event to propagate through bus -> hub -> client.
	select {
	case got := <-c.ch:
		if got.Type != string(event.ScanCompleted) {
			t.Errorf("got type %q, want %q", got.Type, string(event.ScanCompleted))
		}
		if got.Title != "Scan completed" {
			t.Errorf("got title %q, want %q", got.Title, "Scan completed")
		}
		wantMsg := "Scan completed: 3 new artists from 50 directories"
		if got.Message != wantMsg {
			t.Errorf("got message %q, want %q", got.Message, wantMsg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive event within timeout")
	}
}

// TestSSEHub_BroadcastAssignsMonotonicIDs verifies every broadcast event is
// stamped with a distinct, increasing decimal id. The id is what the browser
// EventSource echoes back as the Last-Event-ID header on reconnect, so it must
// be present and strictly increasing for replay to work.
func TestSSEHub_BroadcastAssignsMonotonicIDs(t *testing.T) {
	t.Parallel()
	hub := NewSSEHub(slog.Default())
	c := hub.Register("u1")
	defer hub.Unregister(c)

	hub.Broadcast(SSEEvent{Type: "test"})
	hub.Broadcast(SSEEvent{Type: "test"})

	first := <-c.ch
	second := <-c.ch

	if first.ID != "1" {
		t.Errorf("first event ID = %q, want %q", first.ID, "1")
	}
	if second.ID != "2" {
		t.Errorf("second event ID = %q, want %q", second.ID, "2")
	}
}

// TestSSEHub_ReplayReturnsEventsAfterLastID verifies a client that reconnects
// with a Last-Event-ID inside the buffer window is handed exactly the events it
// missed, in order, plus the boundary id used to dedupe live delivery.
func TestSSEHub_ReplayReturnsEventsAfterLastID(t *testing.T) {
	t.Parallel()
	hub := NewSSEHub(slog.Default())
	for i := 0; i < 5; i++ {
		hub.Broadcast(SSEEvent{Type: "test"})
	}

	events, boundary, complete := hub.Replay("2")
	if !complete {
		t.Fatal("expected complete replay, got buffer loss")
	}
	if boundary != 5 {
		t.Errorf("boundary = %d, want 5", boundary)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 replayed events (ids 3,4,5), got %d", len(events))
	}
	if events[0].ID != "3" || events[2].ID != "5" {
		t.Errorf("replayed ids = %q..%q, want 3..5", events[0].ID, events[2].ID)
	}
}

// TestSSEHub_ReplayDetectsBufferLoss verifies that a Last-Event-ID older than
// the retained window reports buffer loss (so the client refetches derived
// state), while a client already at the newest id replays nothing and is
// considered current.
func TestSSEHub_ReplayDetectsBufferLoss(t *testing.T) {
	t.Parallel()
	hub := NewSSEHub(slog.Default())
	hub.bufMax = 3 // force eviction of the oldest events
	for i := 0; i < 10; i++ {
		hub.Broadcast(SSEEvent{Type: "test"})
	}

	// Client last saw id 2, but only ids 8,9,10 remain buffered -> gap.
	if _, _, complete := hub.Replay("2"); complete {
		t.Error("expected buffer loss for evicted id, got complete replay")
	}

	// A client already at the newest id is current: complete, no events.
	events, _, complete := hub.Replay("10")
	if !complete {
		t.Error("expected complete replay for a current client")
	}
	if len(events) != 0 {
		t.Errorf("expected no replay for a current client, got %d", len(events))
	}
}

// TestSSEHub_ReplayRejectsUnparsableID treats a malformed Last-Event-ID as
// buffer loss rather than guessing, so a corrupt header forces a clean refetch.
func TestSSEHub_ReplayRejectsUnparsableID(t *testing.T) {
	t.Parallel()
	hub := NewSSEHub(slog.Default())
	hub.Broadcast(SSEEvent{Type: "test"})
	if _, _, complete := hub.Replay("not-a-number"); complete {
		t.Error("expected buffer loss for an unparsable Last-Event-ID")
	}
}

// TestHandleSSEStream_ReplaysFromLastEventID verifies the stream handler reads
// the Last-Event-ID request header and replays the buffered events the client
// missed, each carrying its `id:` frame.
func TestHandleSSEStream_ReplaysFromLastEventID(t *testing.T) {
	t.Parallel()
	logger := slog.Default()
	hub := NewSSEHub(logger)
	r := &Router{sseHub: hub, logger: logger}

	// Buffer three events before any client connects.
	for i := 0; i < 3; i++ {
		hub.Broadcast(SSEEvent{Type: "scan.completed", Message: "done"})
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := middleware.WithTestUserID(req.Context(), "test-user")
		r.handleSSEStream(w, req.WithContext(ctx))
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	httpReq.Header.Set("Last-Event-ID", "1") // client last saw event 1

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var got []string
	sawID2, sawID3 := false, false
	for scanner.Scan() {
		line := scanner.Text()
		got = append(got, line)
		if line == "id: 2" {
			sawID2 = true
		}
		if line == "id: 3" {
			sawID3 = true
		}
		if sawID2 && sawID3 {
			break
		}
	}
	if !sawID2 || !sawID3 {
		t.Errorf("expected replay of ids 2 and 3, got frames:\n%s", strings.Join(got, "\n"))
	}
}

// TestSSEHub_BroadcastsNextChannelEvents verifies the new next-channel event
// types defined for M55 are mapped through SubscribeToEventBus and reach
// connected clients with their structured Data preserved. These are the
// cross-tab / dashboard events that flow through the main events stream;
// logs.line / logs.throttled are deliberately NOT mapped here because they are
// emitted on the dedicated logs stream (#1338).
func TestSSEHub_BroadcastsNextChannelEvents(t *testing.T) {
	t.Parallel()
	logger := slog.Default()
	hub := NewSSEHub(logger)
	bus := event.NewBus(logger, 64)
	hub.SubscribeToEventBus(bus)

	c := hub.Register("u1")
	defer hub.Unregister(c)

	go bus.Start()
	defer bus.Stop()

	cases := []struct {
		typ  event.Type
		data map[string]any
	}{
		{event.SettingsChanged, map[string]any{"sectionId": "preferences", "updatedBy": "u1"}},
		{event.DashboardActionResolved, map[string]any{}},
		{event.ActivityRecent, map[string]any{"kind": "scan", "text": "Scan finished"}},
	}
	for _, tc := range cases {
		bus.Publish(event.Event{Type: tc.typ, Data: tc.data})
	}

	got := map[string]bool{}
	for range cases {
		select {
		case e := <-c.ch:
			got[e.Type] = true
		case <-time.After(2 * time.Second):
			t.Fatal("did not receive all next-channel events within timeout")
		}
	}
	for _, tc := range cases {
		if !got[string(tc.typ)] {
			t.Errorf("missing broadcast for event type %q", tc.typ)
		}
	}
}

// TestSSEHub_ConcurrentBroadcastReplay hammers Broadcast (write lock + buffer
// mutation), Replay (read lock + buffer read), and client churn concurrently so
// the race detector can catch any unsynchronized access to the replay buffer.
func TestSSEHub_ConcurrentBroadcastReplay(t *testing.T) {
	t.Parallel()
	hub := NewSSEHub(slog.Default())
	hub.bufMax = 50 // small window so eviction/compaction runs under contention

	reader := hub.Register("reader")
	// Drain the reader so Broadcast never blocks on a full client buffer; the
	// range ends when Unregister closes reader.ch after the workload.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range reader.ch {
		}
	}()

	const iters = 2000
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				hub.Broadcast(SSEEvent{Type: "x"})
			}
		}()
	}
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				hub.Replay("10")
				_ = hub.ClientCount()
			}
		}()
	}
	wg.Wait()

	hub.Unregister(reader)
	<-drained
}

func TestHandleSSEStream(t *testing.T) {
	t.Parallel()
	logger := slog.Default()
	hub := NewSSEHub(logger)

	r := &Router{
		sseHub: hub,
		logger: logger,
	}

	// Create a test server that wraps the handler with a fake auth context.
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := middleware.WithTestUserID(req.Context(), "test-user")
		r.handleSSEStream(w, req.WithContext(ctx))
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Connect to the SSE stream with a context that we can cancel.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("expected Content-Type text/event-stream, got %q", ct)
	}

	// Read the initial "connected" event.
	scanner := bufio.NewScanner(resp.Body)
	var eventLines []string
	for scanner.Scan() {
		line := scanner.Text()
		eventLines = append(eventLines, line)
		// SSE events are terminated by an empty line.
		if line == "" && len(eventLines) > 1 {
			break
		}
	}

	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error reading connected event: %v", err)
	}

	// Verify we got the connected event.
	joined := strings.Join(eventLines, "\n")
	if !strings.Contains(joined, "event: connected") {
		t.Errorf("expected connected event, got: %s", joined)
	}

	// Broadcast an event and verify we receive it.
	hub.Broadcast(SSEEvent{
		Type:      "scan.completed",
		Title:     "Scan completed",
		Message:   "Test scan done",
		Timestamp: time.Now().UTC(),
	})

	eventLines = nil
	for scanner.Scan() {
		line := scanner.Text()
		eventLines = append(eventLines, line)
		if line == "" && len(eventLines) > 1 {
			break
		}
	}

	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error reading scan.completed event: %v", err)
	}

	joined = strings.Join(eventLines, "\n")
	if !strings.Contains(joined, "event: scan.completed") {
		t.Errorf("expected scan.completed event, got: %s", joined)
	}

	// Verify the data payload is valid JSON.
	for _, line := range eventLines {
		if strings.HasPrefix(line, "data: ") {
			dataStr := strings.TrimPrefix(line, "data: ")
			var evt SSEEvent
			if err := json.Unmarshal([]byte(dataStr), &evt); err != nil {
				t.Errorf("invalid JSON in data line: %v", err)
			}
			if evt.Message != "Test scan done" {
				t.Errorf("expected message 'Test scan done', got %q", evt.Message)
			}
		}
	}
}

func TestHandleSSEStream_NoHub(t *testing.T) {
	t.Parallel()
	r := &Router{
		sseHub: nil,
		logger: slog.Default(),
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events/stream", nil)
	r.handleSSEStream(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

// TestSSEHub_BroadcastsOperationProgress verifies the new OperationProgress
// event type is mapped through SubscribeToEventBus and reaches connected
// clients with the structured Data payload the ProgressPill renderer
// expects (op_id, label, processed, total, status, cancel_url all
// preserved verbatim in the SSEEvent.Data map).
func TestSSEHub_BroadcastsOperationProgress(t *testing.T) {
	t.Parallel()
	logger := slog.Default()
	hub := NewSSEHub(logger)
	bus := event.NewBus(logger, 64)
	hub.SubscribeToEventBus(bus)

	c := hub.Register("user-1")
	defer hub.Unregister(c)

	go bus.Start()
	defer bus.Stop()

	bus.Publish(event.Event{
		Type: event.OperationProgress,
		Data: map[string]any{
			"op_id":      "bulk_action",
			"label":      "run_rules",
			"processed":  5,
			"total":      10,
			"status":     "running",
			"cancel_url": "/api/v1/artists/bulk-actions/cancel",
		},
	})

	select {
	case got := <-c.ch:
		if got.Type != string(event.OperationProgress) {
			t.Errorf("got type %q, want %q", got.Type, string(event.OperationProgress))
		}
		// Data must round-trip so the JS renderer can read every field
		// it needs to render a pill (op_id is the dedupe key, cancel_url
		// drives the Cancel button visibility).
		if got.Data["op_id"] != "bulk_action" {
			t.Errorf("op_id = %v, want bulk_action", got.Data["op_id"])
		}
		if got.Data["label"] != "run_rules" {
			t.Errorf("label = %v, want run_rules", got.Data["label"])
		}
		if got.Data["status"] != "running" {
			t.Errorf("status = %v, want running", got.Data["status"])
		}
		if got.Data["cancel_url"] != "/api/v1/artists/bulk-actions/cancel" {
			t.Errorf("cancel_url = %v, want /api/v1/artists/bulk-actions/cancel", got.Data["cancel_url"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive operation.progress event within timeout")
	}
}

// TestSSEHub_BroadcastsConnectionPushFailed verifies the new
// ConnectionPushFailed event type renders a toast-shaped SSEEvent that
// names the connection and an error class. The publish package fires
// this from PushLocks goroutine errors; the SSE hub is the public
// surface, so the test mirrors the same event shape.
func TestSSEHub_BroadcastsConnectionPushFailed(t *testing.T) {
	t.Parallel()
	logger := slog.Default()
	hub := NewSSEHub(logger)
	bus := event.NewBus(logger, 64)
	hub.SubscribeToEventBus(bus)

	c := hub.Register("user-1")
	defer hub.Unregister(c)

	go bus.Start()
	defer bus.Stop()

	bus.Publish(event.Event{
		Type: event.ConnectionPushFailed,
		Data: map[string]any{
			"connection":  "my-emby",
			"error_class": "auth_failed",
			"artist_id":   "a1",
			"artist_name": "Pink Floyd",
			"operation":   "lock_toggle",
		},
	})

	select {
	case got := <-c.ch:
		if got.Type != string(event.ConnectionPushFailed) {
			t.Errorf("got type %q, want %q", got.Type, string(event.ConnectionPushFailed))
		}
		// Message must name connection + class + artist so an operator
		// can distinguish a single failure from a same-artist fan-out.
		wantMsg := "my-emby: auth_failed (artist: Pink Floyd)"
		if got.Message != wantMsg {
			t.Errorf("got message %q, want %q", got.Message, wantMsg)
		}
		if got.Data["connection"] != "my-emby" {
			t.Errorf("connection = %v, want my-emby", got.Data["connection"])
		}
		if got.Data["artist_name"] != "Pink Floyd" {
			t.Errorf("artist_name = %v, want Pink Floyd", got.Data["artist_name"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive connection.push_failed event within timeout")
	}
}
