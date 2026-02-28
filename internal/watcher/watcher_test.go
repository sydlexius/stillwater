package watcher

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/event"
	"github.com/sydlexius/stillwater/internal/library"
)

// mockLibraryLister returns a fixed set of libraries.
type mockLibraryLister struct {
	mu   sync.Mutex
	libs []library.Library
}

func (m *mockLibraryLister) List(_ context.Context) ([]library.Library, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]library.Library, len(m.libs))
	copy(cp, m.libs)
	return cp, nil
}

// testProbeCache returns a ProbeCache with all given paths marked as supported.
func testProbeCache(paths ...string) *ProbeCache {
	pc := NewProbeCache()
	for _, p := range paths {
		pc.Set(p, true)
	}
	return pc
}

func newTestService(t *testing.T, scanCount *atomic.Int32, libs *mockLibraryLister, probeCache *ProbeCache) (*Service, *event.Bus, context.Context, context.CancelFunc) {
	t.Helper()
	logger := testLogger()
	bus := event.NewBus(logger, 64)
	go bus.Start()
	t.Cleanup(bus.Stop)

	scanFn := func(_ context.Context) error {
		scanCount.Add(1)
		return nil
	}

	svc := NewService(scanFn, libs, bus, logger, probeCache)
	svc.SetDebounce(50 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	return svc, bus, ctx, cancel
}

func testLogger() *slog.Logger {
	return slog.Default()
}

func TestNewDirectoryTriggersScan(t *testing.T) {
	root := t.TempDir()

	var scanCount atomic.Int32
	libs := &mockLibraryLister{libs: []library.Library{
		{ID: "1", Name: "Test", Path: root, Type: "regular", FSWatch: library.FSModeWatch},
	}}

	svc, _, ctx, cancel := newTestService(t, &scanCount, libs, testProbeCache(root))
	defer cancel()

	go svc.Start(ctx)
	time.Sleep(100 * time.Millisecond) // let watcher initialize

	// Create a subdirectory.
	if err := os.Mkdir(filepath.Join(root, "New Artist"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Wait for debounce + processing.
	time.Sleep(300 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond)

	if got := scanCount.Load(); got != 1 {
		t.Errorf("expected 1 scan, got %d", got)
	}
}

func TestMultipleCreatesCoalesce(t *testing.T) {
	root := t.TempDir()

	var scanCount atomic.Int32
	libs := &mockLibraryLister{libs: []library.Library{
		{ID: "1", Name: "Test", Path: root, Type: "regular", FSWatch: library.FSModeWatch},
	}}

	svc, _, ctx, cancel := newTestService(t, &scanCount, libs, testProbeCache(root))
	defer cancel()

	go svc.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	// Create 5 directories rapidly.
	for i := 0; i < 5; i++ {
		name := filepath.Join(root, "Artist"+string(rune('A'+i)))
		if err := os.Mkdir(name, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Wait for debounce.
	time.Sleep(300 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond)

	if got := scanCount.Load(); got != 1 {
		t.Errorf("expected 1 coalesced scan, got %d", got)
	}
}

func TestRemovedDirectoryPublishesEvent(t *testing.T) {
	root := t.TempDir()

	// Pre-create a directory to remove.
	subdir := filepath.Join(root, "To Remove")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}

	var scanCount atomic.Int32
	libs := &mockLibraryLister{libs: []library.Library{
		{ID: "1", Name: "Test", Path: root, Type: "regular", FSWatch: library.FSModeWatch},
	}}

	svc, bus, ctx, cancel := newTestService(t, &scanCount, libs, testProbeCache(root))
	defer cancel()

	var received atomic.Int32
	bus.Subscribe(event.FSDirRemoved, func(e event.Event) {
		received.Add(1)
	})

	go svc.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	// Remove the directory.
	if err := os.Remove(subdir); err != nil {
		t.Fatal(err)
	}

	time.Sleep(300 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond)

	if got := received.Load(); got < 1 {
		t.Errorf("expected FSDirRemoved event, got %d", got)
	}
	// Removal should not trigger a scan.
	if got := scanCount.Load(); got != 0 {
		t.Errorf("expected 0 scans on removal, got %d", got)
	}
}

func TestFileCreationIgnored(t *testing.T) {
	root := t.TempDir()

	var scanCount atomic.Int32
	libs := &mockLibraryLister{libs: []library.Library{
		{ID: "1", Name: "Test", Path: root, Type: "regular", FSWatch: library.FSModeWatch},
	}}

	svc, _, ctx, cancel := newTestService(t, &scanCount, libs, testProbeCache(root))
	defer cancel()

	go svc.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	// Create a file (not a directory).
	f, err := os.Create(filepath.Join(root, "README.txt"))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	time.Sleep(300 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond)

	if got := scanCount.Load(); got != 0 {
		t.Errorf("expected 0 scans for file creation, got %d", got)
	}
}

func TestDegradedLibrarySkipped(t *testing.T) {
	var scanCount atomic.Int32
	libs := &mockLibraryLister{libs: []library.Library{
		{ID: "1", Name: "Degraded", Path: "", Type: "regular", FSWatch: library.FSModeWatch},
	}}

	svc, _, ctx, cancel := newTestService(t, &scanCount, libs, NewProbeCache())
	defer cancel()

	go svc.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	// No paths should be watched.
	svc.mu.Lock()
	watchCount := len(svc.watching)
	svc.mu.Unlock()

	cancel()
	time.Sleep(50 * time.Millisecond)

	if watchCount != 0 {
		t.Errorf("expected 0 watched paths for degraded library, got %d", watchCount)
	}
}

func TestContextCancellation(t *testing.T) {
	root := t.TempDir()

	var scanCount atomic.Int32
	libs := &mockLibraryLister{libs: []library.Library{
		{ID: "1", Name: "Test", Path: root, Type: "regular", FSWatch: library.FSModeWatch},
	}}

	svc, _, ctx, cancel := newTestService(t, &scanCount, libs, testProbeCache(root))

	done := make(chan struct{})
	go func() {
		svc.Start(ctx)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Start returned cleanly.
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after context cancellation")
	}
}

func TestFSWatchEnabled(t *testing.T) {
	tests := []struct {
		mode int
		want bool
	}{
		{library.FSModeOff, false},
		{library.FSModeWatch, true},
		{library.FSModePoll, false},
		{library.FSModeBoth, true},
	}
	for _, tt := range tests {
		lib := library.Library{FSWatch: tt.mode}
		if got := lib.FSWatchEnabled(); got != tt.want {
			t.Errorf("FSWatchEnabled() for mode %d = %v, want %v", tt.mode, got, tt.want)
		}
	}
}

func TestFSPollEnabled(t *testing.T) {
	tests := []struct {
		mode int
		want bool
	}{
		{library.FSModeOff, false},
		{library.FSModeWatch, false},
		{library.FSModePoll, true},
		{library.FSModeBoth, true},
	}
	for _, tt := range tests {
		lib := library.Library{FSWatch: tt.mode}
		if got := lib.FSPollEnabled(); got != tt.want {
			t.Errorf("FSPollEnabled() for mode %d = %v, want %v", tt.mode, got, tt.want)
		}
	}
}

func TestPollDetectsNewDirectory(t *testing.T) {
	root := t.TempDir()

	var scanCount atomic.Int32
	libs := &mockLibraryLister{libs: []library.Library{
		{ID: "1", Name: "Test", Path: root, Type: "regular", FSWatch: library.FSModePoll, FSPollInterval: 60},
	}}

	logger := testLogger()
	bus := event.NewBus(logger, 64)
	go bus.Start()
	t.Cleanup(bus.Stop)

	scanFn := func(_ context.Context) error {
		scanCount.Add(1)
		return nil
	}

	svc := NewService(scanFn, libs, bus, logger, NewProbeCache())
	svc.SetDebounce(50 * time.Millisecond)

	ctx := context.Background()
	svc.initPollSnapshots(ctx)

	// Create a new directory.
	if err := os.Mkdir(filepath.Join(root, "Poll Artist"), 0o755); err != nil {
		t.Fatal(err)
	}

	var received atomic.Int32
	bus.Subscribe(event.FSDirCreated, func(e event.Event) {
		received.Add(1)
	})

	// Force poll by setting last poll time far in the past.
	svc.mu.Lock()
	svc.lastPollTime[root] = time.Time{}
	svc.mu.Unlock()

	changed := svc.pollDirectories()
	time.Sleep(100 * time.Millisecond) // let event dispatch

	if !changed {
		t.Error("expected pollDirectories to report changes")
	}
	if got := received.Load(); got != 1 {
		t.Errorf("expected 1 FSDirCreated event from poll, got %d", got)
	}
}

func TestPollDetectsRemovedDirectory(t *testing.T) {
	root := t.TempDir()
	subdir := filepath.Join(root, "Will Remove")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}

	var scanCount atomic.Int32
	libs := &mockLibraryLister{libs: []library.Library{
		{ID: "1", Name: "Test", Path: root, Type: "regular", FSWatch: library.FSModePoll, FSPollInterval: 60},
	}}

	logger := testLogger()
	bus := event.NewBus(logger, 64)
	go bus.Start()
	t.Cleanup(bus.Stop)

	scanFn := func(_ context.Context) error {
		scanCount.Add(1)
		return nil
	}

	svc := NewService(scanFn, libs, bus, logger, NewProbeCache())
	ctx := context.Background()
	svc.initPollSnapshots(ctx)

	// Remove the directory.
	if err := os.Remove(subdir); err != nil {
		t.Fatal(err)
	}

	var received atomic.Int32
	bus.Subscribe(event.FSDirRemoved, func(e event.Event) {
		received.Add(1)
	})

	svc.mu.Lock()
	svc.lastPollTime[root] = time.Time{}
	svc.mu.Unlock()

	changed := svc.pollDirectories()
	time.Sleep(100 * time.Millisecond)

	if !changed {
		t.Error("expected pollDirectories to report changes")
	}
	if got := received.Load(); got != 1 {
		t.Errorf("expected 1 FSDirRemoved event from poll, got %d", got)
	}
}

func TestPollIntervalRespected(t *testing.T) {
	root := t.TempDir()

	var scanCount atomic.Int32
	libs := &mockLibraryLister{libs: []library.Library{
		{ID: "1", Name: "Test", Path: root, Type: "regular", FSWatch: library.FSModePoll, FSPollInterval: 1800},
	}}

	logger := testLogger()
	bus := event.NewBus(logger, 64)
	go bus.Start()
	t.Cleanup(bus.Stop)

	scanFn := func(_ context.Context) error {
		scanCount.Add(1)
		return nil
	}

	svc := NewService(scanFn, libs, bus, logger, NewProbeCache())
	ctx := context.Background()
	svc.initPollSnapshots(ctx)

	// Create a new directory.
	if err := os.Mkdir(filepath.Join(root, "New Dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Don't reset lastPollTime, so the 1800s interval is not met.
	changed := svc.pollDirectories()
	if changed {
		t.Error("expected no changes because poll interval has not elapsed")
	}
}

func TestUnsupportedFSNotifySkipsWatch(t *testing.T) {
	root := t.TempDir()

	var scanCount atomic.Int32
	libs := &mockLibraryLister{libs: []library.Library{
		{ID: "1", Name: "Test", Path: root, Type: "regular", FSWatch: library.FSModeWatch},
	}}

	// Mark the path as unsupported in probe cache.
	pc := NewProbeCache()
	pc.Set(root, false)

	svc, _, ctx, cancel := newTestService(t, &scanCount, libs, pc)
	defer cancel()

	go svc.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	svc.mu.Lock()
	watchCount := len(svc.watching)
	svc.mu.Unlock()

	cancel()
	time.Sleep(50 * time.Millisecond)

	if watchCount != 0 {
		t.Errorf("expected 0 watched paths for unsupported fsnotify, got %d", watchCount)
	}
}
