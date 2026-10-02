package maintenance

// registry_repair_check_test.go -- the cached registry-repair detector (#2678).

import (
	"context"
	"errors"
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
		<-ctx.Done()
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
}
