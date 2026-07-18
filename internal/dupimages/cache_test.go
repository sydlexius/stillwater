package dupimages

// cache_test.go -- the duplicate-image count cache (#2608).
//
// The property under test that matters most: Get() is a pure cached read. If
// Get ever grew a scan, the sidebar's 60s poll would start a from-disk re-hash
// of the entire library plus a sweep of every connected platform. Several
// tests below assert the scan-call counter stays at zero across Get.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func emby(n int) PlatformCount { return PlatformCount{Type: "emby", Label: "Emby", Count: n} }
func jellyfin(n int) PlatformCount {
	return PlatformCount{Type: "jellyfin", Label: "Jellyfin", Count: n}
}

func TestGet_NeverScans(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	var libCalls, platCalls atomic.Int32
	c.SetSources(
		func(context.Context) (int, error) { libCalls.Add(1); return 7, nil },
		func(context.Context) ([]PlatformCount, error) {
			platCalls.Add(1)
			return []PlatformCount{emby(3)}, nil
		},
	)

	// Cold cache: Get must still not scan.
	got := c.Get()
	if got.Computed {
		t.Fatalf("cold cache reported Computed=true: %+v", got)
	}
	// Hot cache: still no scan.
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	before := libCalls.Load() + platCalls.Load()
	for range 100 {
		_ = c.Get()
	}
	if after := libCalls.Load() + platCalls.Load(); after != before {
		t.Fatalf("Get triggered %d scan calls; Get must never scan", after-before)
	}
}

func TestRefresh_StoresLibraryAndPerPlatformCounts(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())
	c.SetSources(
		func(context.Context) (int, error) { return 12, nil },
		func(context.Context) ([]PlatformCount, error) {
			return []PlatformCount{emby(4), jellyfin(2)}, nil
		},
	)

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	got := c.Get()
	if got.Library != 12 {
		t.Errorf("Library = %d, want 12", got.Library)
	}
	if len(got.Platforms) != 2 {
		t.Fatalf("got %d platform entries, want 2: %+v", len(got.Platforms), got.Platforms)
	}
	if got.Platforms[0].Type != "emby" || got.Platforms[0].Count != 4 {
		t.Errorf("platform[0] = %+v, want emby/4", got.Platforms[0])
	}
	if got.Platforms[1].Label != "Jellyfin" || got.Platforms[1].Count != 2 {
		t.Errorf("platform[1] = %+v, want Jellyfin/2", got.Platforms[1])
	}
	if got.PlatformTotal() != 6 {
		t.Errorf("PlatformTotal() = %d, want 6", got.PlatformTotal())
	}
	if !got.Computed || got.ComputedAt.IsZero() {
		t.Fatalf("snapshot not marked computed: %+v", got)
	}
	if got.Empty() {
		t.Error("Empty() true with offenders present")
	}
}

func TestRefresh_ZeroCountsAreEmpty(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())
	c.SetSources(
		func(context.Context) (int, error) { return 0, nil },
		func(context.Context) ([]PlatformCount, error) { return nil, nil },
	)

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	got := c.Get()
	if !got.Empty() {
		t.Fatalf("zero counts not Empty: %+v", got)
	}
	if !got.Computed {
		t.Fatal("a successful all-zero refresh must still mark Computed, or the handler re-triggers a scan forever")
	}
}

// An entry's PRESENCE paints a row, so a zero-count entry must never survive:
// it would claim a clean platform is dirty.
func TestSet_DropsZeroCountPlatformEntries(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())
	c.Set(Counts{Platforms: []PlatformCount{emby(0), jellyfin(3), {Type: "lidarr", Label: "Lidarr"}}})

	got := c.Get()
	if len(got.Platforms) != 1 {
		t.Fatalf("got %+v, want only the Jellyfin entry", got.Platforms)
	}
	if got.Platforms[0].Type != "jellyfin" || got.Platforms[0].Count != 3 {
		t.Errorf("survivor = %+v, want jellyfin/3", got.Platforms[0])
	}
}

// A platform that has been cleaned must lose its row: an empty (non-error)
// result clears stale entries rather than leaving them to rot.
func TestRefresh_EmptyPlatformResultClearsStaleRows(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	c.SetSources(
		func(context.Context) (int, error) { return 1, nil },
		func(context.Context) ([]PlatformCount, error) { return []PlatformCount{emby(5)}, nil },
	)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("seed Refresh: %v", err)
	}
	if len(c.Get().Platforms) != 1 {
		t.Fatalf("seed did not land: %+v", c.Get())
	}

	c.SetSources(
		func(context.Context) (int, error) { return 1, nil },
		func(context.Context) ([]PlatformCount, error) { return nil, nil },
	)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := c.Get().Platforms; len(got) != 0 {
		t.Fatalf("stale platform rows survived a clean scan: %+v", got)
	}
}

// A failing half must not silently zero its previously-known value: a zero
// renders as "clean", which would be a lie after a transient platform outage.
func TestRefresh_FailedHalfKeepsPreviousValue(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	c.SetSources(
		func(context.Context) (int, error) { return 5, nil },
		func(context.Context) ([]PlatformCount, error) { return []PlatformCount{emby(9)}, nil },
	)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("seed Refresh: %v", err)
	}

	boom := errors.New("platform unreachable")
	c.SetSources(
		func(context.Context) (int, error) { return 6, nil },
		func(context.Context) ([]PlatformCount, error) { return nil, boom },
	)
	if err := c.Refresh(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Refresh err = %v, want %v", err, boom)
	}

	got := c.Get()
	if got.Library != 6 {
		t.Errorf("Library = %d, want the fresh 6", got.Library)
	}
	if len(got.Platforms) != 1 || got.Platforms[0].Count != 9 {
		t.Fatalf("failed platform half dropped the known rows: %+v", got.Platforms)
	}
}

// partialErr is what a source returns when its report carried ScanErrors > 0.
func partialErr() error {
	return fmt.Errorf("skipped 3800 artists: %w", ErrPartialScan)
}

// F1 REGRESSION -- the defect: a platform sweep in which EVERY query failed
// comes back PerArtist-empty with err == nil, so the pre-fix code took the
// success branch and used that empty result to CLEAR the rows, reporting a
// still-dirty platform as clean and logging "refreshed" while doing it.
//
// The known rows must survive. Anything the scan could not verify is not
// evidence of cleanliness.
func TestRefresh_PartialPlatformSweepDoesNotClearKnownRows(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	c.SetSources(
		func(context.Context) (int, error) { return 1, nil },
		func(context.Context) ([]PlatformCount, error) { return []PlatformCount{emby(42)}, nil },
	)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("seed Refresh: %v", err)
	}
	// Precondition: the row we are about to defend actually exists.
	if got := c.Get().Platforms; len(got) != 1 || got[0].Count != 42 {
		t.Fatalf("seed did not land: %+v", got)
	}

	// The outage: every platform query fails, so the sweep returns NOTHING and
	// flags itself partial.
	c.SetSources(
		func(context.Context) (int, error) { return 1, nil },
		func(context.Context) ([]PlatformCount, error) { return nil, partialErr() },
	)
	if err := c.Refresh(context.Background()); !errors.Is(err, ErrPartialScan) {
		t.Fatalf("Refresh err = %v, want it to wrap ErrPartialScan", err)
	}

	got := c.Get().Platforms
	if len(got) != 1 || got[0].Count != 42 {
		t.Fatalf("a partial sweep erased the known duplicate rows and reported the platform clean: %+v", got)
	}
}

// F2 REGRESSION -- the library half. A half-unreachable mount yields a
// confident UNDERCOUNT (only the reachable artists were re-hashed) with
// err == nil, which the pre-fix code cached as fact.
func TestRefresh_PartialLibraryScanDoesNotOverwriteKnownCount(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	c.SetSources(func(context.Context) (int, error) { return 42, nil }, nil)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("seed Refresh: %v", err)
	}
	if got := c.Get().Library; got != 42 {
		t.Fatalf("seed did not land: Library = %d, want 42", got)
	}

	// Half the mount is gone: the scan sees 3 of the 42 and flags itself partial.
	c.SetSources(func(context.Context) (int, error) { return 0, partialErr() }, nil)
	if err := c.Refresh(context.Background()); !errors.Is(err, ErrPartialScan) {
		t.Fatalf("Refresh err = %v, want it to wrap ErrPartialScan", err)
	}

	if got := c.Get().Library; got != 42 {
		t.Fatalf("Library = %d; a partial scan's undercount was cached as authoritative (want the previous 42)", got)
	}
}

// F3 REGRESSION -- LOST UPDATE across a multi-minute window.
//
// Refresh used to snapshot the counts BEFORE its minutes-long scans and write
// that copy back at the end. An operator who remediated and loaded the report
// page mid-scan wrote a correct 0, and the finishing refresh clobbered it back
// to the pre-remediation number for the next 12 hours -- defeating the stated
// "drops to zero the moment a remediation run cleans the library" guarantee.
//
// This is a LOST UPDATE, not a data race: every individual access was already
// mutex-guarded, so -race cannot catch a regression here. Hence this test.
func TestRefresh_DoesNotClobberFresherStoreLandedMidScan(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	c.Set(Counts{Library: 42})

	scanning := make(chan struct{})
	release := make(chan struct{})
	c.SetSources(func(context.Context) (int, error) {
		close(scanning)
		<-release
		// The pre-remediation number this scan started from. It is now stale.
		return 42, nil
	}, nil)

	done := make(chan error, 1)
	go func() { done <- c.Refresh(context.Background()) }()

	<-scanning // the refresh is mid-scan, holding its stale view
	// The operator remediates and loads the report page, which stores a
	// FRESHER, authoritative zero.
	c.StoreLibrary(0)
	if got := c.Get().Library; got != 0 {
		t.Fatalf("precondition: the opportunistic store did not land, Library = %d", got)
	}
	close(release)

	if err := <-done; err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if got := c.Get().Library; got != 0 {
		t.Fatalf("Library = %d; the in-flight refresh clobbered the operator's fresher 0 with its stale scan result", got)
	}
}

// The flip side of F3: a store that predates the refresh must NOT suppress it,
// or the counts would freeze at whatever the last report-page visit saw.
func TestRefresh_OverwritesAStoreThatPredatesIt(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	c.StoreLibrary(7)
	c.SetSources(func(context.Context) (int, error) { return 3, nil }, nil)

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := c.Get().Library; got != 3 {
		t.Fatalf("Library = %d, want the refresh's fresh 3; a stale store must not win", got)
	}
}

// F3 REGRESSION (second shape) -- two one-half stores must not lose each
// other. The Get-mutate-Set spelling read the whole snapshot, changed one
// field and wrote it all back, so an interleaved store of the OTHER half was
// silently reverted.
// Hammered rather than run once: a single pair of concurrent stores almost
// never interleaves inside the read-modify-write window, so a one-shot version
// of this test passes against the buggy Get-mutate-Set spelling and guards
// nothing. Many rounds make the interleave near-certain.
//
// The invariant is monotonicity: neither half may ever go BACKWARDS, which is
// exactly what a lost update looks like from the outside.
func TestStoreHalves_DoNotLoseEachOther(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	const rounds = 2000

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 1; i <= rounds; i++ {
			c.StoreLibrary(i)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 1; i <= rounds; i++ {
			c.StorePlatforms([]PlatformCount{emby(i)})
		}
	}()

	// Sample throughout the run: a lost update is transient, so checking only
	// the final state would miss it.
	var libWorst, platWorst int
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		lastLib, lastPlat := 0, 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			got := c.Get()
			if got.Library < lastLib && lastLib-got.Library > libWorst {
				libWorst = lastLib - got.Library
			}
			if len(got.Platforms) == 1 {
				if got.Platforms[0].Count < lastPlat && lastPlat-got.Platforms[0].Count > platWorst {
					platWorst = lastPlat - got.Platforms[0].Count
				}
				lastPlat = got.Platforms[0].Count
			}
			lastLib = got.Library
		}
	}()
	wg.Wait()
	close(stop)
	<-sampled // the sampler owns libWorst/platWorst until it returns

	if libWorst > 0 {
		t.Errorf("library count went backwards by %d; a concurrent platform store reverted it (lost update)", libWorst)
	}
	if platWorst > 0 {
		t.Errorf("platform count went backwards by %d; a concurrent library store reverted it (lost update)", platWorst)
	}

	got := c.Get()
	if got.Library != rounds {
		t.Errorf("Library = %d, want %d", got.Library, rounds)
	}
	if len(got.Platforms) != 1 || got.Platforms[0].Count != rounds {
		t.Errorf("Platforms = %+v, want one emby/%d entry", got.Platforms, rounds)
	}
}

// F4 REGRESSION -- a refresh that established NEITHER half must not latch
// Computed.
//
// Computed is the sole gate on the nav handler's lazy retry. The pre-fix code
// set it unconditionally, so the boot-order case (Stillwater up, Emby still
// starting, startup refresh fails everything) froze the cache as
// authoritative-clean for the full 12h interval on data that never once
// scanned successfully -- and nothing retried, because the retry was gated on
// the very flag the failure had just set.
func TestRefresh_TotalFailureDoesNotLatchComputed(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	boom := errors.New("emby still starting")
	c.SetSources(
		func(context.Context) (int, error) { return 0, boom },
		func(context.Context) ([]PlatformCount, error) { return nil, boom },
	)

	if err := c.Refresh(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Refresh err = %v, want %v", err, boom)
	}

	got := c.Get()
	if got.Computed {
		t.Fatal("a refresh that established neither half marked the snapshot Computed; the lazy retry is now disabled and the sidebar reads authoritative-clean on data that never scanned")
	}
	if !got.ComputedAt.IsZero() {
		t.Errorf("ComputedAt = %v, want zero on a never-established snapshot", got.ComputedAt)
	}
}

// A refresh where ONE half succeeded is genuinely computed -- that half is
// known, and the handler must not keep re-triggering scans for the other.
func TestRefresh_PartialSuccessStillMarksComputed(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	c.SetSources(
		func(context.Context) (int, error) { return 4, nil },
		func(context.Context) ([]PlatformCount, error) { return nil, errors.New("platform down") },
	)
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh returned nil despite a failed platform half")
	}

	got := c.Get()
	if !got.Computed {
		t.Fatal("a refresh whose library half succeeded left Computed false; the handler would re-scan on every poll")
	}
	if got.Library != 4 {
		t.Errorf("Library = %d, want the established 4", got.Library)
	}
}

func TestRefresh_NoSourcesIsAnError(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh with no sources returned nil; an unwired cache must fail loud, not cache zeros")
	}
	if c.Get().Computed {
		t.Fatal("a no-source refresh marked the snapshot computed")
	}
}

// The cached slice must be insulated from the caller's backing array.
func TestSet_CopiesPlatformSlice(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	src := []PlatformCount{emby(4)}
	c.Set(Counts{Platforms: src})
	src[0].Count = 999

	if got := c.Get().Platforms[0].Count; got != 4 {
		t.Fatalf("cached count = %d; mutating the caller's slice changed the snapshot", got)
	}
}

// TriggerRefresh must return before the scan finishes -- it is the non-blocking
// lazy path used from the render/poll handler.
func TestTriggerRefresh_DoesNotBlockCaller(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	c.SetSources(
		func(context.Context) (int, error) {
			once.Do(func() { close(started) })
			<-release
			return 2, nil
		},
		nil,
	)

	done := make(chan struct{})
	go func() { c.TriggerRefresh(); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("TriggerRefresh blocked on the scan")
	}
	// The snapshot is still cold while the scan runs.
	if c.Get().Computed {
		t.Fatal("snapshot marked computed before the scan finished")
	}
	<-started
	close(release)
}

// A burst of sidebar polls on a cold cache must collapse to ONE scan.
func TestTriggerRefresh_SingleFlight(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	var calls atomic.Int32
	release := make(chan struct{})
	c.SetSources(
		func(context.Context) (int, error) {
			calls.Add(1)
			<-release
			return 1, nil
		},
		nil,
	)

	for range 20 {
		c.TriggerRefresh()
	}
	// Give the first goroutine time to latch, then let it finish.
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(release)

	if got := calls.Load(); got != 1 {
		t.Fatalf("scan ran %d times for 20 concurrent triggers; want exactly 1", got)
	}
}

func TestSet_MarksComputed(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())
	c.Set(Counts{})

	got := c.Get()
	if !got.Computed || got.ComputedAt.IsZero() {
		t.Fatalf("Set did not stamp the snapshot: %+v", got)
	}
}

func TestReset_ClearsSnapshotAndSources(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())
	c.SetSources(func(context.Context) (int, error) { return 3, nil }, nil)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	c.Reset()
	if got := c.Get(); got.Computed || got.Library != 0 || len(got.Platforms) != 0 {
		t.Fatalf("Reset left state behind: %+v", got)
	}
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("Reset did not clear the sources")
	}
}

func TestShared_IsStable(t *testing.T) {
	// Bound through a variable so the comparison is not folded away as an
	// identical-expressions mistake by static analysis.
	first := Shared()
	if second := Shared(); first != second {
		t.Fatal("Shared returned two different caches; the maintenance task and the router would refresh/read different state")
	}
}
