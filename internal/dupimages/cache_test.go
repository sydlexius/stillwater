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
	"strings"
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

// singleFlightStallCap bounds how long a deliberately-parked scan stays parked
// in the single-flight tests below.
//
// The cap is what makes those tests fail FAST when the latch regresses. Without
// it, a second sweep that should never have started parks on the same
// unclosed channel and the test deadlocks, surfacing only as the package
// timeout minutes later instead of as the assertion that actually explains the
// bug. It is far longer than the microseconds the tests need, so it never fires
// on the passing path.
const singleFlightStallCap = 5 * time.Second

// heldUntil parks until release is closed, or singleFlightStallCap elapses.
func heldUntil(release <-chan struct{}) {
	select {
	case <-release:
	case <-time.After(singleFlightStallCap):
	}
}

// Get must not hand out an alias of the cached platform slice: the struct copy
// it returns shares its backing array, so a caller that sorted or rewrote the
// returned slice would mutate cached state outside the lock.
func TestGet_ReturnedPlatformSliceIsIndependent(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	c.Set(Counts{Library: 1, Platforms: []PlatformCount{emby(4), jellyfin(6)}})

	got := c.Get()
	if len(got.Platforms) != 2 {
		t.Fatalf("Platforms = %+v; want 2 entries", got.Platforms)
	}
	// Mutate in place the two ways a real consumer would: rewrite an element,
	// and reorder.
	got.Platforms[0].Count = 999
	got.Platforms[0], got.Platforms[1] = got.Platforms[1], got.Platforms[0]

	after := c.Get()
	if after.Platforms[0].Type != "emby" || after.Platforms[0].Count != 4 {
		t.Fatalf("cached Platforms[0] = %+v; want emby/4 -- mutating the slice returned by Get changed cached state", after.Platforms[0])
	}
	if after.Platforms[1].Type != "jellyfin" || after.Platforms[1].Count != 6 {
		t.Fatalf("cached Platforms[1] = %+v; want jellyfin/6", after.Platforms[1])
	}
}

// The single-flight latch must cover the DIRECT Refresh the maintenance
// scheduler calls, not just TriggerRefresh. Before this was shared, a sidebar
// poll landing during a minutes-long scheduled scan saw running == false and
// launched a second concurrent full sweep.
func TestRefresh_SingleFlightCoversDirectAndLazyPaths(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	c.SetSources(
		func(context.Context) (int, error) {
			calls.Add(1)
			once.Do(func() { close(entered) })
			heldUntil(release)
			return 1, nil
		},
		nil,
	)

	// A direct (scheduler-style) Refresh is in flight...
	direct := make(chan error, 1)
	go func() { direct <- c.Refresh(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("direct Refresh never entered the scan")
	}

	// ...so a burst of lazy sidebar triggers must all drop.
	for range 20 {
		c.TriggerRefresh()
	}
	// ...and so must a second direct Refresh.
	if err := c.Refresh(context.Background()); !errors.Is(err, ErrRefreshInFlight) {
		t.Fatalf("concurrent direct Refresh err = %v; want ErrRefreshInFlight", err)
	}

	// Give any escaped goroutine a real chance to start a second sweep before
	// the assertion; without the shared latch this window is where it happens.
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("scan ran %d times while one refresh was in flight; want exactly 1", got)
	}

	close(release)
	if err := <-direct; err != nil {
		t.Fatalf("direct Refresh: %v", err)
	}
	if got := c.Get().Library; got != 1 {
		t.Fatalf("Library = %d; want 1 -- the in-flight refresh did not store its result", got)
	}
}

// The converse ordering: a lazy refresh holds the latch, so the scheduler's
// direct Refresh drops instead of starting a second sweep.
func TestRefresh_DropsWhileLazyRefreshRunning(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	c.SetSources(
		func(context.Context) (int, error) {
			calls.Add(1)
			once.Do(func() { close(entered) })
			heldUntil(release)
			return 2, nil
		},
		nil,
	)

	c.TriggerRefresh()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("lazy refresh never entered the scan")
	}

	if err := c.Refresh(context.Background()); !errors.Is(err, ErrRefreshInFlight) {
		t.Fatalf("direct Refresh during a lazy refresh: err = %v; want ErrRefreshInFlight", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("scan ran %d times; the direct Refresh started a second sweep", got)
	}

	close(release)
}

// A direct Refresh must not arm the LAZY cooldown: lastAttempt means "when the
// lazy path last started", and a scheduled run stamping it would suppress the
// cold-cache warm-up an operator is waiting on.
func TestRefresh_DirectRunDoesNotArmLazyCooldown(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	var calls atomic.Int32
	c.SetSources(func(context.Context) (int, error) { calls.Add(1); return 3, nil }, nil)

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	c.TriggerRefresh()

	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("scan ran %d times; want 2 -- a direct Refresh armed the lazy retry cooldown", got)
	}
}

// The LAZY retry cooldown must actually suppress a second TriggerRefresh.
//
// This is the positive half of the pair above, and it had NO coverage: deleting
// the whole cooldown check from beginBackgroundRefresh left the package green.
// The behavior moved from beginRefresh into beginBackgroundRefresh during the
// #2977 fix, and a moved behavior with no test is exactly what a fix round
// should catch -- a later refactor could drop it silently, and the consequence
// is the ~100% duty cycle TriggerRefresh's doc describes: a persistently failing
// source re-triggered by the very next 60s sidebar poll, forever.
//
// The first trigger is DRAINED before the second, so the single-flight latch is
// provably clear and the cooldown is the only thing that can suppress it.
// Without that step the test would pass even with no cooldown at all, because
// the latch alone would drop the second call.
func TestTriggerRefresh_CooldownSuppressesTheSecondLazyScan(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	var calls atomic.Int32
	scanned := make(chan struct{}, 4)
	c.SetSources(func(context.Context) (int, error) {
		calls.Add(1)
		scanned <- struct{}{}
		return 3, nil
	}, nil)

	c.TriggerRefresh()
	select {
	case <-scanned:
	case <-time.After(5 * time.Second):
		t.Fatal("precondition: the first lazy trigger never reached the source")
	}
	if err := c.Drain(drainCtx(t)); err != nil {
		t.Fatalf("Drain after the first trigger: %v", err)
	}
	// Precondition: the latch is clear, so ONLY the cooldown can suppress the
	// second trigger. Without this the assertion below would hold vacuously.
	if c.refreshRunning() {
		t.Fatal("precondition: the single-flight latch is still held, so the cooldown is not what is being tested")
	}

	c.TriggerRefresh()
	select {
	case <-scanned:
		t.Fatalf("the second lazy trigger scanned %v after the first; retryCooldown (%v) did not suppress it", calls.Load(), retryCooldown)
	case <-time.After(200 * time.Millisecond):
		// Correct: suppressed by the cooldown.
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("scan ran %d times; want 1 -- the lazy retry cooldown did not suppress the second trigger", got)
	}
}

// RefreshContext is the shared per-run bound. Both refresh paths must use it,
// so it has to carry the deadline AND stay derived from its parent (shutdown
// must still abort an in-flight run immediately).
func TestRefreshContext_BoundsAndInheritsCancellation(t *testing.T) {
	t.Parallel()

	parent, cancelParent := context.WithCancel(context.Background())
	ctx, cancel := RefreshContext(parent)
	defer cancel()

	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("RefreshContext returned a context with no deadline")
	}
	if remaining := time.Until(dl); remaining <= 0 || remaining > refreshTimeout {
		t.Fatalf("deadline is %s out; want (0, %s]", remaining, refreshTimeout)
	}

	cancelParent()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("canceling the parent did not cancel the refresh context")
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

// --- Background-refresh lifecycle (#2977) -------------------------------
//
// TriggerRefresh used to spawn a goroutine on context.Background(): nothing
// could cancel it and nothing could wait for it. In production that let a scan
// outlive process shutdown and query a closing database; in tests it let a scan
// started by one test's HTTP request run on into the next test and race the
// state that test was writing, failing a dozen unrelated tests at once. The
// tests below pin both halves of the fix: the refresh is AWAITABLE (Drain
// joins it) and CANCELABLE (Drain stops it).

// drainDeadline bounds every Drain in these tests. Long enough that a healthy
// drain never trips it, short enough that a regression surfaces as a named
// assertion rather than as the package timeout.
const drainDeadline = 10 * time.Second

// refreshRunning reports the single-flight latch's state. Test-only: a drained
// cache that still reports true would silently swallow the next refresh.
func (c *Cache) refreshRunning() bool {
	c.inFlight.Lock()
	defer c.inFlight.Unlock()
	return c.running
}

// drainCtx returns a context bounded by drainDeadline.
func drainCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), drainDeadline)
	t.Cleanup(cancel)
	return ctx
}

// Drain must not return while a TriggerRefresh goroutine is still running.
// This is the awaitability half: without the WaitGroup, Drain returns
// immediately and the caller is told "quiescent" while a scan is mid-flight.
func TestDrain_WaitsForInFlightTriggerRefresh(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	entered := make(chan struct{})
	release := make(chan struct{})
	var finished atomic.Bool
	c.SetSources(
		func(context.Context) (int, error) {
			close(entered)
			// Park until the test releases us, IGNORING ctx cancellation on
			// purpose: this test is about Drain WAITING, not about Drain
			// canceling (that is TestDrain_CancelsInFlightRefresh). A source
			// that returned early on cancellation would let Drain finish for
			// the wrong reason.
			heldUntil(release)
			finished.Store(true)
			return 7, nil
		},
		nil,
	)

	c.TriggerRefresh()
	<-entered // precondition: the scan really is in flight

	ctx := drainCtx(t) // built on the test goroutine; t.Cleanup is not goroutine-safe
	drained := make(chan error, 1)
	go func() { drained <- c.Drain(ctx) }()

	// Drain must still be blocked while the scan is parked.
	select {
	case err := <-drained:
		t.Fatalf("Drain returned (%v) while the refresh goroutine was still running", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	if err := <-drained; err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if !finished.Load() {
		t.Fatal("Drain returned before the refresh goroutine finished its work")
	}
	// The single-flight latch must be clear after a drain, or the next
	// caller's refresh is silently swallowed and its test passes vacuously.
	if c.refreshRunning() {
		t.Fatal("single-flight latch still held after Drain; the next refresh would be dropped")
	}
}

// Drain must CANCEL the in-flight scan, not merely wait it out. This is the
// half that fails against the old context.Background() goroutine: a scan that
// honors its context never learns a shutdown began, so Drain blocks until the
// scan's own 30-minute deadline instead of returning promptly.
func TestDrain_CancelsInFlightRefresh(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	entered := make(chan struct{})
	var sawCancel atomic.Bool
	c.SetSources(
		func(ctx context.Context) (int, error) {
			close(entered)
			// A context-aware scan: parks until ITS context dies. Nothing else
			// ever releases it, so the only way this test finishes is if Drain
			// propagated the cancellation.
			select {
			case <-ctx.Done():
				sawCancel.Store(true)
				return 0, ctx.Err()
			case <-time.After(singleFlightStallCap):
				return 0, errors.New("scan was never canceled")
			}
		},
		nil,
	)

	c.TriggerRefresh()
	<-entered

	if err := c.Drain(drainCtx(t)); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if !sawCancel.Load() {
		t.Fatal("the background refresh context was never canceled; it is not derived from the cache shutdown context")
	}
}

// A Drain whose context expires first reports ctx.Err() rather than hanging --
// the shutdown sequence has to be able to move on.
func TestDrain_ReturnsContextErrorWhenTheWaitOutlastsIt(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	c.SetSources(
		func(context.Context) (int, error) {
			close(entered)
			heldUntil(release) // deliberately ignores cancellation
			return 1, nil
		},
		nil,
	)

	c.TriggerRefresh()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain err = %v; want context.DeadlineExceeded", err)
	}
}

// Drain on an idle cache, and Drain after Drain, must both return nil
// immediately rather than panicking or blocking. Shutdown paths call it
// unconditionally.
//
// HONEST SCOPE: this is a CRASH/HANG guard, not a behavior assertion, and it
// has no teeth against a logic change. It survived every mutation tried against
// the drain, because "returns nil on an idle cache" is true of almost any
// implementation. What it does catch is real but narrow: a nil shutdownCancel
// deref (Drain calls cancel unconditionally, so a constructor that forgot to
// install one panics here), and a Drain that blocks forever when there is
// nothing to wait for. Keep it for those; do not read a pass here as evidence
// that draining works. TestDrain_WaitsForInFlightTriggerRefresh and
// TestTriggerRefresh_CannotEscapeAConcurrentDrain are where that is asserted.
func TestDrain_IsIdempotentAndSafeWhenIdle(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	for i := range 3 {
		if err := c.Drain(drainCtx(t)); err != nil {
			t.Fatalf("Drain #%d on an idle cache: %v", i+1, err)
		}
	}
}

// A TriggerRefresh issued after a Drain must not panic, hang, or wedge the
// single-flight latch. It starts on an already-canceled context, so it gives
// up immediately -- and crucially it RELEASES the latch on the way out.
func TestTriggerRefresh_AfterDrainDoesNotWedgeTheLatch(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	var sawLiveContext atomic.Bool
	c.SetSources(
		func(ctx context.Context) (int, error) {
			if ctx.Err() == nil {
				sawLiveContext.Store(true)
			}
			return 1, ctx.Err()
		},
		nil,
	)

	if err := c.Drain(drainCtx(t)); err != nil {
		t.Fatalf("initial Drain: %v", err)
	}

	c.TriggerRefresh()
	// Joinable even post-drain: the WaitGroup still tracks it.
	if err := c.Drain(drainCtx(t)); err != nil {
		t.Fatalf("Drain after a post-drain TriggerRefresh: %v", err)
	}
	if sawLiveContext.Load() {
		t.Fatal("a post-drain refresh ran on a live context; the shutdown cancellation did not reach it")
	}
	if c.refreshRunning() {
		t.Fatal("single-flight latch still held after a post-drain TriggerRefresh")
	}
}

// A Reset whose drain TIMES OUT must fail loudly, not carry on.
//
// WHY THIS MATTERS MORE THAN IT LOOKS. The expiry branch used to log at Error
// and then proceed: fresh context, zeroed counts, nil sources. But the
// goroutine that outlived the drain still holds the source funcs it captured at
// refresh() entry, and it will write its result into the cache the NEXT test is
// using. Nothing failed; the only trace was one stderr line nobody reads. That
// degrades to a GREEN test over the exact bug this package's drain exists to
// prevent, so the signal has to be one a test runner cannot ignore.
//
// Reset is test-only (production never calls it), so a panic is the right
// loudness: inside a test it is a named, stack-carrying failure.
func TestReset_PanicsWhenTheDrainTimesOut(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())
	// Shorten THIS cache's drain bound. A field rather than a package-level
	// var, so this test shares no mutable state with the parallel tests around
	// it and needs no serialization to be correct.
	c.drainTimeout = 20 * time.Millisecond

	release := make(chan struct{})
	entered := make(chan struct{})
	c.SetSources(
		func(context.Context) (int, error) {
			close(entered)
			// Park PAST the shortened drain timeout, and ignore cancellation --
			// this stands in for a scan wedged in a syscall, which is the only
			// way the timeout can genuinely fire.
			heldUntil(release)
			return 1, nil
		},
		nil,
	)
	// Release it at the end whatever happens, so the goroutine cannot outlive
	// the test it belongs to.
	t.Cleanup(func() { close(release) })

	c.TriggerRefresh()
	<-entered

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Reset returned normally after its drain timed out; a live goroutine is still holding the previous owner's sources and will write into the next test's cache (#2977)")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "drain timed out") {
			t.Fatalf("panic value = %v; want a message naming the timed-out drain", r)
		}
	}()
	c.Reset()
}

// Reset must never break single-flight for a refresh that took the latch
// LEGITIMATELY after its drain completed.
//
// THE BUG THIS GUARDS. A revision of Reset cleared c.running unconditionally as
// "defense in depth". Drain only waits for refreshes registered BEFORE it
// started, so a TriggerRefresh landing after the drain and before Reset returns
// owns the latch with nothing wrong anywhere -- and the blind clear erased it
// from single-flight, so the NEXT TriggerRefresh launched a second concurrent
// full scan. That is the doubled I/O and CPU on the most expensive task in the
// process that beginRefresh's latch exists to prevent.
//
// THE SHAPE IS THE POINT. This models what dupNavRouter/dupNavStubSources
// actually do: Reset, then the next owner REINSTALLS sources, then triggers.
// Without the reinstall the bug is invisible, which is why the original suite
// missed it -- concurrency is only half of it, the source lifecycle is the
// other half.
func TestReset_DoesNotBreakSingleFlightForALaterRefresh(t *testing.T) {
	t.Parallel()

	var (
		concurrent atomic.Int32
		maxSeen    atomic.Int32
	)
	src := func(ctx context.Context) (int, error) {
		n := concurrent.Add(1)
		for {
			old := maxSeen.Load()
			if n <= old || maxSeen.CompareAndSwap(old, n) {
				break
			}
		}
		// Hold briefly so a genuinely concurrent second scan overlaps rather
		// than tidily following this one.
		time.Sleep(time.Millisecond)
		concurrent.Add(-1)
		return 1, ctx.Err()
	}

	for i := range 400 {
		c := New(quietLogger())
		c.SetSources(src, nil)

		// A trigger racing Reset: some iterations land before the drain, some
		// after it. The ones landing AFTER are the interesting ones.
		go c.TriggerRefresh()
		c.Reset()

		// The next owner reinstalls sources and triggers, exactly as the api
		// test helpers do.
		c.SetSources(src, nil)
		c.TriggerRefresh()

		if got := maxSeen.Load(); got > 1 {
			t.Fatalf("iteration %d: SINGLE-FLIGHT BROKEN: %d concurrent scans after Reset (the latch was cleared while a refresh legitimately held it)", i, got)
		}
		if err := c.Drain(drainCtx(t)); err != nil {
			t.Fatalf("iteration %d: Drain: %v", i, err)
		}
	}
	if maxSeen.Load() == 0 {
		t.Fatal("precondition: no scan ever ran, so single-flight was never exercised")
	}
}

// A PANIC inside Drain must not leave c.inFlight held.
//
// WHY THIS IS WORTH A TEST. Drain takes c.inFlight around the cancel (that is
// what makes the drain airtight against a concurrent TriggerRefresh, see
// beginBackgroundRefresh). An earlier revision unlocked POSITIONALLY rather
// than with a defer, so a cancel that panicked -- a nil shutdownCancel on a
// Cache built as &Cache{} instead of via New -- skipped the unlock and left the
// lock held FOREVER. Every later Drain, Refresh, TriggerRefresh and
// endBackgroundRefresh then blocked on it: one goroutine's panic escalated into
// a package-wide deadlock. A panic must never be able to strand a lock.
//
// The test recovers the panic and then proves the cache is still USABLE, which
// is the property that matters; asserting on the panic alone would pass against
// the stranding version too.
func TestDrain_PanickingCancelDoesNotStrandTheLatch(t *testing.T) {
	t.Parallel()
	// Deliberately NOT built by New: no shutdownCancel, so cancel() panics.
	// This is the "misconstructed cache" path, and it is exactly the shape that
	// turns a positional unlock into a deadlock.
	c := &Cache{logger: quietLogger(), drainTimeout: defaultDrainTimeout}

	func() {
		defer func() {
			if recover() == nil {
				t.Error("precondition: Drain on a cache with no shutdownCancel did not panic, so this test is not exercising the stranding path")
			}
		}()
		_ = c.Drain(context.Background())
	}()

	// The lock must be free. Probed on another goroutine with a timeout,
	// because the failure mode is a BLOCK -- asserting it inline would hang the
	// package instead of failing this test.
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.beginRefresh() // takes and holds c.inFlight briefly
		c.endRefresh()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("c.inFlight was left HELD by the panicking Drain; every later Drain/Refresh/TriggerRefresh would block forever")
	}
}

// A refresh must never become invisible to a concurrent drain.
//
// THE HOLE THIS GUARDS. Taking the single-flight latch, registering as
// drainable and reading the shutdown context were once three separate steps. A
// Reset landing in the gap between the first and the second saw a registered
// count of zero, so its drain returned "everything has finished" about a
// refresh that had not started -- and then installed a FRESH, LIVE shutdown
// context that the escaping goroutine picked up, so it ran the NEXT owner's
// sources on a context nobody would ever cancel. That is #2977 through a
// narrower window.
//
// HOW IT IS DETECTED. The invariant is expressed in terms an escape must
// violate: once Reset has RETURNED, no source may still be entering on a LIVE
// context. The source records exactly that pairing. It is checked from inside
// the source rather than from a post-hoc flag because the escape is precisely a
// goroutine the test has no handle on.
//
// WHY A LOOP, AND WHY THE TRIGGER IS UNSYNCHRONIZED. The window is a few
// instructions wide, so the trigger must be left to race Reset freely --
// waiting for TriggerRefresh to RETURN would guarantee the registration already
// happened and put the window out of reach, testing nothing. The iteration
// count is what gives this teeth; the property itself is exact, not
// statistical. (During development the defect was ALSO reproduced
// deterministically by widening the window with a temporary hook, which is what
// proved the fix rather than this loop.)
//
// NOTE THE DIVISION OF LABOR. This test deliberately does NOT assert on the
// single-flight latch. With an unsynchronized trigger, a refresh that takes the
// latch legitimately AFTER Reset finished is an ordinary interleaving, not a
// bug, so a latch assertion here would fail against correct code -- it did,
// about 1 iteration in 12. The latch property needs the opposite fixture (the
// trigger provably first) and lives in
// TestReset_LeavesNoLatchHeldByADrainedRefresh.
// DETERMINISTIC, NOT STATISTICAL. The test pins the refresh at the exact point
// the bug lived -- latch taken, not yet drainable -- using the
// beforeRegisterHook seam, and runs an entire Reset while it is held there.
// That removes the one ambiguity a racing loop cannot resolve: whether a
// refresh seen running after Reset was an ESCAPEE (registered first, so Drain
// owed it a wait) or an ordinary new refresh that legitimately started
// afterwards. Both look identical from outside, which is why a loop-and-hope
// version of this test failed against correct code roughly 1 iteration in 12.
//
// With the hook, the ordering is a fact rather than an inference, and the
// assertion is exact: Reset must NOT be able to return while this refresh is
// pending, and the refresh must NOT come back on a live context afterwards.
func TestTriggerRefresh_CannotEscapeAConcurrentDrain(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	pinned := make(chan struct{})  // closed once the refresh is held in the window
	release := make(chan struct{}) // closed to let it proceed
	c.beforeRegisterHook = func() {
		close(pinned)
		<-release
	}

	var (
		nextOwnerRan atomic.Bool
		sawLiveCtx   atomic.Bool
	)
	ran := make(chan struct{})
	nextOwnerSrc := func(ctx context.Context) (int, error) {
		nextOwnerRan.Store(true)
		sawLiveCtx.Store(ctx.Err() == nil)
		close(ran)
		return 7, ctx.Err()
	}

	c.SetSources(func(ctx context.Context) (int, error) { return 1, ctx.Err() }, nil)
	go c.TriggerRefresh()
	<-pinned // the refresh now holds the latch and is NOT yet drainable

	resetDone := make(chan struct{})
	go func() {
		c.Reset()
		close(resetDone)
	}()

	// THE CORE ASSERTION. Reset drains, and this refresh is already inside
	// beginBackgroundRefresh, so Reset must NOT be able to complete. If it
	// returns here the refresh was invisible to the drain -- the #2977 escape.
	select {
	case <-resetDone:
		close(release)
		t.Fatal("Reset RETURNED while a TriggerRefresh was pending in the register window; the refresh escaped the drain (#2977)")
	case <-time.After(250 * time.Millisecond):
		// Correct: Reset is blocked waiting for this refresh.
	}

	close(release)
	<-resetDone

	// The NEXT owner installs its sources only now, exactly as dupNavRouter and
	// dupNavStubSources do at the start of the following test. Installing them
	// STRICTLY AFTER Reset returned is what makes the assertion below exact: any
	// call to nextOwnerSrc is necessarily a call that happened after Reset
	// returned, so there is no legitimate interleaving it could be confused
	// with. (Installing them earlier makes the check race the drain and fail
	// against correct code -- an ordinary concurrent run, not an escape.)
	c.SetSources(nextOwnerSrc, nil)

	select {
	case <-ran:
	case <-time.After(500 * time.Millisecond):
	}

	// An escapee is a goroutine Drain was owed and did not wait for. Reaching
	// the NEXT owner's source at all means it outlived Reset; doing so on a LIVE
	// context means it picked up the freshly installed shutdown context, which
	// is what lets it corrupt the next owner's state. That pairing is the #2977
	// defect.
	if nextOwnerRan.Load() {
		t.Fatalf("the pinned refresh ran the NEXT owner's source after Reset returned (live ctx = %v); it escaped the drain (#2977)", sawLiveCtx.Load())
	}
}

// After Reset has drained a refresh, that refresh has RETURNED and released the
// single-flight latch. A latch left held would silently swallow the next
// owner's TriggerRefresh -- their test would then pass while exercising
// nothing, which is the same class of failure as the escape itself.
//
// THE FIXTURE IS THE WHOLE POINT. The trigger must provably complete BEFORE
// Reset starts, or the assertion is unsound: with a racing trigger, a refresh
// that takes the latch legitimately after the drain finished holds it for
// perfectly good reasons. An earlier revision did exactly that -- it closed its
// `started` channel BEFORE calling TriggerRefresh, so waiting on it proved only
// that the goroutine had been SCHEDULED, and the resulting flake (~1 in 12) was
// the assertion being wrong, not the code. Closing AFTER TriggerRefresh returns
// is what makes "the latch is clear" a genuine post-drain guarantee.
//
// Reset itself does not touch c.running; the guarantee comes from the drain.
// See the note in Reset for why clearing it there would break a concurrent
// direct Refresh.
func TestReset_LeavesNoLatchHeldByADrainedRefresh(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	for i := range 200 {
		c.SetSources(func(ctx context.Context) (int, error) { return 1, ctx.Err() }, nil)

		started := make(chan struct{})
		go func() {
			c.TriggerRefresh()
			close(started)
		}()
		<-started // TriggerRefresh has RETURNED: the refresh is registered.

		c.Reset()

		if c.refreshRunning() {
			t.Fatalf("iteration %d: the single-flight latch was still held after Reset drained the refresh that owned it", i)
		}
	}
}

// Reset is the test-isolation entry point, and it has to do BOTH halves: join
// the in-flight refresh (so no goroutine reads state after Reset returns) and
// reinstall a live shutdown context (so the NEXT test's lazy path still
// scans). A Reset that only drained would leave the process-wide cache
// permanently canceled and every later TriggerRefresh a no-op.
func TestReset_DrainsThenLeavesTheCacheUsable(t *testing.T) {
	t.Parallel()
	c := New(quietLogger())

	release := make(chan struct{})
	entered := make(chan struct{})
	var running atomic.Bool
	c.SetSources(
		func(context.Context) (int, error) {
			running.Store(true)
			close(entered)
			heldUntil(release)
			running.Store(false)
			return 5, nil
		},
		nil,
	)

	c.TriggerRefresh()
	<-entered
	if !running.Load() {
		t.Fatal("precondition: the background scan is not running")
	}

	// Reset must not return until that goroutine is done, so releasing it
	// concurrently is the only way this completes.
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(release)
	}()
	c.Reset()

	if running.Load() {
		t.Fatal("Reset returned while a background refresh goroutine was still running")
	}

	// Now prove the cache is LIVE again: a fresh refresh must see an
	// un-canceled context and complete.
	//
	// The source signals entry and the test waits for it BEFORE draining. Drain
	// cancels on entry, so a drain issued before the goroutine reached the
	// source would observe a canceled context for a reason that has nothing to
	// do with Reset -- the assertion would then fail against correct code.
	var sawLiveContext atomic.Bool
	reached := make(chan struct{})
	c.SetSources(
		func(ctx context.Context) (int, error) {
			sawLiveContext.Store(ctx.Err() == nil)
			close(reached)
			return 9, nil
		},
		nil,
	)
	c.TriggerRefresh()
	<-reached
	if err := c.Drain(drainCtx(t)); err != nil {
		t.Fatalf("Drain after Reset: %v", err)
	}
	if !sawLiveContext.Load() {
		t.Fatal("the refresh after Reset ran on a canceled context; Reset did not reinstall a live shutdown context")
	}
	if got := c.Get().Library; got != 9 {
		t.Fatalf("Library = %d after a post-Reset refresh; want 9 -- the cache did not come back to life", got)
	}
}
