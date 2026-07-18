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
// Until the first successful Refresh completes, Get returns a zero Counts with
// Computed=false, which renders as "no duplicate rows". That is the intended
// hide-when-unknown behavior from the issue: a not-yet-computed count is
// indistinguishable from clean, and the steady state is clean anyway.
//
// SCOPE: these counts gate the three DUPLICATE ROWS only (Library Duplicates,
// <Platform> Duplicates). The Images section itself is always rendered for
// admins because it also carries the Unmatched item, whose allowlist has to
// stay reachable at a zero count. Nothing here may be used to hide the
// section.
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
// NOTE this governs only the DUPLICATE ROWS. The sidebar's Images section
// itself is always rendered for admins, because it also carries the Unmatched
// item, whose allowlist must stay reachable at a zero count (#2608). Never
// use Empty to decide whether to render the section.
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

	logger *slog.Logger
}

// New returns an empty cache. logger must not be nil.
func New(logger *slog.Logger) *Cache {
	if logger == nil {
		logger = slog.Default()
	}
	return &Cache{logger: logger}
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

// Reset drops the snapshot and the installed sources, returning the cache to
// its just-constructed state. Exposed for tests, which share the process-wide
// cache and must not bleed state into one another; production code never
// calls it.
func (c *Cache) Reset() {
	c.mu.Lock()
	c.counts = Counts{}
	c.mu.Unlock()
	c.srcMu.Lock()
	c.library, c.platform = nil, nil
	c.srcMu.Unlock()
	// Clear the lazy-path cooldown too, or a test that reset the process-wide
	// cache would silently inherit the previous test's cooldown and observe no
	// trigger at all.
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
	if !c.beginRefresh(false) {
		return ErrRefreshInFlight
	}
	defer c.endRefresh()
	return c.refresh(ctx)
}

// beginRefresh takes the single-flight latch, reporting whether the caller owns
// it. The caller MUST call endRefresh when it owns the latch.
//
// This is the one place the latch is taken, which is the point: c.running used
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
func (c *Cache) beginRefresh(lazy bool) bool {
	c.inFlight.Lock()
	defer c.inFlight.Unlock()
	if c.running {
		return false
	}
	if lazy && !c.lastAttempt.IsZero() && time.Since(c.lastAttempt) < retryCooldown {
		return false
	}
	c.running = true
	if lazy {
		c.lastAttempt = time.Now()
	}
	return true
}

// endRefresh releases the single-flight latch.
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
func (c *Cache) TriggerRefresh() {
	if !c.beginRefresh(true) {
		return
	}

	go func() {
		defer c.endRefresh()
		ctx, cancel := RefreshContext(context.Background())
		defer cancel()
		// Error already logged inside refresh; nothing actionable here.
		_ = c.refresh(ctx)
	}()
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
