package maintenance

// registry_repair_check_test.go -- the cached registry-repair detector (#2678).

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegistryRepairCheck_CachesCount(t *testing.T) {
	svc := newDupCountService(t)
	svc.registryScan = func(context.Context) (int, error) { return 7, nil }
	cache := &RegistryRepairCache{}
	if _, _, ok := cache.Get(); ok {
		t.Fatal("a fresh cache must report ok=false")
	}
	svc.checkRegistryRepair(context.Background(), cache, func() bool { return false }, time.Second)
	count, at, ok := cache.Get()
	if !ok || count != 7 || at.IsZero() {
		t.Fatalf("Get() = %d, %v, %v; want 7, non-zero, true", count, at, ok)
	}
}

func TestRegistryRepairCheck_FailureMarksNotOK(t *testing.T) {
	svc := newDupCountService(t)
	cache := &RegistryRepairCache{}
	cache.SetFromRepair(3)
	svc.registryScan = func(context.Context) (int, error) { return 0, errors.New("boom") }
	svc.checkRegistryRepair(context.Background(), cache, func() bool { return false }, time.Second)
	if count, _, ok := cache.Get(); ok || count != 0 {
		t.Fatalf("after a failed check Get() = %d, ok=%v; want 0, false", count, ok)
	}
}

func TestRegistryRepairCheck_SingleFlight(t *testing.T) {
	svc := newDupCountService(t)
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	svc.registryScan = func(context.Context) (int, error) {
		calls.Add(1)
		close(entered)
		<-release
		return 1, nil
	}
	cache := &RegistryRepairCache{}
	done := make(chan struct{})
	go func() {
		svc.checkRegistryRepair(context.Background(), cache, func() bool { return false }, time.Second)
		close(done)
	}()
	<-entered
	// Second check while the first is mid-scan: must return without scanning.
	svc.checkRegistryRepair(context.Background(), cache, func() bool { return false }, time.Second)
	close(release)
	<-done
	if n := calls.Load(); n != 1 {
		t.Fatalf("scan ran %d times, want 1", n)
	}
}

func TestRegistryRepairCheck_SkipsWhileRepairRuns(t *testing.T) {
	svc := newDupCountService(t)
	var calls atomic.Int32
	svc.registryScan = func(context.Context) (int, error) { calls.Add(1); return 1, nil }
	cache := &RegistryRepairCache{}
	svc.checkRegistryRepair(context.Background(), cache, func() bool { return true }, time.Second)
	if calls.Load() != 0 {
		t.Fatal("scan ran while a repair was running")
	}
	if _, _, ok := cache.Get(); ok {
		t.Fatal("a skipped tick must not touch the cache")
	}
}

func TestRegistryRepairCheck_DeadlineRespected(t *testing.T) {
	svc := newDupCountService(t)
	svc.registryScan = func(ctx context.Context) (int, error) {
		// Exits on its own after 5s so an ignored deadline fails with a named
		// --- FAIL (the 1s bound below) instead of hanging to the package timeout.
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
		return 0, ctx.Err()
	}
	cache := &RegistryRepairCache{}
	start := time.Now()
	svc.checkRegistryRepair(context.Background(), cache, func() bool { return false }, 20*time.Millisecond)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("check took %v; the per-run deadline did not bound it", d)
	}
	if _, _, ok := cache.Get(); ok {
		t.Fatal("a timed-out check must leave ok=false")
	}
	// The latch must be free again.
	if _, claimed := cache.begin(); !claimed {
		t.Fatal("latch still held after a timed-out check")
	}
}

// A committed repair that lands while a scan is in flight wins: the scan may
// have read the pre-repair state.
func TestRegistryRepairCheck_RepairResultBeatsStaleScan(t *testing.T) {
	svc := newDupCountService(t)
	cache := &RegistryRepairCache{}
	svc.registryScan = func(context.Context) (int, error) {
		cache.SetFromRepair(0)
		return 9, nil
	}
	svc.checkRegistryRepair(context.Background(), cache, func() bool { return false }, time.Second)
	if count, _, ok := cache.Get(); !ok || count != 0 {
		t.Fatalf("Get() = %d, %v; want the repair's 0, true", count, ok)
	}
	if _, claimed := cache.begin(); !claimed {
		t.Fatal("latch still held after a scan that lost to a repair result")
	}
}

// A panicking scan must not crash the caller, must leave the cache not-ok, and
// must free the single-flight latch.
func TestRegistryRepairCheck_PanicFreesLatch(t *testing.T) {
	svc := newDupCountService(t)
	svc.registryScan = func(context.Context) (int, error) { panic("boom") }
	cache := &RegistryRepairCache{}
	cache.SetFromRepair(3)
	svc.checkRegistryRepair(context.Background(), cache, func() bool { return false }, time.Second)
	if count, _, ok := cache.Get(); ok || count != 0 {
		t.Fatalf("after a panicking scan Get() = %d, ok=%v; want 0, false", count, ok)
	}
	if _, claimed := cache.begin(); !claimed {
		t.Fatal("latch still held after a panicking scan")
	}
}

// The real detector (no fake) must report exactly what the dry-run passes
// plan, and must not write.
func TestScanRegistryRepair_MatchesDryRunPlan(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	applyFixture(t, db, t.TempDir())
	svc := newRepairService(t, db, dbPath, "")
	before := fullRows(t, db)

	got, err := svc.scanRegistryRepair(context.Background())
	if err != nil {
		t.Fatalf("scanRegistryRepair: %v", err)
	}
	rebuild, err := svc.RepairImageRegistry(context.Background(), ImageRepairOpts{})
	if err != nil {
		t.Fatal(err)
	}
	restore, err := svc.RestoreExistsFlags(context.Background(), ExistsFlagRestoreOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if want := rebuild.RowsPlanned + restore.Restored; got != want || got == 0 {
		t.Fatalf("scan = %d, want %d (non-zero: fixture needs repair)", got, want)
	}
	if after := fullRows(t, db); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatal("the detector wrote to artist_images")
	}
}

// The loop scans once after the startup delay, again on a tick, and stops on
// cancel; a missing cache or repairRunning hook fails loudly and never scans.
func TestStartRegistryRepairCheck_LoopAndGuards(t *testing.T) {
	svc := newDupCountService(t)
	var calls atomic.Int32
	svc.registryScan = func(context.Context) (int, error) { calls.Add(1); return 1, nil }
	idle := func() bool { return false }

	svc.StartRegistryRepairCheck(context.Background(), nil, idle, time.Millisecond, time.Millisecond)
	svc.StartRegistryRepairCheck(context.Background(), &RegistryRepairCache{}, nil, time.Millisecond, time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("an unwired detector scanned")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.StartRegistryRepairCheck(ctx, &RegistryRepairCache{}, idle, 5*time.Millisecond, time.Millisecond)
		close(done)
	}()
	if !waitFor(t, func() bool { return calls.Load() >= 2 }) {
		t.Fatalf("expected startup scan plus a tick, got %d", calls.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(waitForTimeout):
		t.Fatal("loop did not stop on cancel")
	}

	// A long interval must still scan once at startup (no tick ever fires).
	calls.Store(0)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go svc.StartRegistryRepairCheck(ctx2, &RegistryRepairCache{}, idle, time.Hour, time.Millisecond)
	if !waitFor(t, func() bool { return calls.Load() == 1 }) {
		t.Fatalf("startup scan with a 1h interval ran %d times, want 1", calls.Load())
	}
}
