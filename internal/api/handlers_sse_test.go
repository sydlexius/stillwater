package api

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/event"
)

func TestSSEHub_RegisterUnregister(t *testing.T) {
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

func TestHandleSSEStream(t *testing.T) {
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

	httpReq, err := http.NewRequestWithContext(ctx, "GET", ts.URL, nil)
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
	r := &Router{
		sseHub: nil,
		logger: slog.Default(),
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/events/stream", nil)
	r.handleSSEStream(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}
