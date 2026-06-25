package publish

import (
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/event"
)

// TestBusNotifier_PublishesConnectionPushFailed verifies the adapter
// translates a NotifyConnectionPushFailed call into a published
// event.ConnectionPushFailed event with the connection name, error
// class, and error string preserved. The SSE hub subscribes to that
// event type and renders the toast, so any drift here silently drops
// the operator notification.
func TestBusNotifier_PublishesConnectionPushFailed(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := event.NewBus(logger, 8)

	// Subscribe before Publish; the bus drops events on a full buffer
	// but the test sub is synchronous from dispatch's perspective and
	// the buffer is generous, so a single event is delivered.
	var (
		mu       sync.Mutex
		received []event.Event
	)
	bus.Subscribe(event.ConnectionPushFailed, func(e event.Event) {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, e)
	})
	go bus.Start()
	defer bus.Stop()

	n := NewBusNotifier(bus)
	// The raw err (which may carry a URL like "Post http://emby.lan/..."
	// or a 5xx response body) must NOT appear on the published event.
	n.NotifyConnectionPushFailed("conn-uuid-1", "my-emby", "auth_failed", "a1", "Test Artist", "lock_toggle",
		errors.New("Post \"http://emby.internal.lan:8096/Items/p1\": HTTP 401 Unauthorized"))

	// Wait briefly for the event to drain through the bus's worker.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(received)
		mu.Unlock()
		if count > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("received events = %d, want 1", len(received))
	}
	e := received[0]
	if e.Type != event.ConnectionPushFailed {
		t.Errorf("type = %q, want %q", e.Type, event.ConnectionPushFailed)
	}
	if e.Data["connection"] != "my-emby" {
		t.Errorf("connection = %v, want my-emby", e.Data["connection"])
	}
	if e.Data["error_class"] != "auth_failed" {
		t.Errorf("error_class = %v, want auth_failed", e.Data["error_class"])
	}
	if e.Data["artist_id"] != "a1" {
		t.Errorf("artist_id = %v, want a1", e.Data["artist_id"])
	}
	if e.Data["artist_name"] != "Test Artist" {
		t.Errorf("artist_name = %v, want Test Artist", e.Data["artist_name"])
	}
	if e.Data["operation"] != "lock_toggle" {
		t.Errorf("operation = %v, want lock_toggle", e.Data["operation"])
	}
	// connection_id must be present so the frontend can deep-link to the edit panel.
	if e.Data["connection_id"] != "conn-uuid-1" {
		t.Errorf("connection_id = %v, want conn-uuid-1", e.Data["connection_id"])
	}
	// The raw error -- which can leak internal hostnames + tokens -- must
	// NOT be on the event Data. The SSE hub broadcasts Data to every
	// connected client, so any DevTools observer would see it. The
	// server-side slog.Error in the publisher is the only sink for the
	// detailed error.
	if _, present := e.Data["error"]; present {
		t.Errorf("error key must not be present on published event (security: leaks URLs/tokens); got %v", e.Data["error"])
	}
}

// TestBusNotifier_NilBusIsNoOp verifies the adapter survives being
// constructed with a nil bus (test wiring path). The publisher should
// not panic, and no event is observable.
func TestBusNotifier_NilBusIsNoOp(t *testing.T) {
	t.Parallel()
	n := NewBusNotifier(nil)
	// Must not panic.
	n.NotifyConnectionPushFailed("", "conn", "class", "a1", "Test", "lock_toggle", errors.New("boom"))
}

// TestBusNotifier_OmitsEmptyContextFields verifies that the optional
// artist/operation fields are left off the event when empty, so a future
// caller that hasn't been plumbed for context still produces a clean
// payload (no "" fields cluttering downstream consumers).
func TestBusNotifier_OmitsEmptyContextFields(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := event.NewBus(logger, 8)

	var (
		mu       sync.Mutex
		received []event.Event
	)
	bus.Subscribe(event.ConnectionPushFailed, func(e event.Event) {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, e)
	})
	go bus.Start()
	defer bus.Stop()

	n := NewBusNotifier(bus)
	n.NotifyConnectionPushFailed("conn-uuid-2", "my-emby", "auth_failed", "", "", "", nil)

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(received)
		mu.Unlock()
		if count > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("received = %d, want 1", len(received))
	}
	// connection_id is present when non-empty (conn-uuid-2 was passed).
	if received[0].Data["connection_id"] != "conn-uuid-2" {
		t.Errorf("connection_id = %v, want conn-uuid-2", received[0].Data["connection_id"])
	}
	for _, k := range []string{"artist_id", "artist_name", "operation"} {
		if _, present := received[0].Data[k]; present {
			t.Errorf("optional field %q should be omitted when empty, got %v", k, received[0].Data[k])
		}
	}
}

// TestBusNotifier_OmitsEmptyConnectionID verifies that the connection_id field
// is omitted from the event payload when the caller passes an empty string,
// matching the behavior of the other optional fields.
func TestBusNotifier_OmitsEmptyConnectionID(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := event.NewBus(logger, 8)

	var (
		mu       sync.Mutex
		received []event.Event
	)
	bus.Subscribe(event.ConnectionPushFailed, func(e event.Event) {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, e)
	})
	go bus.Start()
	defer bus.Stop()

	n := NewBusNotifier(bus)
	n.NotifyConnectionPushFailed("", "my-emby", "auth_failed", "", "", "", nil)

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(received)
		mu.Unlock()
		if count > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("received = %d, want 1", len(received))
	}
	if _, present := received[0].Data["connection_id"]; present {
		t.Errorf("connection_id should be omitted when empty, got %v", received[0].Data["connection_id"])
	}
}
