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
	f.platformPrune.Store(&platformPruneWiring{pruner: p, onPrune: onPrune})
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
	if res.BackdropsRemoved > 0 {
		fr.Irreversible = true
		// A clean local folder plus a platform delete is a completed fix. A
		// blocked local phase is not: its duplicates are still on disk.
		if local == localClean {
			fr.Fixed = true
		}
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
