// Package dupimages holds the cached duplicate-image offender counts that back
// the duplicate rows of the sidebar's "Images" section (#2608).
//
// Why a cache exists at all: BOTH underlying counts are expensive. The library
// count comes from rule.Pipeline.ScanFanartDuplicates, which re-hashes every
// artist's fanart FROM DISK; the platform count comes from
// publish.Publisher.ScanPlatformBackdropDuplicates, which queries every
// connected Emby/Jellyfin for every artist. Neither can run on a sidebar
// render -- that is exactly why the pre-#2608 nav links carried no count pill.
//
// So the serving path reads a cached value and NEVER scans:
//
//	Get()            O(1) read under an RLock. No scan, no I/O, no DB.
//	Refresh(ctx)     Blocking full scan. Called ONLY by the background
//	                 maintenance task and by the opportunistic post-scan hook.
//	TriggerRefresh() Fire-and-forget, single-flight. Kicks a background
//	                 Refresh and returns immediately -- this is the "lazy"
//	                 path for a cold cache, and it still does not block the
//	                 caller's render.
//
// The background refresh TriggerRefresh spawns has an owned lifecycle, so
// nothing it does can outlive whoever started it (#2977):
//
//	Drain(ctx)       Cancels every in-flight background refresh and BLOCKS
//	                 until each has returned. Returns ctx.Err() if ctx expires
//	                 first. Called at process shutdown, and by Reset.
//	Reset()          Test-only. DRAINS FIRST -- so it BLOCKS while a refresh is
//	                 in flight -- then clears the snapshot, the sources and the
//	                 single-flight latch, and installs a fresh shutdown context
//	                 so the cache is live for the next test. It PANICS if the
//	                 drain times out, because proceeding would hand the next
//	                 test a cache a live goroutine is still writing into.
//
// A test that shares the process-wide cache MUST call Reset (usually in
// t.Cleanup); a test that spawns a background refresh and does NOT drain it is
// the exact defect #2977 fixed.
//
// Until the first successful Refresh completes, Get returns a zero Counts with
// Computed=false, which renders as "no duplicate rows". That is the intended
// hide-when-unknown behavior from the issue: a not-yet-computed count is
// indistinguishable from clean, and the steady state is clean anyway.
//
// SCOPE: these counts cover the DUPLICATE ROWS only (Library Duplicates and one
// row per offending platform). They say nothing about the Unmatched row, whose
// count is not part of this snapshot.
//
// So these counts are NECESSARY BUT NOT SUFFICIENT for hiding the Images
// section. The section hides only when the unmatched count is zero too; the
// whole-section test is templates.ImagesNavView.Empty, not anything here (#2608).
//
// An earlier revision pinned the section open for admins so the allowlist behind
// the Unmatched row stayed reachable at a zero count. That guarantee was
// deliberately withdrawn: the maintainer chose full hide-when-clean, accepting
// that at a zero unmatched count the allowlist is reachable only by direct URL.
package dupimages

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// ErrPartialScan marks a scan that COMPLETED WITHOUT A TRANSPORT ERROR but
// could not see the whole library or every platform -- the underlying reports
// carry a ScanErrors counter for exactly this (rule.FanartDupReport.ScanErrors,
// publish.PlatformBackdropDupReport.ScanErrors), and both return err == nil
// while it is nonzero.
//
// A source MUST wrap this when its report has ScanErrors > 0. Refresh treats
// any non-nil error from a half as "this half produced no authoritative value",
// so the previous known value is carried forward rather than being overwritten
// by a number that is confidently wrong.
//
// Why that matters (#2608): the dominant failure mode of both scans is
// per-artist failures swallowed into ScanErrors, not a returned error. A
// half-unreachable library yields an UNDERCOUNT and a fully-unreachable
// platform yields ZERO -- and a zero renders as "clean", silently erasing rows
// for duplicates that are still there. Only a scan that saw everything
// (ScanErrors == 0) is allowed to clear rows.
var ErrPartialScan = errPartialScan{}

type errPartialScan struct{}

func (errPartialScan) Error() string {
	return "duplicate-image scan was partial: some artists or connections could not be scanned"
}

// LibraryCountFn returns the number of redundant within-artist fanart images
// across the whole library. Supplied by the caller so this package does not
// depend on internal/rule.
//
// It must return an error wrapping ErrPartialScan when the underlying scan was
// incomplete; see ErrPartialScan.
type LibraryCountFn func(ctx context.Context) (int, error)

// PlatformCountFn returns the redundant mirrored-backdrop count PER PLATFORM
// TYPE, one entry per offending platform. Supplied by the caller so this
// package does not depend on internal/publish or internal/connection.
//
// Implementations return an entry only for a platform that is actually
// connected AND has offenders; the sidebar renders one row per entry, so an
// entry with a zero count would paint a row claiming a clean platform is
// dirty.
//
// It must return an error wrapping ErrPartialScan when the underlying sweep was
// incomplete; see ErrPartialScan.
type PlatformCountFn func(ctx context.Context) ([]PlatformCount, error)

// PlatformCount is one platform type's duplicate-backdrop tally.
//
// Keyed by platform TYPE ("emby", "jellyfin"), not by connection: the sidebar
// row reads "Emby Duplicates", so two Emby connections both carrying
// duplicates collapse into one row with the combined count rather than two
// rows the operator has to mentally add up.
type PlatformCount struct {
	// Type is the connection type key ("emby", "jellyfin"). Stable across
	// locales and user-chosen connection names; used for ordering and tests.
	Type string
	// Label is the platform's display name ("Emby", "Jellyfin"), which the
	// sidebar renders as "<Label> Duplicates".
	Label string
	// Count is the number of redundant backdrops across every connection of
	// this type. Always > 0 for an entry that is present.
	Count int
}

// Counts is the cached snapshot the sidebar renders from.
type Counts struct {
	// Library is the redundant within-artist fanart slot count (local, on-disk).
	Library int
	// Platforms holds one entry per OFFENDING platform type, in a stable
	// order. Empty when no connected platform carries duplicates.
	Platforms []PlatformCount
	// ComputedAt is when the snapshot was produced -- the later of LibraryAt and
	// PlatformsAt. Zero when neither half has ever been established.
	ComputedAt time.Time
	// Computed is false until at least ONE half has been authoritatively
	// established. Callers use this to distinguish "known clean" from "not yet
	// known"; both render no duplicate rows, but only the latter warrants
	// triggering a refresh.
	//
	// DERIVED, never set by hand: it is true iff LibraryAt or PlatformsAt is
	// non-zero. A refresh where BOTH halves failed must leave it false, or the
	// lazy retry in the nav handler is disabled for the whole 12h refresh
	// interval on data that never scanned successfully (#2608).
	Computed bool

	// LibraryAt is when the Library count was last established by a scan that
	// saw the whole library. Zero means never. This is the per-half provenance
	// stamp: it is what lets a minutes-long Refresh detect that a FRESHER
	// opportunistic store landed while it was scanning, and decline to clobber
	// it with its own now-stale number.
	LibraryAt time.Time
	// PlatformsAt is the same provenance stamp for the Platforms half.
	PlatformsAt time.Time
}

// Empty reports whether no duplicate row has anything to show. An un-computed
// snapshot is Empty.
//
// NOTE this governs only the DUPLICATE ROWS -- it knows nothing about the
// Unmatched row, whose count is not part of this snapshot. The sidebar's
// Images section hides entirely only when the unmatched count is zero TOO, so
// this is a necessary but not sufficient condition for hiding it; the
// whole-section test is templates.ImagesNavView.Empty (#2608).
func (c Counts) Empty() bool { return c.Library <= 0 && len(c.Platforms) == 0 }

// PlatformTotal is the summed redundant-backdrop count across every offending
// platform. Reporting/logging only; the sidebar renders per-platform rows.
func (c Counts) PlatformTotal() int {
	total := 0
	for _, p := range c.Platforms {
		total += p.Count
	}
	return total
}

// Cache memoizes Counts. Safe for concurrent use.
type Cache struct {
	mu     sync.RWMutex
	counts Counts

	// srcMu guards the source functions, which are installed after
	// construction (the API router owns the pipeline/publisher handles).
	srcMu    sync.RWMutex
	library  LibraryCountFn
	platform PlatformCountFn

	// inFlight is the single-flight latch shared by BOTH refresh entry points
	// (Refresh and TriggerRefresh). A second refresh of either kind while one
	// is running is dropped, not queued: these scans are minutes long and the
	// result would be identical.
	inFlight sync.Mutex
	running  bool
	// lastAttempt is when the lazy path last STARTED a refresh, guarded by
	// inFlight. Backs the retry cooldown in TriggerRefresh.
	lastAttempt time.Time

	// backgroundCount is how many background refresh goroutines have been
	// registered and have not yet finished. idle is closed when that count
	// drops to zero, and replaced with a fresh open channel when it rises from
	// zero. Both are guarded by inFlight. Drain waits by receiving on idle.
	//
	// WHY NOT A sync.WaitGroup. This was one, and the WaitGroup contract makes
	// it unusable here: "calls with a positive delta that start when the counter
	// is zero must happen before a Wait". Drain cannot honor that. It must
	// register its intent to wait and then release inFlight (the finishing
	// goroutine needs that lock), which leaves an Add from a concurrent
	// TriggerRefresh racing Wait's first read of the counter -- a genuine data
	// race the detector reports, not a theoretical one.
	//
	// A counter plus a channel has no such rule. Every mutation happens under
	// inFlight, and Drain's only shared read is grabbing the channel VALUE under
	// that same lock; the blocking receive afterwards touches no cache state at
	// all. It also removes the waiter goroutine Drain used to spawn, so a
	// timed-out Drain no longer leaves anything parked.
	backgroundCount int
	idle            chan struct{}

	// lifeMu guards the two shutdown fields below and NOTHING else.
	//
	// LOCK ORDER: c.inFlight may be held while taking lifeMu -- that is how
	// beginBackgroundRefresh and cancelAndSnapshotIdle (Drain's helper) make
	// "latch state" and "shutdown context" agree with each other, and those two
	// functions are the ONLY places the locks nest. (Reset takes lifeMu, mu,
	// srcMu and inFlight too, but strictly one after another, never nested.)
	// The reverse order is FORBIDDEN: nothing anywhere holds lifeMu and then
	// reaches for inFlight, mu or srcMu. One direction only means no cycle,
	// hence no deadlock.
	//
	// No lock is held across a WAIT either. That matters: the goroutine being
	// waited on must take c.inFlight (endBackgroundRefresh), c.mu (update) and
	// c.srcMu (sources) before it can finish, so holding any of them while
	// blocking on idle would deadlock the drain against the very work it is
	// waiting for.
	lifeMu sync.Mutex
	// shutdownCtx is the parent of every background refresh context. Canceling
	// it is how Drain tells an in-flight scan to stop rather than run on past
	// the lifetime of whoever owns this cache (in production: past process
	// shutdown, where it could touch a closing database; in tests: past the end
	// of the test that started it, where it races the next test's state).
	shutdownCtx context.Context
	// shutdownCancel cancels shutdownCtx. Reset installs a FRESH pair so the
	// process-wide cache stays usable by the next test after a drain.
	shutdownCancel context.CancelFunc

	// drainTimeout bounds how long Reset waits for an in-flight background
	// refresh. Per-cache rather than package-level so a test can shorten its own
	// cache's value without reaching into state every other test shares; see
	// defaultDrainTimeout. Set once in New and not written afterwards, so it
	// needs no lock.
	drainTimeout time.Duration

	// beforeRegisterHook runs inside beginBackgroundRefresh's critical section,
	// after the latch is taken and before the refresh becomes drainable. nil in
	// production; set only by tests in this package, and only before any
	// concurrent use of the cache begins. See its call site for why the seam is
	// necessary rather than convenient.
	beforeRegisterHook func()

	logger *slog.Logger
}

// New returns an empty cache. logger must not be nil.
func New(logger *slog.Logger) *Cache {
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Cache{logger: logger, shutdownCtx: ctx, shutdownCancel: cancel, drainTimeout: defaultDrainTimeout}
}

var (
	sharedOnce sync.Once
	shared     *Cache
)

// Shared returns the process-wide cache. It exists so the API router (which
// owns the scan sources) and the maintenance scheduler (which owns the refresh
// cadence) can meet without threading a new dependency through
// cmd/stillwater/main.go.
func Shared() *Cache {
	sharedOnce.Do(func() { shared = New(slog.Default()) })
	return shared
}

// SetSources installs the scan functions. Calling it again replaces them,
// which is what test setup and router re-construction want.
func (c *Cache) SetSources(library LibraryCountFn, platform PlatformCountFn) {
	c.srcMu.Lock()
	c.library, c.platform = library, platform
	c.srcMu.Unlock()
}

// SetLogger replaces the logger. Used when the shared cache is adopted by a
// component that has a properly configured logger.
func (c *Cache) SetLogger(logger *slog.Logger) {
	if logger == nil {
		return
	}
	c.srcMu.Lock()
	c.logger = logger
	c.srcMu.Unlock()
}

func (c *Cache) sources() (LibraryCountFn, PlatformCountFn, *slog.Logger) {
	c.srcMu.RLock()
	defer c.srcMu.RUnlock()
	return c.library, c.platform, c.logger
}

// Get returns the current snapshot. O(1): a mutex-guarded struct copy. It
// performs NO scan, no DB query and no network I/O, so it is safe on the
// sidebar's render/poll path. This is the property the whole package exists
// to guarantee.
func (c *Cache) Get() Counts {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := c.counts
	// Copy the platform slice out. The struct copy above shares its backing
	// array with the cached snapshot, so a caller that sorted or otherwise
	// mutated the returned slice would be writing to cached state outside the
	// lock -- a data race against every other reader. No current consumer does
	// that, but this package advertises concurrency safety, so the hazard is
	// closed here rather than left to every future caller to remember.
	out.Platforms = append([]PlatformCount(nil), c.counts.Platforms...)
	return out
}

// Set overwrites BOTH halves of the snapshot, stamping each as established
// now. Used by tests and by callers that genuinely own the whole snapshot.
//
// Prefer StoreLibrary / StorePlatforms for a caller that computed only one
// half: Set claims provenance over the half it was not given, which would let a
// stale value masquerade as fresh.
func (c *Cache) Set(counts Counts) {
	now := time.Now()
	if counts.LibraryAt.IsZero() {
		counts.LibraryAt = now
	}
	if counts.PlatformsAt.IsZero() {
		counts.PlatformsAt = now
	}
	c.mu.Lock()
	c.counts = normalize(counts)
	c.mu.Unlock()
}

// StoreLibrary records an authoritative library count, leaving the platform
// half untouched. Called by the local backdrop-duplicates report page, which
// pays for the full scan anyway (#2608).
//
// Read-modify-write under the cache lock. The obvious Get-mutate-Set spelling
// is a LOST UPDATE: a concurrent StorePlatforms would be read before its write
// and overwritten by this one's stale copy of the platform half.
func (c *Cache) StoreLibrary(count int) {
	c.update(func(cur Counts) Counts {
		cur.Library = count
		cur.LibraryAt = time.Now()
		return cur
	})
}

// StorePlatforms records authoritative per-platform counts, leaving the library
// half untouched. Same locking rationale as StoreLibrary.
func (c *Cache) StorePlatforms(platforms []PlatformCount) {
	c.update(func(cur Counts) Counts {
		cur.Platforms = platforms
		cur.PlatformsAt = time.Now()
		return cur
	})
}

// update applies fn to the live snapshot with c.mu held for the whole
// read-modify-write, so two concurrent one-half stores cannot lose each other's
// work. fn must not block or call back into the cache.
func (c *Cache) update(fn func(Counts) Counts) {
	c.mu.Lock()
	c.counts = normalize(fn(c.counts))
	c.mu.Unlock()
}

// normalize enforces the snapshot's invariants: Computed and ComputedAt are
// DERIVED from the per-half provenance stamps, zero-count platform entries are
// dropped, and the platform slice is copied away from the caller's array.
func normalize(counts Counts) Counts {
	// Drop any zero-count platform entry: an entry's presence is what paints a
	// row, so a zero would claim a clean platform is dirty.
	//
	// Defensive copy -- the caller keeps its slice, and a later append on their
	// side must not mutate the cached snapshot other goroutines are reading.
	counts.Platforms = append([]PlatformCount(nil), nonZeroPlatforms(counts.Platforms)...)

	counts.Computed = !counts.LibraryAt.IsZero() || !counts.PlatformsAt.IsZero()
	counts.ComputedAt = counts.LibraryAt
	if counts.PlatformsAt.After(counts.ComputedAt) {
		counts.ComputedAt = counts.PlatformsAt
	}
	return counts
}

// nonZeroPlatforms filters out entries that would render an empty row.
func nonZeroPlatforms(in []PlatformCount) []PlatformCount {
	out := in[:0:0]
	for _, p := range in {
		if p.Count > 0 {
			out = append(out, p)
		}
	}
	return out
}

// defaultDrainTimeout bounds how long Reset waits for an in-flight background
// refresh. It is far longer than any test's stub scan needs, so on the healthy
// path it never fires; it exists so a test whose stub source parks forever
// fails with a named, explanatory panic instead of wedging the whole package
// until the go test timeout.
//
// It is the DEFAULT for the per-cache c.drainTimeout field, which is what Reset
// actually reads. A test that needs the expiry branch shortens its OWN cache's
// field. An earlier revision made this package-level value a mutable var for
// that purpose, which was a hazard held together only by convention: 31 tests
// in cache_test.go call t.Parallel(), and Go PAUSES rather than finishes
// previously-started parallel tests while a non-parallel one runs, so a
// "briefly shortened" global is not provably exclusive and gets less so as the
// package grows. A field has no such coupling.
const defaultDrainTimeout = 30 * time.Second

// Reset drops the snapshot and the installed sources, returning the cache to
// its just-constructed state. Exposed for tests, which share the process-wide
// cache and must not bleed state into one another; production code never
// calls it.
//
// It DRAINS FIRST (#2977). A TriggerRefresh started by a request under test
// runs in the background and, without this, was still running when the test
// returned -- calling back into the previous test's router closures while the
// next test wrote that same state. That is a real data race, and because Go
// attributes it to whatever tests happen to be in flight, it failed a dozen
// unrelated tests at once. Draining here means "the test that started the
// refresh is also the test that finishes it".
//
// After draining, a FRESH shutdown context is installed, because the drain
// permanently canceled the old one. Without this the process-wide cache would
// be left in a state where every subsequent TriggerRefresh aborted instantly,
// so the next test's lazy path would silently do nothing.
//
// It BLOCKS for as long as the in-flight refresh takes, up to c.drainTimeout
// (defaultDrainTimeout unless a test shortened it), and PANICS if that timeout
// expires -- see the drain branch below for why proceeding would be worse than
// failing.
func (c *Cache) Reset() {
	// No cache lock is held here: the goroutine being waited on needs c.mu,
	// c.srcMu and c.inFlight to finish. See Drain's lock-order note.
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), c.drainTimeout)
	defer cancelDrain()
	if err := c.Drain(drainCtx); err != nil {
		_, _, logger := c.sources() // logger is guarded by srcMu; do not read the field directly
		logger.Error("duplicate-image cache reset timed out waiting for a background refresh; a scan goroutine is still running",
			slog.Any("error", err))
		// PANIC RATHER THAN PROCEED. Everything below -- fresh context, zeroed
		// counts, nil sources -- assumes the drain succeeded. A goroutine that
		// outlived it still holds the source funcs it captured at refresh()
		// entry and will write into the NEXT test's cache, which is exactly
		// #2977. Continuing turns that into a silent green test whose only
		// trace is one stderr line, so the failure has to be unmissable.
		//
		// Safe to panic here: Reset is a test-only entry point (production code
		// never calls it), and inside a test a panic is a named, stack-carrying
		// failure rather than a swallowed log.
		panic("dupimages: Reset drain timed out; a background refresh goroutine is still running and will corrupt the next test (#2977): " + err.Error())
	}

	// Install a fresh shutdown context so the cache is live again.
	ctx, cancel := context.WithCancel(context.Background())
	c.lifeMu.Lock()
	c.shutdownCtx, c.shutdownCancel = ctx, cancel
	c.lifeMu.Unlock()

	c.mu.Lock()
	c.counts = Counts{}
	c.mu.Unlock()
	c.srcMu.Lock()
	c.library, c.platform = nil, nil
	c.srcMu.Unlock()
	// Clear the lazy-path cooldown too, or a test that reset the process-wide
	// cache would silently inherit the previous test's cooldown and observe no
	// trigger at all.
	//
	// DO NOT CLEAR c.running HERE. An earlier revision did, as "defense in
	// depth" against a latch stranded by the old register-window escape (the
	// gap that once existed between taking the latch and becoming drainable,
	// closed in beginBackgroundRefresh). It was a
	// silent correctness bug: Reset cleared the latch UNCONDITIONALLY, so a
	// refresh that took it LEGITIMATELY after the drain completed (the drain
	// only waits for refreshes registered before it started) was erased from
	// single-flight, and the next TriggerRefresh launched a SECOND CONCURRENT
	// FULL SCAN -- doubling the I/O and CPU of the most expensive task in the
	// process, which is the exact failure beginRefresh's latch exists to
	// prevent. The clear looked harmless because deleting it broke no test;
	// "nothing depends on it" is not "it is safe", and the question that
	// mattered was what it does when the invariant it assumes does not hold.
	//
	// NOR IS AN ASSERTION SOUND HERE, which is the second thing that revision
	// got wrong. Drain covers BACKGROUND refreshes only -- a direct, blocking
	// Refresh takes the same latch (beginRefresh) but never registers itself,
	// because nothing needs to await it. So a held latch at this point is
	// legitimately reachable, with no bug anywhere, whenever a Refresh is
	// running concurrently with a Reset. A panic here would be a false alarm,
	// and a clear here would break that Refresh's single-flight.
	//
	// So Reset simply does not touch c.running. The failure the old clear was
	// aimed at -- a latch stranded by a refresh that escaped the drain -- is
	// closed at its source by beginBackgroundRefresh's atomicity, which is
	// where a correctness property belongs.
	c.inFlight.Lock()
	c.lastAttempt = time.Time{}
	c.inFlight.Unlock()
}

// Refresh runs both scans and stores the result. BLOCKING and expensive --
// only the background maintenance task should call it.
//
// Three rules govern what actually gets written, each guarding a way the
// sidebar could otherwise report a lie (#2608):
//
//  1. A half that FAILED is not written, so its last known value carries
//     forward. A broken platform connection cannot blank out a real count, and
//     a zero would render as "clean". A PARTIAL scan counts as failed -- see
//     ErrPartialScan, which is the failure mode both scans actually exhibit.
//  2. A half that succeeded is written only if no FRESHER value landed while
//     the scan was running. The scans take minutes, so an operator's
//     remediation plus a report-page visit mid-scan is ordinary; without this
//     the finishing refresh would clobber their correct number with its stale
//     one.
//  3. If NEITHER half was established, nothing is written at all, leaving
//     Computed false. Computed gates the nav handler's lazy retry, so latching
//     it on a wholly failed refresh would freeze a never-scanned cache as
//     authoritative-clean until the next periodic tick.
//
// The first error encountered is returned regardless of what was written.
//
// SINGLE-FLIGHT: Refresh shares one latch with TriggerRefresh (see
// beginRefresh). If a refresh of either kind is already running, this call
// DROPS and returns ErrRefreshInFlight without scanning.
func (c *Cache) Refresh(ctx context.Context) error {
	if !c.beginRefresh() {
		return ErrRefreshInFlight
	}
	defer c.endRefresh()
	return c.refresh(ctx)
}

// beginRefresh takes the single-flight latch for a DIRECT, blocking Refresh,
// reporting whether the caller owns it. The caller MUST call endRefresh when it
// owns the latch.
//
// The lazy path takes the same latch through beginBackgroundRefresh, which
// additionally registers the refresh as drainable (c.backgroundCount and the
// c.idle channel) and pairs with endBackgroundRefresh rather than endRefresh.
// A direct Refresh needs no such registration: its caller is already blocked on
// it, so there is nothing for Drain to await.
//
// c.running is the shared latch, which is the point: it used
// to be set only by TriggerRefresh, so the maintenance scheduler's direct
// Refresh never participated. During a cold scheduled scan -- minutes of disk
// re-hashing plus a platform sweep -- a sidebar poll saw running == false and
// launched a SECOND concurrent full sweep, doubling the I/O and CPU cost of the
// most expensive task in the process on a design that advertises single-flight.
//
// WHY A DIRECT Refresh DROPS RATHER THAN WAITS. Both callers want the same
// thing: a reasonably fresh snapshot, cheaply. Neither consumes the return
// value as an answer to a question -- the sidebar reads Get(), never a Refresh
// result. So a refresh that is ALREADY RUNNING will satisfy the dropped
// caller's need within the same window, and waiting for it would buy nothing
// while pinning a scheduler goroutine (and, for the 12h tick, delaying the
// loop's return to its select for the whole remaining duration of the in-flight
// scan). Dropping is also the safer failure mode: a wait could stack callers
// behind a stalled sweep, which is exactly the pathology the per-run deadline
// in maintenance exists to prevent. The dropped tick is not data loss -- the
// next tick is 12h out and the in-flight scan writes the same numbers this one
// would have.
//
// Lazy callers additionally honor retryCooldown; a direct Refresh does not,
// because its cadence is already governed by the scheduler's interval.
// A direct Refresh likewise does not stamp lastAttempt: that field means "when
// the LAZY path last started", and letting a scheduled run arm the lazy
// cooldown would suppress a cold-cache warm-up the operator is waiting on.
func (c *Cache) beginRefresh() bool {
	c.inFlight.Lock()
	defer c.inFlight.Unlock()
	if c.running {
		return false
	}
	c.running = true
	return true
}

// beginBackgroundRefresh is beginRefresh for the LAZY path, plus the two extra
// steps that only a background (fire-and-forget) refresh needs: registering
// itself as drainable, and reading the shutdown context the scan will derive
// from. It returns that context and whether the caller owns the latch. When it
// reports false the caller has taken nothing and must not spawn anything.
//
// WHY ALL THREE STEPS SHARE ONE CRITICAL SECTION. Taking the latch, becoming
// visible to Drain, and choosing a parent context must be INDIVISIBLE. When
// they were three separate steps -- latch, then register, then read the context
// -- there was a window in which a refresh had already claimed the latch but
// was still uncounted. A Drain (or the Drain inside Reset) landing entirely
// inside that window saw a count of zero, so it returned at once and reported
// "everything has finished" about a refresh that had not started. Reset then
// installed a FRESH, LIVE shutdown context -- and the escaping goroutine, still
// to read it, picked THAT one up and ran the NEXT owner's freshly installed
// sources on a context nobody would ever cancel. That is #2977 again, just
// through a narrower window.
//
// Holding c.inFlight while taking c.lifeMu is what closes it: Drain takes
// c.inFlight around its cancel AND around grabbing the idle channel, so a drain
// can no longer interleave between these steps. Either it runs entirely before
// this call (and this call derives from the context Drain just canceled, so the
// scan aborts immediately), or entirely after (and the count is already 1, so
// Drain blocks). Neither lock is held across a wait, and nothing takes them in
// the opposite order, so this cannot deadlock -- see lifeMu's lock-order note.
func (c *Cache) beginBackgroundRefresh() (context.Context, bool) {
	c.inFlight.Lock()
	defer c.inFlight.Unlock()

	if c.running {
		return nil, false
	}
	if !c.lastAttempt.IsZero() && time.Since(c.lastAttempt) < retryCooldown {
		return nil, false
	}
	c.running = true
	c.lastAttempt = time.Now()

	// TEST SEAM, nil in production and never set outside this package's tests.
	//
	// It exists because the property this function guarantees -- that taking the
	// latch and becoming drainable is INDIVISIBLE -- is otherwise untestable.
	// The window is a few instructions wide, and a test that merely races a
	// TriggerRefresh against a Reset cannot tell an escapee (a refresh Drain
	// owed a wait) from an ordinary refresh that legitimately started after
	// Reset finished: both end up running on the freshly installed context.
	// Asserting without that distinction produces a test that fails against
	// correct code, which is precisely the flake an earlier revision shipped.
	//
	// Widening the window under a test's control removes the ambiguity: the
	// test holds a refresh at this exact point, runs a whole Reset, and can then
	// state without qualification that the refresh was registered first. A seam
	// is the honest instrument here, not a heuristic loop.
	if hook := c.beforeRegisterHook; hook != nil {
		hook()
	}

	// Visible to Drain BEFORE the latch is released, so there is no instant at
	// which this refresh exists but a drain cannot see it.
	if c.backgroundCount == 0 {
		// Rising from zero: whoever drains from here on must wait for THIS
		// refresh, so install a fresh open channel for them to block on.
		c.idle = make(chan struct{})
	}
	c.backgroundCount++

	c.lifeMu.Lock()
	parent := c.shutdownCtx
	c.lifeMu.Unlock()
	return parent, true
}

// endBackgroundRefresh releases the single-flight latch AND deregisters the
// refresh, in one critical section. Both must happen together for the same
// reason they are taken together: a Drain that observed the count hit zero must
// never then find the latch still held.
func (c *Cache) endBackgroundRefresh() {
	c.inFlight.Lock()
	defer c.inFlight.Unlock()
	c.running = false
	c.backgroundCount--
	if c.backgroundCount == 0 && c.idle != nil {
		// Quiescent: release every waiting Drain. Closing rather than sending
		// is what lets any number of concurrent drains observe it.
		close(c.idle)
		c.idle = nil
	}
}

// endRefresh releases the single-flight latch for a DIRECT Refresh, which was
// never registered as drainable and so has nothing to deregister.
func (c *Cache) endRefresh() {
	c.inFlight.Lock()
	c.running = false
	c.inFlight.Unlock()
}

// refresh is the unguarded scan-and-store body shared by both entry points. The
// caller must already hold the single-flight latch.
func (c *Cache) refresh(ctx context.Context) error {
	library, platform, logger := c.sources()
	if library == nil && platform == nil {
		// Fail loud rather than silently caching zeros: an unwired cache
		// would render "everything is clean" forever.
		logger.Error("duplicate-image count refresh skipped: no scan sources installed")
		return errNoSources
	}

	// Stamped BEFORE the scans start. Both are minutes long, so an operator's
	// remediation plus a report-page visit can easily land mid-scan; comparing
	// against this is how the write below detects that it is about to overwrite
	// a value FRESHER than its own.
	startedAt := time.Now()

	var (
		firstErr    error
		libN        int
		libOK       bool
		platforms   []PlatformCount
		platformsOK bool
	)

	if library != nil {
		n, err := library(ctx)
		if err != nil {
			logger.Error("duplicate-image library count scan failed", slog.Any("error", err))
			firstErr = err
		} else {
			libN, libOK = n, true
		}
	}

	if platform != nil {
		p, err := platform(ctx)
		if err != nil {
			logger.Error("duplicate-image platform count scan failed", slog.Any("error", err))
			if firstErr == nil {
				firstErr = err
			}
		} else {
			platforms, platformsOK = p, true
		}
	}

	if !libOK && !platformsOK {
		// NOTHING was established. Write nothing: a failed refresh must not
		// stamp provenance, because Computed is derived from those stamps and
		// the nav handler's lazy retry is gated on !Computed. Latching Computed
		// here would freeze a never-successfully-scanned cache as
		// authoritative-clean until the next periodic tick (#2608).
		logger.Error("duplicate-image count refresh established neither half; snapshot left unchanged",
			slog.Any("error", firstErr))
		return firstErr
	}

	// Apply both halves in ONE locked read-modify-write against the CURRENT
	// snapshot, not against a copy taken before the scans ran.
	//
	// A half that FAILED is simply not applied, so its last known value carries
	// forward rather than being silently zeroed (a zero renders as "clean",
	// which would be a lie after a transient outage). A half that SUCCEEDED is
	// applied only if no fresher value landed while this scan was running.
	var skippedStale []string
	c.update(func(cur Counts) Counts {
		now := time.Now()
		if libOK {
			if cur.LibraryAt.After(startedAt) {
				skippedStale = append(skippedStale, "library")
			} else {
				cur.Library, cur.LibraryAt = libN, now
			}
		}
		if platformsOK {
			if cur.PlatformsAt.After(startedAt) {
				skippedStale = append(skippedStale, "platforms")
			} else {
				// Assign unconditionally, including an empty result: an empty
				// slice is the legitimate "every connected platform is clean"
				// answer and must clear stale rows. This is safe ONLY because a
				// partial sweep arrives as an error (see ErrPartialScan) and so
				// never reaches this branch.
				cur.Platforms, cur.PlatformsAt = platforms, now
			}
		}
		return cur
	})

	if len(skippedStale) > 0 {
		logger.Info("duplicate-image count refresh declined to overwrite a fresher value",
			slog.Any("halves", skippedStale))
	}

	got := c.Get() // read back the normalized snapshot for the log line
	logger.Info("duplicate-image counts refreshed",
		slog.Int("library", got.Library),
		slog.Int("platform_total", got.PlatformTotal()),
		slog.Int("platforms_affected", len(got.Platforms)),
		slog.Bool("library_established", libOK),
		slog.Bool("platforms_established", platformsOK))
	return firstErr
}

// TriggerRefresh starts a Refresh in the background and returns immediately.
// It is the lazy path: a cold cache asks for numbers without making anyone
// wait for them. Single-flight -- a trigger while a refresh is already running
// is dropped.
//
// The background scan gets its own context (detached from any request), with a
// generous timeout, because a full platform sweep outlives the HTTP request
// that noticed the cache was cold.
// A refresh that establishes NEITHER half correctly leaves Computed false (see
// Refresh), which keeps this lazy path armed -- otherwise a boot-order failure
// would freeze the sidebar as authoritative-clean for the whole 12h interval.
// The cooldown is what keeps "still armed" from becoming "scanning constantly":
// without it, a persistently failing source would be re-triggered by the very
// next 60s sidebar poll after each multi-minute scan gave up, pinning the
// process at a ~100% duty cycle on the most expensive task it runs.
//
// The cooldown bounds only the LAZY path. The periodic maintenance task calls
// Refresh directly and is not subject to it, so a genuine outage is still
// retried on schedule. Both paths DO share the single-flight latch -- see
// beginRefresh.
// The signature stays argument-less ON PURPOSE. Threading the caller's REQUEST
// context in would be wrong: that context is canceled the moment the response
// is written, so the scan this call exists to start would be killed seconds
// into a job measured in minutes. The lifecycle that DOES apply is the cache's
// own (see Drain), which is why the background context is derived from
// c.shutdownCtx rather than from context.Background().
func (c *Cache) TriggerRefresh() {
	// One indivisible step: take the latch, become countable by Drain, and read
	// the shutdown context to derive from. Splitting those apart is what let a
	// refresh escape a drain -- see beginBackgroundRefresh.
	parent, ok := c.beginBackgroundRefresh()
	if !ok {
		return
	}

	go func() {
		// Releases the latch and deregisters from the drain in ONE critical
		// section, so a Drain that saw the count reach zero can never then find
		// the latch still held.
		defer c.endBackgroundRefresh()
		ctx, cancel := RefreshContext(parent)
		defer cancel()
		// Error already logged inside refresh; nothing actionable here.
		//
		// A TriggerRefresh issued AFTER a Drain is not an error and does not
		// spin: parent is already canceled, so the scan sources see a dead
		// context, return promptly, and the latch is released as usual.
		_ = c.refresh(ctx)
	}()
}

// Drain cancels any in-flight background refresh and waits for it to finish,
// mirroring api.Router.DrainWebhooks. It returns nil once every goroutine
// TriggerRefresh spawned has returned, or ctx.Err() if the supplied context
// expires first.
//
// WHY THIS EXISTS. TriggerRefresh is fire-and-forget, so without Drain nothing
// could observe or stop the resulting scan. In production that means a refresh
// could still be reading the database after shutdown closed it. In tests it
// means a scan started by one test's HTTP request runs on into the NEXT test
// and reads state that test is writing, which is the data race in #2977.
//
// WHY THE CANCEL AND THE CHANNEL SNAPSHOT ARE TAKEN UNDER c.inFlight.
// beginBackgroundRefresh holds c.inFlight while it takes the latch, registers
// itself, and reads the shutdown context. Taking the same lock here means a
// drain can never interleave with those steps: it either happens entirely
// BEFORE one (so the refresh derives from the context this cancel just killed,
// and its scan aborts immediately) or entirely AFTER (so the count is already 1
// and the receive below actually blocks). Without this, a drain landing between
// the latch and the registration saw a zero count, returned "quiescent", and
// Reset then handed the escaping goroutine a fresh LIVE context -- #2977
// through a narrower window.
//
// LOCK ORDER. Every lock is released before the receive. That matters: the
// goroutine being waited on must take c.inFlight (endBackgroundRefresh), c.mu
// (update) and c.srcMu (sources) before it can finish, so holding any of them
// across the wait would deadlock the drain against the very work it is waiting
// for. The inFlight-then-lifeMu order matches beginBackgroundRefresh; nothing
// anywhere takes them the other way round, so there is no cycle.
//
// Calling Drain twice is safe: the second cancel is a no-op and the count is
// already zero, so it returns nil immediately. After a Drain the cache
// still serves Get and still accepts a TriggerRefresh -- that refresh just
// starts on an already-canceled context and gives up at once. Reset is what
// makes the cache fully live again.
func (c *Cache) Drain(ctx context.Context) error {
	idle := c.cancelAndSnapshotIdle()
	if idle == nil {
		// Nothing was registered, so the cache is already quiescent.
		return nil
	}

	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// cancelAndSnapshotIdle cancels the shutdown context and returns the channel
// that will be closed when the last in-flight background refresh finishes, or
// nil if there are none. Split out from Drain so that every unlock can be a
// DEFER: the previous inline spelling unlocked positionally, so a panicking
// cancel() (a nil shutdownCancel on a Cache built without New) left c.inFlight
// held FOREVER and every later Drain, Refresh, TriggerRefresh and
// endBackgroundRefresh blocked on it -- a one-goroutine failure escalated into
// a package-wide deadlock. A panic must not be able to strand a lock.
//
// Both the cancel and the channel snapshot happen under c.inFlight, which is
// what makes the drain airtight against a concurrent TriggerRefresh: see
// beginBackgroundRefresh. Returning the channel rather than waiting here is
// deliberate -- the caller blocks with NO lock held, because the goroutine it
// waits for needs c.inFlight, c.mu and c.srcMu to finish.
func (c *Cache) cancelAndSnapshotIdle() chan struct{} {
	c.inFlight.Lock()
	defer c.inFlight.Unlock()

	func() {
		c.lifeMu.Lock()
		defer c.lifeMu.Unlock()
		// Called with lifeMu held, which is safe because it cancels a context
		// and touches nothing on this cache. Holding it means a Reset cannot
		// swap in a fresh context between the read and the call, which would
		// cancel the OLD context after the new one was installed.
		c.shutdownCancel()
	}()

	return c.idle
}

// RefreshContext derives a context bounded by the standard per-refresh
// deadline. Every caller of Refresh must use it (the lazy path above does so
// internally) so a stalled scan cannot run forever.
//
// It is exported for the maintenance scheduler, whose own ctx is
// process-lifetime: without a per-run bound, one stalled sweep blocks the
// ticker loop indefinitely and EVERY subsequent refresh is lost, not just the
// stalled one. Sharing this single helper is what keeps the two paths'
// deadlines from drifting apart again.
//
// The returned context is derived from parent, so canceling the scheduler's
// context still aborts an in-flight run promptly. The caller must call cancel.
func RefreshContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, refreshTimeout)
}

// refreshTimeout bounds a background refresh. Generous: a full from-disk
// re-hash plus a platform sweep over a large library is measured in minutes.
const refreshTimeout = 30 * time.Minute

// retryCooldown is the minimum gap between two LAZY (TriggerRefresh) scans.
// See TriggerRefresh for why it exists. Well under the 12h periodic cadence, so
// a cold cache still warms promptly once the source recovers.
const retryCooldown = 15 * time.Minute

// ErrRefreshInFlight is returned by Refresh when another refresh (scheduled or
// lazy) already holds the single-flight latch, so this call scanned nothing.
//
// It is NOT a failure: the in-flight scan produces the same numbers this call
// would have. Callers should log it at info level, not as an error.
var ErrRefreshInFlight = errRefreshInFlight{}

type errRefreshInFlight struct{}

func (errRefreshInFlight) Error() string {
	return "duplicate-image count refresh skipped: another refresh is already running"
}

// errNoSources is returned by Refresh when the cache has no scan functions.
var errNoSources = errNoSourcesType{}

type errNoSourcesType struct{}

func (errNoSourcesType) Error() string {
	return "duplicate-image count cache: no scan sources installed"
}
