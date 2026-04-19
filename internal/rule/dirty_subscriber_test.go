package rule

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/event"
)

// pollDirty waits up to 5s for the artist's dirty_since to satisfy the
// condition. The DirtySubscriber writes synchronously inside HandleEvent,
// but the surrounding event bus delivers events on its own goroutine, so
// tests must poll rather than assume immediate visibility.
func pollDirty(t *testing.T, svc *artist.Service, artistID string, condition func(*time.Time) bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var lastDirty *time.Time
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for dirty_since condition (last value: %v)", lastDirty)
		case <-ticker.C:
			a, err := svc.GetByID(context.Background(), artistID)
			if err != nil {
				continue
			}
			lastDirty = a.DirtySince
			if condition(a.DirtySince) {
				return
			}
		}
	}
}

// TestDirtySubscriber_HandleEventStampsDirty verifies the happy path:
// an ArtistUpdated event flows through HandleEvent and dirty_since is
// stamped on the database row.
func TestDirtySubscriber_HandleEventStampsDirty(t *testing.T) {
	db := setupSubscriberTestDB(t)
	svc := artist.NewService(db)

	a := &artist.Artist{
		Name:     "Dirty Stamp",
		SortName: "Dirty Stamp",
		Path:     "/music/dirty-stamp",
	}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	sub := NewDirtySubscriber(svc, slog.Default())
	sub.HandleEvent(event.Event{
		Type: event.ArtistUpdated,
		Data: map[string]any{"artist_id": a.ID},
	})

	pollDirty(t, svc, a.ID, func(ts *time.Time) bool { return ts != nil })

	reread, err := svc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if reread.DirtySince == nil || reread.DirtySince.IsZero() {
		t.Fatalf("dirty_since not stamped after HandleEvent: %v", reread.DirtySince)
	}
}

// TestDirtySubscriber_BusEnd2End wires the subscriber into a real event
// bus and verifies that events published from another goroutine reach
// HandleEvent and stamp dirty_since. Run with -race to catch unsafe
// shared state if a future change introduces it.
func TestDirtySubscriber_BusEnd2End(t *testing.T) {
	db := setupSubscriberTestDB(t)
	svc := artist.NewService(db)

	a := &artist.Artist{
		Name:     "End To End",
		SortName: "End To End",
		Path:     "/music/end-to-end",
	}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	bus := event.NewBus(slog.Default(), 32)
	go bus.Start()
	defer bus.Stop()

	sub := NewDirtySubscriber(svc, slog.Default())
	bus.Subscribe(event.ArtistUpdated, sub.HandleEvent)

	bus.Publish(event.Event{
		Type: event.ArtistUpdated,
		Data: map[string]any{"artist_id": a.ID},
	})

	pollDirty(t, svc, a.ID, func(ts *time.Time) bool { return ts != nil })
}

// TestDirtySubscriber_InvalidEventsAreNoOps verifies that malformed
// events (missing or wrong-typed artist_id) are logged but never panic
// or affect any artist row. Important because the bus delivers a wide
// variety of event payloads.
func TestDirtySubscriber_InvalidEventsAreNoOps(t *testing.T) {
	db := setupSubscriberTestDB(t)
	svc := artist.NewService(db)

	a := &artist.Artist{
		Name:     "Untouched",
		SortName: "Untouched",
		Path:     "/music/untouched",
	}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	sub := NewDirtySubscriber(svc, slog.Default())

	cases := []event.Event{
		{Type: event.ArtistUpdated, Data: nil},
		{Type: event.ArtistUpdated, Data: map[string]any{"artist_id": 42}}, // wrong type
		{Type: event.ArtistUpdated, Data: map[string]any{"artist_id": ""}}, // empty string
		{Type: event.ArtistUpdated, Data: map[string]any{"other": "x"}},    // missing key
	}
	for _, e := range cases {
		sub.HandleEvent(e) // must not panic
	}

	// Give a tick in case any rogue write was queued.
	time.Sleep(100 * time.Millisecond)

	reread, err := svc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if reread.DirtySince != nil {
		t.Fatalf("dirty_since stamped from invalid event: %v", reread.DirtySince)
	}
}

// TestDirtySubscriber_NilArtistServiceIsNoOp verifies graceful
// degradation: the subscriber must not panic when its dependency is nil
// (mirrors the HealthSubscriber's nil-engine contract).
func TestDirtySubscriber_NilArtistServiceIsNoOp(t *testing.T) {
	sub := NewDirtySubscriber(nil, slog.Default())
	sub.HandleEvent(event.Event{
		Type: event.ArtistUpdated,
		Data: map[string]any{"artist_id": "anything"},
	})
}

// TestDirtySubscriber_ConcurrentEvents stresses the subscriber under
// many parallel HandleEvent calls. The race detector flags any unsafe
// shared state; the assertion confirms every artist ended up dirty.
func TestDirtySubscriber_ConcurrentEvents(t *testing.T) {
	db := setupSubscriberTestDB(t)
	svc := artist.NewService(db)

	const n = 20
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		a := &artist.Artist{
			Name:     "Concurrent",
			SortName: "Concurrent",
			Path:     "/music/concurrent/" + string(rune('A'+i)),
		}
		if err := svc.Create(context.Background(), a); err != nil {
			t.Fatalf("creating artist %d: %v", i, err)
		}
		ids[i] = a.ID
	}

	sub := NewDirtySubscriber(svc, slog.Default())

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sub.HandleEvent(event.Event{
				Type: event.ArtistUpdated,
				Data: map[string]any{"artist_id": id},
			})
		}(id)
	}
	wg.Wait()

	for _, id := range ids {
		pollDirty(t, svc, id, func(ts *time.Time) bool { return ts != nil })
	}
}
