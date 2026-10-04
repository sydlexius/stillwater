package rule

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/sydlexius/stillwater/internal/artist"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/publish"
)

// platformBackdropPruner is the slice of *publish.Publisher the duplicate
// fixer's platform phase needs (#3138). An interface so tests can supply a
// fake platform without a live media server.
type platformBackdropPruner interface {
	PrunePlatformBackdropsForArtist(ctx context.Context, a *artist.Artist, opts publish.ArtistBackdropPruneOptions) (publish.PlatformBackdropPruneResult, error)
}

// localOutcome is how far ImageDuplicateFixer.fixLocal got.
type localOutcome int

const (
	localNotRun  localOutcome = iota // returned before detection completed
	localClean                       // detection found nothing to remove
	localBlocked                     // duplicates remain, every one protected
	localRemoved                     // local duplicate files were deleted
)

var _ platformBackdropPruner = (*publish.Publisher)(nil)

// platformPruneWiring is installed in one step so the pruner can never be
// visible without the cache-invalidation hook that must follow its deletes.
type platformPruneWiring struct {
	pruner  platformBackdropPruner
	onPrune func()
	dups    *publish.PlatformDupCache // the sweep's cache (S3b); nil when none is wired
}

// platformPruneToleranceFloor is the lowest similarity the PLATFORM phase
// accepts. The rule PUT stores Config unvalidated, and a platform delete is
// unattended and irreversible, so a fat-fingered tolerance (0.5 treats most
// distinct backdrops as "the same picture") is refused, not obeyed. The local
// phase keeps its own handling of the value.
const platformPruneToleranceFloor = 0.85

// SetPlatformPruner wires the platform phase. onPrune runs after any platform
// delete and must invalidate every cache listing platform backdrops (the
// report snapshot and the sidebar counts, #3092). Until both are set the
// platform phase refuses rather than delete without invalidating.
func (f *ImageDuplicateFixer) SetPlatformPruner(p platformBackdropPruner, onPrune func()) {
	w := platformPruneWiring{pruner: p, onPrune: onPrune}
	if old := f.platformPrune.Load(); old != nil {
		w.dups = old.dups
	}
	f.platformPrune.Store(&w)
}

// SetPlatformDupCache wires the sweep's cache; either setter may come first.
func (f *ImageDuplicateFixer) SetPlatformDupCache(c *publish.PlatformDupCache) {
	w := platformPruneWiring{dups: c}
	if old := f.platformPrune.Load(); old != nil {
		w.pruner, w.onPrune = old.pruner, old.onPrune
	}
	f.platformPrune.Store(&w)
}

// platformPruneTolerance returns the tolerance the platform phase runs at:
// zero (unset) is the rule default, anything else must lie in [floor, 1].
func platformPruneTolerance(configured float64) (float64, bool) {
	if configured == 0 {
		return img.DefaultDuplicateTolerance, true
	}
	return configured, configured >= platformPruneToleranceFloor && configured <= 1
}

// PlatformDupSweepPolicy tells the background platform near-duplicate sweep
// (publish.PlatformDupSweep, #3138 S3a) whether to run and at what tolerance.
// It is on only when the fixer's platform phase could act: the rule enabled,
// prune_platform_copies set, and a tolerance platformPruneTolerance accepts.
// Going through that same function is what keeps the sweep's findings and the
// fixer's deletes in agreement. Every disabled answer carries tolerance 0.
func (s *Service) PlatformDupSweepPolicy(ctx context.Context) (tolerance float64, enabled bool, err error) {
	r, err := s.GetByID(ctx, RuleImageDuplicate)
	if errors.Is(err, ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if !r.Enabled || !r.Config.PrunePlatformCopies {
		return 0, false, nil
	}
	if tolerance, enabled = platformPruneTolerance(r.Config.Tolerance); !enabled {
		return 0, false, nil // refused (below the floor or above 1)
	}
	return tolerance, true, nil
}

// runPlatformPhase prunes near-duplicate backdrops on the artist's connected
// platforms, after the local phase. Every outcome, including a refusal or an
// error, is folded into fr.Message: the local delete already happened and
// cannot be undone, so a Go error here would only skip the persist and
// reconcile that the local change still needs.
func (f *ImageDuplicateFixer) runPlatformPhase(ctx context.Context, a *artist.Artist, configuredTolerance float64, local localOutcome, fr *FixResult) {
	note := func(msg string) { fr.Message += "; platform: " + msg }
	// Publish paths never check exclusion; the rule pipeline does. A Fix that
	// reaches here for an excluded artist (a stale row) stays local-only.
	if a.IsExcluded {
		note("skipped, artist is excluded")
		return
	}
	tol, ok := platformPruneTolerance(configuredTolerance)
	if !ok {
		f.logger.Warn("duplicate-images platform phase refused: tolerance outside the allowed range",
			slog.String("artist_id", a.ID), slog.Float64("tolerance", configuredTolerance),
			slog.Float64("floor", platformPruneToleranceFloor))
		note(fmt.Sprintf("refused, similarity tolerance %v is outside %.2f-1.0", configuredTolerance, platformPruneToleranceFloor))
		return
	}
	w := f.platformPrune.Load()
	if w == nil || w.pruner == nil || w.onPrune == nil {
		f.logger.Error("duplicate-images platform phase enabled but no platform pruner is wired",
			slog.String("artist_id", a.ID))
		note("not available (no platform pruner wired)")
		return
	}
	res, err := w.pruner.PrunePlatformBackdropsForArtist(ctx, a, publish.ArtistBackdropPruneOptions{Perceptual: true, Tolerance: tol})
	// Invalidate on any run that may have deleted, error or not (mirrors the
	// prune handler's partial-failure branch).
	if res.BackdropsRemoved > 0 || len(res.Failures) > 0 {
		w.onPrune()
	}
	// complete: every connection was read and every planned delete landed. A
	// failure, policy skip, skipped copy or unhealthy connection leaves duplicates
	// behind, so the fix is NOT Fixed even if the local phase removed files.
	complete := err == nil && len(res.Failures)+len(res.Unhealthy)+len(res.Skipped)+res.SkippedChanged == 0
	if !complete {
		fr.Fixed = false
	}
	localRemains := false
	if res.BackdropsRemoved > 0 {
		fr.Irreversible = true
		// A clean local folder plus a COMPLETE platform prune is a fixed
		// violation. A blocked local phase is not (its duplicates are still on
		// disk), nor is a cross-type pair this fixer never removes.
		localRemains = local == localClean && f.localDuplicateRemains(ctx, a, configuredTolerance)
		if complete && local == localClean && !localRemains {
			fr.Fixed = true
		}
	}
	// After a complete run nothing redundant is left, so the cached finding is
	// stale. The publisher's write observer drops it when the run deleted; this
	// covers the run that found nothing (no platform target left, which the
	// cache never hears about, or already clean), or a finding nothing can fix
	// stands until it ages out. An incomplete run must leave the finding open.
	if w.dups != nil && complete {
		w.dups.Invalidate(a.ID)
	}
	f.logPlatformDeletes(a, tol, res)
	// Raw platform/connection/DB error text stays in the server log; the
	// message below is returned to API clients.
	for _, fl := range res.Failures {
		f.logger.Warn("duplicate-images platform prune connection failed",
			slog.String("artist_id", a.ID), slog.String("connection_id", fl.ConnectionID),
			slog.String("error", fl.Err))
	}
	if err != nil {
		f.logger.Warn("duplicate-images platform prune failed",
			slog.String("artist_id", a.ID), slog.String("error", err.Error()))
	}
	note(summarizePlatformPrune(res, err))
	if localRemains {
		fr.Message += "; not resolved: a local duplicate remains that this fix cannot remove"
	}
}

// localDuplicateRemains reports whether the checker would still raise a LOCAL
// duplicate, from stored hashes as the checker does: fixLocal's fresh pass
// cannot see a cross-type pair. Any doubt reads as "remains" (row stays open).
func (f *ImageDuplicateFixer) localDuplicateRemains(ctx context.Context, a *artist.Artist, configuredTolerance float64) bool {
	tolerance := configuredTolerance // normalized as the checker does
	if tolerance <= 0 || tolerance > 1.0 {
		tolerance = defaultImageDupTolerance
	}
	res, err := findImageDuplicates(ctx, f.db, a, resolveFanartPrimaryName(ctx, f.platformService), tolerance, f.imageHashRecorder, false, f.logger)
	return err != nil || len(res.perceptual) > 0
}

// logPlatformDeletes records each platform deletion with structured attrs so
// an unattended (auto / Fix All) run can be audited after the fact.
func (f *ImageDuplicateFixer) logPlatformDeletes(a *artist.Artist, tol float64, res publish.PlatformBackdropPruneResult) {
	for _, e := range res.Plan {
		if e.Outcome != publish.PrunePlanDeleted {
			continue
		}
		f.logger.Info("duplicate-images rule deleted a platform backdrop",
			slog.String("artist_id", a.ID), slog.String("artist", a.Name),
			slog.String("connection_id", e.ConnectionID), slog.Int("index", e.Index),
			slog.Int("survivor", e.Survivor), slog.String("tier", e.Tier),
			slog.Float64("tolerance", tol))
	}
}

// summarizePlatformPrune renders the per-connection audit line: how many
// backdrops each connection lost and which detection-time slot was kept.
func summarizePlatformPrune(res publish.PlatformBackdropPruneResult, err error) string {
	type tally struct {
		removed   int
		survivors map[int]bool
	}
	byConn := map[string]*tally{}
	for _, e := range res.Plan {
		if e.Outcome != publish.PrunePlanDeleted {
			continue
		}
		t := byConn[e.ConnectionID]
		if t == nil {
			t = &tally{survivors: map[int]bool{}}
			byConn[e.ConnectionID] = t
		}
		t.removed++
		t.survivors[e.Survivor] = true
	}
	conns := make([]string, 0, len(byConn))
	for id := range byConn {
		conns = append(conns, id)
	}
	sort.Strings(conns)
	parts := make([]string, 0, len(conns)+4)
	for _, id := range conns {
		t := byConn[id]
		kept := make([]int, 0, len(t.survivors))
		for s := range t.survivors {
			kept = append(kept, s)
		}
		sort.Ints(kept)
		parts = append(parts, fmt.Sprintf("connection %s removed %d backdrop(s), kept slot(s) %s", id, t.removed, joinInts(kept)))
	}
	if len(parts) == 0 {
		parts = append(parts, "removed 0 backdrop(s)")
	}
	if res.SkippedChanged > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped because they changed since detection", res.SkippedChanged))
	}
	for _, s := range res.Skipped {
		parts = append(parts, "near-duplicate pass skipped: "+s.Reason)
	}
	for _, fl := range res.Failures {
		parts = append(parts, fmt.Sprintf("connection %s failed (see server log)", fl.ConnectionID))
	}
	if err != nil {
		parts = append(parts, "platform prune error (see server log)")
	}
	return strings.Join(parts, "; ")
}

func joinInts(xs []int) string {
	s := make([]string, len(xs))
	for i, x := range xs {
		s[i] = fmt.Sprint(x)
	}
	return strings.Join(s, ",")
}

// platformPruneSlot is the atomic holder embedded in ImageDuplicateFixer.
type platformPruneSlot = atomic.Pointer[platformPruneWiring]

// platformDupSlot is the Engine's handle on the sweep's cache; empty = no findings.
type platformDupSlot = atomic.Pointer[publish.PlatformDupCache]

// SetPlatformDupCache wires the cache the duplicate-images checker reads.
func (e *Engine) SetPlatformDupCache(c *publish.PlatformDupCache) { e.platformDups.Store(c) }

// withPlatformDupFindings makes the duplicate-images checker also report the
// platform near-duplicates the background sweep cached (#3138 S3b). With
// prune_platform_copies off it returns the local checker's answer unchanged:
// entries outlive the option until the next sweep pass clears them.
func (e *Engine) withPlatformDupFindings(local Checker) Checker {
	return func(ctx context.Context, a *artist.Artist, cfg RuleConfig) *Violation {
		v := local(ctx, a, cfg)
		if !cfg.PrunePlatformCopies {
			return v
		}
		detail := e.platformDupDetail(ctx, a, cfg)
		if detail == "" {
			return v
		}
		if v == nil {
			return &Violation{
				RuleID:   RuleImageDuplicate,
				RuleName: "No duplicate images",
				Category: "image",
				Severity: effectiveSeverity(cfg),
				Message:  fmt.Sprintf("artist %s: near-duplicate backdrops on connected platforms: %s", a.Name, detail),
				Fixable:  true,
			}
		}
		// Fixable even over a local cross-type pair the fixer cannot remove:
		// the fix prunes the platform, then reports not-fixed, so the row stays open.
		v.Message += "; also near-duplicate backdrops on connected platforms: " + detail
		v.Fixable = true
		return v
	}
}

// platformDupDetail renders the artist's cached platform finding, or "" when
// there is none to raise. A cache read: no platform or network I/O.
//
// Only a Found entry computed at the tolerance the fixer would use now is a
// finding. Never swept, invalidated, another tolerance, clean and Undetermined
// (a policy skip, or a read the sweep could not complete) all read as "no
// finding", never an error. No age bound: the sweep's entry TTL is the one
// freshness policy. Artists the FIXER always refuses are filtered too; the
// cache holds findings for them and is not invalidated by a later lock.
func (e *Engine) platformDupDetail(ctx context.Context, a *artist.Artist, cfg RuleConfig) string {
	cache := e.platformDups.Load()
	if cache == nil || a.IsExcluded || a.Locked || a.Path == "" {
		return ""
	}
	// As the fixer and the sweep policy: a refused tolerance refuses there too.
	tol, ok := platformPruneTolerance(cfg.Tolerance)
	if !ok {
		return ""
	}
	entry := cache.Lookup(a.ID, tol)
	if entry.State != publish.PlatformDupFound || e.db == nil {
		return ""
	}
	// The cache is not invalidated when fanart becomes locked or user-set, or a
	// mapping or connection is removed or disabled. The prune refuses or skips
	// those, so re-check them (database reads only). Doubt = no finding.
	live := func(query string, args ...any) bool {
		n := 0
		return e.db.QueryRowContext(ctx, query, args...).Scan(&n) == nil && n > 0
	}
	if !live(`SELECT COUNT(*) = 0 FROM artist_images WHERE artist_id = ? AND image_type = 'fanart' AND (locked = 1 OR source = ?)`,
		a.ID, artist.ImageSourceUser) {
		return ""
	}
	// Fail closed like the fixer's SharedFSCheck: unknown counts as shared.
	if a.LibraryID == "" || e.libraryService == nil || e.IsSharedFilesystem(ctx, a) {
		return ""
	}
	parts := make([]string, 0, len(entry.Findings))
	for _, f := range entry.Findings {
		if !live(`SELECT COUNT(*) FROM artist_platform_ids m JOIN connections c ON c.id = m.connection_id
			WHERE m.artist_id = ? AND m.connection_id = ? AND c.enabled = 1 AND c.feature_image_write = 1`, a.ID, f.ConnectionID) {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s has %d redundant of %d backdrops", f.Connection, f.Redundant, f.Backdrops))
	}
	return strings.Join(parts, "; ")
}
