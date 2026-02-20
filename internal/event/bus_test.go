package event

import (
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestPublishSubscribe(t *testing.T) {
	bus := NewBus(testLogger(), 16)
	go bus.Start()
	defer bus.Stop()

	var mu sync.Mutex
	var received []Event

	bus.Subscribe(ScanCompleted, func(e Event) {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, e)
	})

	bus.Publish(Event{
		Type: ScanCompleted,
		Data: map[string]any{"artists": 42},
	})

	// Give the goroutine time to process
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("got %d events, want 1", len(received))
	}
	if received[0].Data["artists"] != 42 {
		t.Errorf("data[artists] = %v, want 42", received[0].Data["artists"])
	}
	if received[0].Timestamp.IsZero() {
		t.Error("expected timestamp to be set")
	}
}

func TestMultipleSubscribers(t *testing.T) {
	bus := NewBus(testLogger(), 16)
	go bus.Start()
	defer bus.Stop()

	var mu sync.Mutex
	count := 0

	for range 3 {
		bus.Subscribe(BulkCompleted, func(_ Event) {
			mu.Lock()
			defer mu.Unlock()
			count++
		})
	}

	bus.Publish(Event{Type: BulkCompleted})
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if count != 3 {
		t.Errorf("got %d handler calls, want 3", count)
	}
}

func TestNoSubscribers(t *testing.T) {
	bus := NewBus(testLogger(), 16)
	go bus.Start()
	defer bus.Stop()

	// Should not panic
	bus.Publish(Event{Type: ArtistNew})
	time.Sleep(50 * time.Millisecond)
}

func TestBufferFull(t *testing.T) {
	bus := NewBus(testLogger(), 2)
	// Do NOT start the bus -- events will accumulate in the channel

	bus.Publish(Event{Type: ScanCompleted})
	bus.Publish(Event{Type: ScanCompleted})
	// Third event should be dropped (buffer full)
	bus.Publish(Event{Type: ScanCompleted})
	// No panic or deadlock expected
}

func TestHandlerPanicRecovery(t *testing.T) {
	bus := NewBus(testLogger(), 16)
	go bus.Start()
	defer bus.Stop()

	var mu sync.Mutex
	secondCalled := false

	bus.Subscribe(MetadataFixed, func(_ Event) {
		panic("test panic")
	})
	bus.Subscribe(MetadataFixed, func(_ Event) {
		mu.Lock()
		defer mu.Unlock()
		secondCalled = true
	})

	bus.Publish(Event{Type: MetadataFixed})
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if !secondCalled {
		t.Error("second handler should still be called after first panics")
	}
}

func TestStopDrainsBuffer(t *testing.T) {
	bus := NewBus(testLogger(), 16)

	var mu sync.Mutex
	count := 0

	bus.Subscribe(ScanCompleted, func(_ Event) {
		mu.Lock()
		defer mu.Unlock()
		count++
	})

	// Publish before starting
	bus.Publish(Event{Type: ScanCompleted})
	bus.Publish(Event{Type: ScanCompleted})

	go bus.Start()
	time.Sleep(50 * time.Millisecond)
	bus.Stop()
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if count != 2 {
		t.Errorf("got %d events, want 2 (all drained)", count)
	}
}
