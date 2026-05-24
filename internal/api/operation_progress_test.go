package api

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/event"
)

// (helpers below are defined in this file: newRecorderRouter)

// TestPublishOpProgress_RoundTripsAllFields verifies the helper packs
// every ProgressPill-relevant field onto the event bus so the JS
// renderer in the layout-level component has everything it needs
// (op_id for pill identity, cancel_url for the Cancel button, status
// for the terminal-state branches).
func TestPublishOpProgress_RoundTripsAllFields(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := event.NewBus(logger, 16)

	var (
		mu       sync.Mutex
		received []event.Event
	)
	bus.Subscribe(event.OperationProgress, func(e event.Event) {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, e)
	})
	go bus.Start()
	defer bus.Stop()

	r := &Router{eventBus: bus}
	r.publishOpProgress("bulk_action", "run_rules", 10, 3, "running", "/api/v1/artists/bulk-actions/cancel")

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
	e := received[0]
	if e.Type != event.OperationProgress {
		t.Errorf("type = %q, want %q", e.Type, event.OperationProgress)
	}
	want := map[string]any{
		"op_id":      "bulk_action",
		"label":      "run_rules",
		"processed":  3,
		"total":      10,
		"status":     "running",
		"cancel_url": "/api/v1/artists/bulk-actions/cancel",
	}
	for k, v := range want {
		if e.Data[k] != v {
			t.Errorf("data[%q] = %v, want %v", k, e.Data[k], v)
		}
	}
}

// TestPublishOpProgress_OmitsCancelURLWhenEmpty verifies the terminal
// path (no cancel possible once completed/failed) leaves cancel_url
// off the event so the pill JS hides the Cancel button instead of
// rendering a broken one.
func TestPublishOpProgress_OmitsCancelURLWhenEmpty(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := event.NewBus(logger, 16)
	var (
		mu       sync.Mutex
		received []event.Event
	)
	bus.Subscribe(event.OperationProgress, func(e event.Event) {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, e)
	})
	go bus.Start()
	defer bus.Stop()

	r := &Router{eventBus: bus}
	r.publishOpProgress("bulk_action", "run_rules", 10, 10, "completed", "")

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
	if _, present := received[0].Data["cancel_url"]; present {
		t.Errorf("cancel_url should be omitted on terminal events, got %v", received[0].Data["cancel_url"])
	}
}

// TestPublishOpProgress_NilEventBusIsNoOp guards the test/headless
// wiring path: a router constructed without an event bus must not
// panic when the bulk-action goroutine emits progress.
func TestPublishOpProgress_NilEventBusIsNoOp(t *testing.T) {
	t.Parallel()
	r := &Router{}
	r.publishOpProgress("bulk_action", "run_rules", 10, 3, "running", "/cancel")
	// Must not panic; nothing to assert.
}

// newRecorderRouter builds a minimal Router with a fresh event bus and
// an OperationProgress subscriber, returning the router and a snapshot
// helper. Defensive-guard tests use this so they can both invoke
// publishOpProgress and observe whether anything landed on the bus.
func newRecorderRouter(t *testing.T) (*Router, func() []event.Event, func()) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := event.NewBus(logger, 32)
	var (
		mu       sync.Mutex
		received []event.Event
	)
	bus.Subscribe(event.OperationProgress, func(e event.Event) {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, e)
	})
	go bus.Start()
	r := &Router{eventBus: bus, logger: logger}
	snapshot := func() []event.Event {
		mu.Lock()
		defer mu.Unlock()
		out := make([]event.Event, len(received))
		copy(out, received)
		return out
	}
	cleanup := func() {
		bus.Stop()
		time.Sleep(20 * time.Millisecond)
	}
	return r, snapshot, cleanup
}

// TestPublishOpProgress_EmptyOpIDDropped: an empty op_id would either
// collide with the default-key pill (overwriting unrelated state) or
// render a never-dismissable phantom pill. The guard drops the event.
func TestPublishOpProgress_EmptyOpIDDropped(t *testing.T) {
	t.Parallel()
	r, snap, stop := newRecorderRouter(t)
	defer stop()
	r.publishOpProgress("", "run_rules", 10, 3, "running", "/cancel")
	time.Sleep(50 * time.Millisecond)
	if got := snap(); len(got) != 0 {
		t.Errorf("empty op_id should be dropped, got %d events: %+v", len(got), got)
	}
}

// TestPublishOpProgress_NegativeTotalNormalized: a negative total is
// meaningless; the guard treats it as indeterminate (0).
func TestPublishOpProgress_NegativeTotalNormalized(t *testing.T) {
	t.Parallel()
	r, snap, stop := newRecorderRouter(t)
	defer stop()
	r.publishOpProgress("bulk_action", "run_rules", -5, 0, "running", "")
	time.Sleep(50 * time.Millisecond)
	got := snap()
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if got[0].Data["total"] != 0 {
		t.Errorf("total = %v, want 0 (negative normalized)", got[0].Data["total"])
	}
}

// TestPublishOpProgress_ProcessedClampedToTotal: a processed > total
// value would invert the progress bar; clamp to total instead.
func TestPublishOpProgress_ProcessedClampedToTotal(t *testing.T) {
	t.Parallel()
	r, snap, stop := newRecorderRouter(t)
	defer stop()
	r.publishOpProgress("bulk_action", "run_rules", 10, 99, "running", "")
	time.Sleep(50 * time.Millisecond)
	got := snap()
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if got[0].Data["processed"] != 10 {
		t.Errorf("processed = %v, want 10 (clamped to total)", got[0].Data["processed"])
	}
}

// TestPublishOpProgress_ZeroTotalAllowsAnyProcessed: total=0 is the
// indeterminate signal; processed is passed through unchanged in that
// case (used by future callers that can't precompute total).
func TestPublishOpProgress_ZeroTotalAllowsAnyProcessed(t *testing.T) {
	t.Parallel()
	r, snap, stop := newRecorderRouter(t)
	defer stop()
	r.publishOpProgress("bulk_action", "run_rules", 0, 7, "running", "")
	time.Sleep(50 * time.Millisecond)
	got := snap()
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if got[0].Data["processed"] != 7 {
		t.Errorf("processed = %v, want 7 (no clamp when total=0)", got[0].Data["processed"])
	}
}
