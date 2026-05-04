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

// waitOrFail blocks until wg signals done or the timeout fires. Tests use
// this to join handler goroutines deterministically -- a fixed time.Sleep
// would either under-wait (flake) or over-wait (slow).
func waitOrFail(t *testing.T, wg *sync.WaitGroup, msg string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal(msg)
	}
}

func TestPublishSubscribe(t *testing.T) {
	bus := NewBus(testLogger(), 16)
	go bus.Start()
	defer bus.Stop()

	var mu sync.Mutex
	var received []Event
	var wg sync.WaitGroup
	wg.Add(1)

	bus.Subscribe(ScanCompleted, func(e Event) {
		defer wg.Done()
		mu.Lock()
		defer mu.Unlock()
		received = append(received, e)
	})

	bus.Publish(Event{
		Type: ScanCompleted,
		Data: map[string]any{"artists": 42},
	})

	waitOrFail(t, &wg, "handler not invoked within 1s")

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
	var wg sync.WaitGroup
	wg.Add(3)

	for range 3 {
		bus.Subscribe(BulkCompleted, func(_ Event) {
			defer wg.Done()
			mu.Lock()
			defer mu.Unlock()
			count++
		})
	}

	bus.Publish(Event{Type: BulkCompleted})
	waitOrFail(t, &wg, "all 3 handlers not invoked within 1s")

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

	// Publish must not panic when no subscribers are registered. With no
	// handlers there is nothing to wait on -- the bus dispatch loop simply
	// drops the event. The synchronous Publish call returning is the only
	// signal we need; defer bus.Stop() drains the loop.
	bus.Publish(Event{Type: ArtistNew})
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
	var wg sync.WaitGroup
	wg.Add(2)

	bus.Subscribe(MetadataFixed, func(_ Event) {
		// The bus must still call wg.Done() for this handler even though
		// it panics; the bus's recover() path is what we are testing.
		defer wg.Done()
		panic("test panic")
	})
	bus.Subscribe(MetadataFixed, func(_ Event) {
		defer wg.Done()
		mu.Lock()
		defer mu.Unlock()
		secondCalled = true
	})

	bus.Publish(Event{Type: MetadataFixed})
	waitOrFail(t, &wg, "both handlers not invoked within 1s")

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
	var wg sync.WaitGroup
	wg.Add(2)

	bus.Subscribe(ScanCompleted, func(_ Event) {
		defer wg.Done()
		mu.Lock()
		defer mu.Unlock()
		count++
	})

	// Publish before starting -- both events sit in the channel buffer.
	bus.Publish(Event{Type: ScanCompleted})
	bus.Publish(Event{Type: ScanCompleted})

	go bus.Start()
	// Wait until both buffered events have been dispatched, then Stop. The
	// wg signal is the contract: "both handlers ran". Bus.Stop() is then
	// the synchronous drain barrier; no second wait needed.
	waitOrFail(t, &wg, "buffered events not drained within 1s")
	bus.Stop()

	mu.Lock()
	defer mu.Unlock()
	if count != 2 {
		t.Errorf("got %d events, want 2 (all drained)", count)
	}
}
