package maintenance

// dupimage_counts_test.go -- the periodic duplicate-image count refresh (#2608).

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/dupimages"
)

// newDupCountService builds a Service with no DB. The refresh loop is a pure
// scheduler -- it owns cadence, not computation -- so it never touches the DB;
// passing nil makes any accidental DB access panic loudly instead of quietly
// working against a throwaway database.
func newDupCountService(t *testing.T) *Service {
	t.Helper()
	return NewService(nil, "", "",
		slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})))
}

// waitForTimeout bounds every wait below. Generous: these loops tick in
// milliseconds, so a multi-second ceiling only trips on a genuine hang.
const waitForTimeout = 3 * time.Second

// waitFor polls cond until it holds or waitForTimeout elapses.
func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	return waitForWithin(waitForTimeout, cond)
}

// waitForWithin polls cond until it holds or d elapses. Used where the DEADLINE
// itself is the assertion (proving something happened promptly, not merely
// eventually); waitFor's multi-second ceiling is too loose for that.
func waitForWithin(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

// countingHandler counts ERROR records carrying an exact message, so a test can
// assert that a specific operator-facing log line was actually emitted (and how
// often) rather than inferring it from cache state.
type countingHandler struct {
	want string
	n    atomic.Int32
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelError && r.Message == h.want {
		h.n.Add(1)
	}
	return nil
}

func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *countingHandler) WithGroup(string) slog.Handler { return h }

func (h *countingHandler) count() int { return int(h.n.Load()) }

// The loop runs once after the startup delay and then on every tick.
func TestStartDuplicateImageCountRefresh_RunsOnStartupThenOnInterval(t *testing.T) {
	svc := newDupCountService(t)
	cache := dupimages.New(svc.logger)

	var calls atomic.Int32
	cache.SetSources(func(context.Context) (int, error) {
		return int(calls.Add(1)), nil
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.StartDuplicateImageCountRefresh(ctx, cache, 20*time.Millisecond, time.Millisecond)

	if !waitFor(t, func() bool { return calls.Load() >= 1 }) {
		t.Fatal("startup refresh never ran")
	}
	if !waitFor(t, func() bool { return calls.Load() >= 3 }) {
		t.Fatalf("periodic refresh stalled after %d runs", calls.Load())
	}
	if got := cache.Get(); !got.Computed || got.Library == 0 {
		t.Fatalf("cache not populated by the loop: %+v", got)
	}
}

// Canceling the context stops the loop; no further scans fire.
func TestStartDuplicateImageCountRefresh_StopsOnContextCancel(t *testing.T) {
	svc := newDupCountService(t)
	cache := dupimages.New(svc.logger)

	var calls atomic.Int32
	cache.SetSources(func(context.Context) (int, error) { calls.Add(1); return 1, nil }, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.StartDuplicateImageCountRefresh(ctx, cache, 10*time.Millisecond, time.Millisecond)
		close(done)
	}()

	if !waitFor(t, func() bool { return calls.Load() >= 1 }) {
		t.Fatal("startup refresh never ran")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("loop did not return after context cancel")
	}

	settled := calls.Load()
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != settled {
		t.Fatalf("scan ran %d more times after cancel", got-settled)
	}
}

// Canceling DURING the startup delay must return without ever scanning.
func TestStartDuplicateImageCountRefresh_CancelDuringStartupDelay(t *testing.T) {
	svc := newDupCountService(t)
	cache := dupimages.New(svc.logger)

	var calls atomic.Int32
	cache.SetSources(func(context.Context) (int, error) { calls.Add(1); return 1, nil }, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.StartDuplicateImageCountRefresh(ctx, cache, time.Hour, 5*time.Second)
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("loop did not return while waiting out the startup delay")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("scan ran %d times despite cancel during the startup delay", got)
	}
}

// A nil cache is a wiring bug: return immediately rather than panicking or
// silently looping forever over nothing.
func TestStartDuplicateImageCountRefresh_NilCacheReturns(t *testing.T) {
	svc := newDupCountService(t)

	done := make(chan struct{})
	go func() {
		svc.StartDuplicateImageCountRefresh(context.Background(), nil, time.Millisecond, time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("loop did not return on a nil cache")
	}
}

// A scan error must not kill the loop -- a transient platform outage should
// not permanently stop count refreshes.
//
// F4 REGRESSION: this test used to assert ONLY that the next tick recovered,
// which left the damaging in-between snapshot unchecked. The startup refresh
// failing is the ordinary boot-order case (Stillwater up, Emby/the mount still
// coming up), and the pre-fix code stamped that failure Computed=true on
// zeros. Computed is the sole gate on the nav handler's lazy retry, so the
// sidebar then read authoritative-clean -- on data that had never once scanned
// successfully -- and nothing retried it.
//
// So the snapshot BETWEEN the failure and the recovery is now asserted too:
// the second call blocks until the test has inspected it, which makes the
// observation deterministic rather than a race against the next tick.
// The loop must tolerate a cache that has NO sources installed at all, which is
// distinct from the failing-source case below. This is a real shipping state,
// not a hypothetical: the scan sources are installed by the API router, so
// between process start and router construction -- and for the whole of the
// foundation slice of #2608, where the router half has not landed yet -- the
// loop drives a source-less cache on every tick.
//
// Two properties matter. The loop must not die or spin (it logs the error and
// keeps its cadence), and it must never latch Computed: a source-less cache
// that reported itself computed would render as authoritative-clean and
// permanently disable the lazy retry. Recovery once sources appear is what
// proves the loop was still alive rather than silently stopped.
func TestStartDuplicateImageCountRefresh_SurvivesCacheWithNoSources(t *testing.T) {
	svc := newDupCountService(t)

	// The cache logs the source-less skip, so counting that exact line is what
	// makes the no-source window OBSERVABLE. It also pins the operator-facing
	// text, which is the only signal an unwired cache produces.
	sourceless := &countingHandler{want: "duplicate-image count refresh skipped: no scan sources installed"}
	cache := dupimages.New(slog.New(sourceless))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.StartDuplicateImageCountRefresh(ctx, cache, 5*time.Millisecond, time.Millisecond)

	// Assert the source-less window is actually EXERCISED before inspecting the
	// snapshot. The !Computed check below is satisfied by the zero-value Counts
	// whether the loop ticked ten times, once, or never, so on its own it proves
	// nothing about this test's stated subject: a startup delay long enough to
	// skip the window entirely, or a reordered guard in Refresh that never
	// reaches the no-source branch, would gut the coverage while staying green.
	//
	// The deadline is the assertion. With a 1ms startup delay and a 5ms tick,
	// two attempts are due by ~6ms; 100ms is ~16x that margin while still being
	// far below any startup delay that would skip the window.
	const sourcelessWindow = 100 * time.Millisecond
	if !waitForWithin(sourcelessWindow, func() bool { return sourceless.count() >= 2 }) {
		t.Fatalf("only %d source-less refresh attempts within %s (want >=2); the loop never exercised the no-source window this test exists to cover",
			sourceless.count(), sourcelessWindow)
	}

	if got := cache.Get(); got.Computed {
		t.Fatalf("a source-less refresh marked the snapshot Computed (%+v); the sidebar now reads authoritative-clean and the lazy retry is disabled", got)
	}

	// Sources arrive the way the router installs them, proving the loop kept
	// ticking through the no-source window instead of exiting.
	cache.SetSources(func(context.Context) (int, error) { return 7, nil }, nil)

	if !waitFor(t, func() bool { return cache.Get().Library == 7 }) {
		t.Fatalf("loop did not pick up sources installed after startup (cache=%+v); it stopped ticking during the no-source window", cache.Get())
	}
	if got := cache.Get(); !got.Computed {
		t.Fatalf("a successful refresh left the snapshot un-computed: %+v", got)
	}
}

// Every SCHEDULED refresh must run under a per-run deadline, not the loop's
// process-lifetime context. Without one, a single stalled sweep blocks the loop
// forever and the ticker's subsequent ticks are dropped, so every later refresh
// is lost too -- not just the stalled one.
//
// The source inspects the context it is actually handed, which is the only way
// to observe the bound: the deadline itself is 30 minutes out, so a test cannot
// wait for it to fire.
func TestStartDuplicateImageCountRefresh_BoundsEachRunWithADeadline(t *testing.T) {
	svc := newDupCountService(t)
	cache := dupimages.New(svc.logger)

	var (
		calls       atomic.Int32
		sawDeadline atomic.Bool
		unbounded   atomic.Bool
		cancels     = make(chan struct{}, 4)
	)
	cache.SetSources(func(ctx context.Context) (int, error) {
		calls.Add(1)
		if _, ok := ctx.Deadline(); ok {
			sawDeadline.Store(true)
		} else {
			unbounded.Store(true)
		}
		// Record that this run's context is cancelable from the outside --
		// the shutdown path a stalled sweep depends on.
		go func() {
			<-ctx.Done()
			select {
			case cancels <- struct{}{}:
			default:
			}
		}()
		return 1, nil
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.StartDuplicateImageCountRefresh(ctx, cache, 10*time.Millisecond, time.Millisecond)

	if !waitFor(t, func() bool { return calls.Load() >= 2 }) {
		t.Fatalf("scheduled refresh ran only %d times", calls.Load())
	}
	if unbounded.Load() {
		t.Fatal("a scheduled refresh received a context with NO deadline; a stalled sweep would block the loop forever and drop every later tick")
	}
	if !sawDeadline.Load() {
		t.Fatal("no scheduled refresh observed a deadline")
	}

	// Each run's context is canceled when the run returns, so nothing leaks
	// per tick.
	select {
	case <-cancels:
	case <-time.After(waitForTimeout):
		t.Fatal("a completed scheduled run left its context uncanceled (per-tick leak)")
	}
}

// A tick that collides with an in-flight lazy refresh is a SKIP, not a failure:
// it must not be logged as an error, and the loop must keep its cadence.
func TestStartDuplicateImageCountRefresh_InFlightCollisionIsNotAnError(t *testing.T) {
	// The counting handler must be the SERVICE's logger: the error line under
	// test is emitted by the scheduler (s.logger), not by the cache.
	failures := &countingHandler{want: "duplicate-image count refresh failed"}
	svc := NewService(nil, "", "", slog.New(failures))
	cache := dupimages.New(svc.logger)

	release := make(chan struct{})
	var calls atomic.Int32
	var once sync.Once
	entered := make(chan struct{})
	cache.SetSources(func(context.Context) (int, error) {
		calls.Add(1)
		once.Do(func() { close(entered) })
		// Bounded park: if the single-flight latch regresses, the second
		// (scheduled) sweep that should never have started parks here too, and
		// an unbounded wait would deadlock the test instead of failing the
		// assertion that names the bug.
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		return 5, nil
	}, nil)

	// A lazy refresh takes the latch first and parks inside the scan.
	cache.TriggerRefresh()
	select {
	case <-entered:
	case <-time.After(waitForTimeout):
		t.Fatal("lazy refresh never entered the scan")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.StartDuplicateImageCountRefresh(ctx, cache, 5*time.Millisecond, time.Millisecond)

	// Several ticks collide with the in-flight lazy scan. None may start a
	// second sweep, and none may log an error.
	time.Sleep(60 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("scan ran %d times; a scheduled tick started a second concurrent sweep", got)
	}
	if got := failures.count(); got != 0 {
		t.Fatalf("%d dropped ticks logged as ERRORS; an already-running refresh is a skip, not a failure", got)
	}

	close(release)
	if !waitFor(t, func() bool { return cache.Get().Library == 5 }) {
		t.Fatalf("cache never populated (calls=%d, cache=%+v)", calls.Load(), cache.Get())
	}
}

func TestStartDuplicateImageCountRefresh_SurvivesScanError(t *testing.T) {
	svc := newDupCountService(t)
	cache := dupimages.New(svc.logger)

	var calls atomic.Int32
	hold := make(chan struct{})
	cache.SetSources(func(context.Context) (int, error) {
		if calls.Add(1) == 1 {
			return 0, context.DeadlineExceeded
		}
		<-hold
		return 4, nil
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.StartDuplicateImageCountRefresh(ctx, cache, 10*time.Millisecond, time.Millisecond)

	// calls >= 2 means the FIRST refresh has completed (failing) and the second
	// is parked on hold, so what we read now is the post-failure snapshot.
	if !waitFor(t, func() bool { return calls.Load() >= 2 }) {
		t.Fatalf("second refresh never started (calls=%d)", calls.Load())
	}
	if got := cache.Get(); got.Computed {
		t.Fatalf("a wholly failed refresh marked the snapshot Computed (%+v); the sidebar now reads authoritative-clean and the lazy retry is disabled", got)
	}

	close(hold)
	if !waitFor(t, func() bool { return cache.Get().Library == 4 }) {
		t.Fatalf("loop did not recover after a failed scan (calls=%d, cache=%+v)", calls.Load(), cache.Get())
	}
	if got := cache.Get(); !got.Computed {
		t.Fatalf("a recovered refresh left the snapshot un-computed: %+v", got)
	}
}
