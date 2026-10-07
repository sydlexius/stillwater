package maintenance

// registry_repair_check_test.go -- the cached registry-repair detector (#2678).

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// noClaim is a claim that always succeeds and holds nothing.
func noClaim() (func(), bool) { return func() {}, true }

func TestRegistryRepairCheck_CachesCount(t *testing.T) {
	svc := newDupCountService(t)
	svc.registryScan = func(context.Context) (RegistryRepairPlan, error) { return RegistryRepairPlan{Rebuild: 7}, nil }
	cache := &RegistryRepairCache{}
	if _, _, ok := cache.Get(); ok {
		t.Fatal("a fresh cache must report ok=false")
	}
	svc.checkRegistryRepair(context.Background(), cache, noClaim, time.Second)
	count, at, ok := cache.Get()
	if !ok || count != 7 || at.IsZero() {
		t.Fatalf("Get() = %d, %v, %v; want 7, non-zero, true", count, at, ok)
	}
}

func TestRegistryRepairCheck_FailureMarksNotOK(t *testing.T) {
	svc := newDupCountService(t)
	cache := &RegistryRepairCache{}
	cache.SetFromRepair(3)
	svc.registryScan = func(context.Context) (RegistryRepairPlan, error) { return RegistryRepairPlan{}, errors.New("boom") }
	svc.checkRegistryRepair(context.Background(), cache, noClaim, time.Second)
	if count, _, ok := cache.Get(); ok || count != 0 {
		t.Fatalf("after a failed check Get() = %d, ok=%v; want 0, false", count, ok)
	}
}

func TestRegistryRepairCheck_SingleFlight(t *testing.T) {
	svc := newDupCountService(t)
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	svc.registryScan = func(context.Context) (RegistryRepairPlan, error) {
		calls.Add(1)
		close(entered)
		<-release
		return RegistryRepairPlan{Rebuild: 1}, nil
	}
	cache := &RegistryRepairCache{}
	done := make(chan struct{})
	go func() {
		svc.checkRegistryRepair(context.Background(), cache, noClaim, time.Second)
		close(done)
	}()
	<-entered
	// Second check while the first is mid-scan: must return without scanning.
	svc.checkRegistryRepair(context.Background(), cache, noClaim, time.Second)
	close(release)
	<-done
	if n := calls.Load(); n != 1 {
		t.Fatalf("scan ran %d times, want 1", n)
	}
}

func TestRegistryRepairCheck_SkipsWhileRepairRuns(t *testing.T) {
	svc := newDupCountService(t)
	var calls atomic.Int32
	svc.registryScan = func(context.Context) (RegistryRepairPlan, error) {
		calls.Add(1)
		return RegistryRepairPlan{Rebuild: 1}, nil
	}
	cache := &RegistryRepairCache{}
	svc.checkRegistryRepair(context.Background(), cache, func() (func(), bool) { return nil, false }, time.Second)
	if calls.Load() != 0 {
		t.Fatal("scan ran while a repair was running")
	}
	if _, _, ok := cache.Get(); ok {
		t.Fatal("a skipped tick must not touch the cache")
	}
}

func TestRegistryRepairCheck_DeadlineRespected(t *testing.T) {
	svc := newDupCountService(t)
	svc.registryScan = func(ctx context.Context) (RegistryRepairPlan, error) {
		// Exits on its own after 5s so an ignored deadline fails with a named
		// --- FAIL (the 1s bound below) instead of hanging to the package timeout.
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
		return RegistryRepairPlan{}, ctx.Err()
	}
	cache := &RegistryRepairCache{}
	start := time.Now()
	svc.checkRegistryRepair(context.Background(), cache, noClaim, 20*time.Millisecond)
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
	svc.registryScan = func(context.Context) (RegistryRepairPlan, error) {
		cache.SetFromRepair(0)
		return RegistryRepairPlan{Rebuild: 9}, nil
	}
	svc.checkRegistryRepair(context.Background(), cache, noClaim, time.Second)
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
	svc.registryScan = func(context.Context) (RegistryRepairPlan, error) { panic("boom") }
	cache := &RegistryRepairCache{}
	cache.SetFromRepair(3)
	svc.checkRegistryRepair(context.Background(), cache, noClaim, time.Second)
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
	if want := (RegistryRepairPlan{Rebuild: rebuild.RowsPlanned, Restore: restore.Restored}); got != want || got.Rebuild+got.Restore == 0 {
		t.Fatalf("scan = %+v, want %+v (non-zero: fixture needs repair)", got, want)
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
	svc.registryScan = func(context.Context) (RegistryRepairPlan, error) {
		calls.Add(1)
		return RegistryRepairPlan{Rebuild: 1}, nil
	}
	idle := noClaim

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

// A refused claim (a user repair is running) must not scan or touch the cache.
func TestRegistryRepairCheck_RefusedClaimDoesNotScan(t *testing.T) {
	svc := newDupCountService(t)
	var calls atomic.Int32
	svc.registryScan = func(context.Context) (RegistryRepairPlan, error) {
		calls.Add(1)
		return RegistryRepairPlan{Rebuild: 1}, nil
	}
	cache := &RegistryRepairCache{}
	svc.checkRegistryRepair(context.Background(), cache, func() (func(), bool) { return nil, false }, time.Second)
	if calls.Load() != 0 {
		t.Fatal("scanned although the claim was refused")
	}
	if _, _, ok := cache.Get(); ok {
		t.Fatal("a skipped check wrote the cache")
	}
}

// The claim is held for the whole scan and released after it, on success and
// after a panicking scan.
func TestRegistryRepairCheck_ClaimHeldDuringScanAndReleased(t *testing.T) {
	for _, panics := range []bool{false, true} {
		svc := newDupCountService(t)
		var held, releases atomic.Int32
		claim := func() (func(), bool) {
			held.Add(1)
			return func() { held.Add(-1); releases.Add(1) }, true
		}
		var heldDuring int32
		svc.registryScan = func(context.Context) (RegistryRepairPlan, error) {
			heldDuring = held.Load()
			if panics {
				panic("boom")
			}
			return RegistryRepairPlan{Rebuild: 1}, nil
		}
		svc.checkRegistryRepair(context.Background(), &RegistryRepairCache{}, claim, time.Second)
		if heldDuring != 1 {
			t.Fatalf("panics=%v: claim held during scan = %d, want 1", panics, heldDuring)
		}
		if held.Load() != 0 || releases.Load() != 1 {
			t.Fatalf("panics=%v: held=%d releases=%d after the check, want 0 and 1", panics, held.Load(), releases.Load())
		}
	}
}

// The detector keeps the two halves apart. The fixture is asymmetric on
// purpose: two fanart files with no row (rebuild) and one stale-flag thumb row
// whose file is present (restore), so swapped or summed halves go red.
func TestScanRegistryRepair_ReturnsSplit(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := newRepairService(t, db, dbPath, "")
	dir := filepath.Join(t.TempDir(), "artist")
	writeImage(t, filepath.Join(dir, "folder.jpg"), 100, 100)
	writeImage(t, filepath.Join(dir, "backdrop.jpg"), 100, 100)
	writeImage(t, filepath.Join(dir, "backdrop2.jpg"), 100, 100)
	const id = "88888888-0000-0000-0000-000000000001"
	seedArtist(t, db, id, dir)
	seedImageRow(t, db, id, "thumb", 0, 0, 0) // file present, flag 0 -> restore

	got, err := svc.scanRegistryRepair(context.Background())
	if err != nil {
		t.Fatalf("scanRegistryRepair: %v", err)
	}
	if want := (RegistryRepairPlan{Rebuild: 2, Restore: 1}); got != want {
		t.Fatalf("scan = %+v, want %+v", got, want)
	}
}

func TestRegistryRepairCache_KeepsSplitAndTimestamp(t *testing.T) {
	cache := &RegistryRepairCache{}
	if _, known := cache.Plan(); known {
		t.Fatal("a fresh cache must not know a plan")
	}
	gen, _ := cache.begin()
	cache.finish(gen, RegistryRepairPlan{Rebuild: 2, Restore: 3}, nil)
	plan, known := cache.Plan()
	count, at, ok := cache.Get()
	if !known || plan != (RegistryRepairPlan{Rebuild: 2, Restore: 3}) {
		t.Fatalf("Plan() = %+v, %v; want {2 3}, true", plan, known)
	}
	if !ok || count != 5 || at.IsZero() {
		t.Fatalf("Get() = %d, %v, %v; want 5 (the sum), non-zero, true", count, at, ok)
	}
}

// Anything that is not a finished scan must drop the plan, and an older scan
// finishing after a commit must not bring a stale plan back.
func TestRegistryRepairCache_PlanClearedByRepairErrorAndStaleScan(t *testing.T) {
	seeded := func() *RegistryRepairCache {
		c := &RegistryRepairCache{}
		gen, _ := c.begin()
		c.finish(gen, RegistryRepairPlan{Rebuild: 4, Restore: 1}, nil)
		return c
	}
	c := seeded()
	c.SetFromRepair(2)
	if p, known := c.Plan(); known || p != (RegistryRepairPlan{}) {
		t.Fatalf("after SetFromRepair Plan() = %+v, %v; want none", p, known)
	}
	c = seeded()
	gen, _ := c.begin()
	c.finish(gen, RegistryRepairPlan{}, errors.New("boom"))
	if _, known := c.Plan(); known {
		t.Fatal("a failed scan left the plan known")
	}
	// An older scan (begun before the commit) finishing after it.
	c = &RegistryRepairCache{}
	gen, _ = c.begin()
	c.SetFromRepair(0)
	c.finish(gen, RegistryRepairPlan{Rebuild: 9, Restore: 9}, nil)
	if p, known := c.Plan(); known {
		t.Fatalf("stale scan restored plan %+v after a commit", p)
	}
	if count, _, _ := c.Get(); count != 0 {
		t.Fatalf("stale scan overwrote the commit's count: %d", count)
	}
}
