package publish

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

// Background sweep that fills PlatformDupCache (#3138 S3a).
//
// DETECTION ONLY. Each artist goes through PrunePlatformBackdropsForArtist with
// DryRun set: the same entry point, survivor rule, fail-closed skips and
// per-target lock the fixer's platform phase uses, so the sweep and the fixer
// cannot disagree about what is redundant, and the sweep cannot delete.
//
// COST. One artist's read fetches every backdrop's bytes on every connection,
// so the sweep is sequential, capped per pass, and incremental: an artist with
// a current entry is skipped. After the first passes have walked the library,
// a pass only re-reads artists whose entry was invalidated or has aged out.
// An artist the perceptual tier would skip by policy (locked, protected fanart,
// no usable local folder) is decided from local state and costs no platform
// read at all.
//
// Known limits, left for a later change: the target lock is held across one
// artist's backdrop fetches; passes are not jittered; and the consecutive-
// failure stop does not tell a dead platform from a run of broken artists.

// PlatformDupSweepPolicy reports whether the sweep is wanted and at what
// tolerance. rule.Service.PlatformDupSweepPolicy is the production value; it is
// read again on every pass (and before every artist), so turning the rule
// option on or off, or changing the tolerance, needs no restart.
type PlatformDupSweepPolicy func(ctx context.Context) (tolerance float64, enabled bool, err error)

// PlatformDupSweepConfig paces the sweep. Zero fields take the defaults.
type PlatformDupSweepConfig struct {
	Interval     time.Duration // between passes (15m)
	StartupDelay time.Duration // before the first pass (2m)
	MaxPerPass   int           // artists read from the platform per pass (300)
	EntryTTL     time.Duration // re-read a decided entry after this (7 days)
	RetryAfter   time.Duration // re-read an undecided entry after this (6h)
}

// platformDupSweepMaxConsecutiveFailures stops a pass early when the platform
// is evidently down, rather than failing once per artist in the library.
const platformDupSweepMaxConsecutiveFailures = 10

// ErrPlatformDupSweepRunning is returned by Run while another Run is in flight.
var ErrPlatformDupSweepRunning = errors.New("platform duplicate sweep already running")

// PlatformDupSweepStats is one pass's tally, logged as its summary.
type PlatformDupSweepStats struct {
	Enabled      bool
	Tolerance    float64
	Artists      int // artists listed
	Swept        int // artists read through the publisher this pass
	Clean        int
	Found        int
	Redundant    int // redundant backdrops across Found artists
	Undetermined int
	NoTarget     int
	Discarded    int  // results dropped because a writer raced the read
	Stopped      bool // canceled, policy changed, cap reached, or platform down
	Duration     time.Duration
}

// PlatformDupSweep owns the cache and the loop that fills it.
type PlatformDupSweep struct {
	p       *Publisher
	policy  PlatformDupSweepPolicy
	cfg     PlatformDupSweepConfig
	cache   *PlatformDupCache
	logger  *slog.Logger
	running atomic.Bool
	now     func() time.Time
}

// NewPlatformDupSweep builds the sweep. THIS constructor is what installs the
// cache as the publisher's backdrop-write observer: before it runs, no write
// is observed. From then on every backdrop write that takes the per-target
// lock (rule fix, API prune, sync, push, image delete, phash repair) drops
// that artist's entry. One writer is still outside the lock: the generic
// imagebridge uploader, (*Bridge).UploadArtistImage, called today with "logo"
// only. If it is ever used for fanart, that write will not invalidate.
//
// A nil publisher or policy is a wiring bug and panics, as the sibling sweeps
// do: a sweep without either would run, log a tidy summary, and cache nothing.
func NewPlatformDupSweep(p *Publisher, policy PlatformDupSweepPolicy, cfg PlatformDupSweepConfig, logger *slog.Logger) *PlatformDupSweep {
	if p == nil || policy == nil {
		panic("publish.NewPlatformDupSweep: nil publisher or policy")
	}
	if logger == nil {
		logger = slog.Default()
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&cfg.Interval, 15*time.Minute)
	def(&cfg.StartupDelay, 2*time.Minute)
	def(&cfg.EntryTTL, 7*24*time.Hour)
	def(&cfg.RetryAfter, 6*time.Hour)
	if cfg.MaxPerPass <= 0 {
		cfg.MaxPerPass = 300
	}
	s := &PlatformDupSweep{
		p: p, policy: policy, cfg: cfg, cache: newPlatformDupCache(), now: time.Now,
		logger: logger.With(slog.String("component", "platform-dup-sweep")),
	}
	observer := s.cache.invalidateTarget
	p.backdropWriteObserver.Store(&observer)
	return s
}

// Cache is the read side, for the rule checker.
func (s *PlatformDupSweep) Cache() *PlatformDupCache { return s.cache }

// Start runs a pass after the startup delay and then on the interval, until
// ctx is canceled. It runs passes on its own goroutine only, so it returns
// (and leaks nothing) as soon as the pass in flight notices the cancellation.
func (s *PlatformDupSweep) Start(ctx context.Context) {
	timer := time.NewTimer(s.cfg.StartupDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.runWithRecover(ctx)
		timer.Reset(s.cfg.Interval)
	}
}

func (s *PlatformDupSweep) runWithRecover(ctx context.Context) {
	defer func() {
		if v := recover(); v != nil {
			s.logger.Error("platform duplicate sweep: panic recovered",
				slog.Any("panic", v), slog.String("stack", string(debug.Stack())))
		}
	}()
	if _, err := s.Run(ctx); err != nil && ctx.Err() == nil {
		s.logger.Warn("platform duplicate sweep pass failed", slog.String("error", err.Error()))
	}
}

// Run performs one pass. Two passes never overlap: the second returns
// ErrPlatformDupSweepRunning immediately.
func (s *PlatformDupSweep) Run(ctx context.Context) (PlatformDupSweepStats, error) {
	if !s.running.CompareAndSwap(false, true) {
		return PlatformDupSweepStats{}, ErrPlatformDupSweepRunning
	}
	defer s.running.Store(false)

	var st PlatformDupSweepStats
	start := s.now()
	tol, enabled, err := s.policy(ctx)
	if err != nil {
		return st, err // unknown policy: do nothing, and keep what is cached
	}
	if !enabled {
		// The gate that makes "option off" mean NO platform I/O. Findings for a
		// phase nobody enabled are dropped rather than left to go stale.
		s.cache.Clear()
		s.logger.Debug("platform duplicate sweep skipped: rule option is off")
		return st, nil
	}
	st.Enabled, st.Tolerance = true, tol
	err = s.sweep(ctx, tol, &st)
	st.Duration = s.now().Sub(start)
	level := slog.LevelInfo
	if st.Swept == 0 {
		level = slog.LevelDebug // an idle pass every interval is not news
	}
	s.logger.Log(ctx, level, "platform duplicate sweep pass complete",
		slog.Int("artists", st.Artists), slog.Int("swept", st.Swept),
		slog.Int("clean", st.Clean), slog.Int("with_findings", st.Found),
		slog.Int("redundant_backdrops", st.Redundant), slog.Int("undetermined", st.Undetermined),
		slog.Int("no_target", st.NoTarget), slog.Int("discarded", st.Discarded),
		slog.Bool("stopped_early", st.Stopped), slog.Float64("tolerance", tol),
		slog.String("duration", st.Duration.String()))
	return st, err
}

func (s *PlatformDupSweep) sweep(ctx context.Context, tol float64, st *PlatformDupSweepStats) error {
	failures := 0
	for page := 1; ; page++ {
		artists, _, err := s.p.artistLister.List(ctx, artist.ListParams{Page: page, PageSize: scanBackdropPageSize})
		if err != nil {
			st.Stopped = true
			return err
		}
		for i := range artists {
			st.Artists++
			if s.sweepArtist(ctx, &artists[i], tol, st, &failures) {
				st.Stopped = true
				return nil
			}
		}
		if len(artists) < scanBackdropPageSize {
			return nil
		}
	}
}

// sweepArtist handles one artist and reports whether the pass must stop.
//
// A panic is recovered HERE, per artist, and recorded as Undetermined. If only
// the whole pass were guarded, an artist that panics every time would end
// every pass at the same place, and no artist listed after it would ever be
// swept. The entry also keeps it from being retried before the retry window.
func (s *PlatformDupSweep) sweepArtist(ctx context.Context, a *artist.Artist, tol float64, st *PlatformDupSweepStats, failures *int) (stop bool) {
	undetermined := func(reason string) {
		s.cache.store(s.cache.begin(), a.ID, PlatformDupEntry{
			State: PlatformDupUndetermined, Reasons: []string{reason}, ComputedAt: s.now(), Tolerance: tol,
		}, nil)
		st.Undetermined++
	}
	defer func() {
		if v := recover(); v != nil {
			s.logger.Error("platform duplicate sweep: panic on one artist; continuing with the next",
				slog.String("artist_id", a.ID), slog.Any("panic", v), slog.String("stack", string(debug.Stack())))
			undetermined(PlatformDupReasonError)
		}
	}()
	if a.IsExcluded { // the rule pipeline never evaluates an excluded artist
		s.cache.Invalidate(a.ID)
		return false
	}
	if s.cache.fresh(a.ID, tol, s.now(), s.cfg.EntryTTL, s.cfg.RetryAfter) {
		return false
	}
	// Re-read the policy before each artist, so switching the option off (or
	// changing the tolerance) stops the pass here.
	if st.Swept >= s.cfg.MaxPerPass || !s.stillWanted(ctx, tol) {
		return true
	}
	if reason := s.policySkipReason(ctx, a); reason != "" {
		undetermined(reason) // known without asking the platform anything
		return false
	}
	window := s.cache.begin()
	// No cache lock is held here: this call does platform and disk I/O.
	res, runErr := s.p.PrunePlatformBackdropsForArtist(ctx, a, ArtistBackdropPruneOptions{Perceptual: true, DryRun: true, Tolerance: tol})
	if canceled(ctx) {
		// A canceled read fails like a dead platform. Storing nothing leaves
		// the artist Unknown, which is the truth.
		return true
	}
	st.Swept++
	entry, targets, hardFail := classifyPlatformDup(res, runErr)
	entry.ComputedAt, entry.Tolerance = s.now(), tol
	if !s.cache.store(window, a.ID, entry, targets) {
		st.Discarded++
		return false
	}
	switch entry.State {
	case PlatformDupClean:
		st.Clean++
	case PlatformDupFound:
		st.Found++
		st.Redundant += entry.Redundant()
	case PlatformDupUndetermined:
		st.Undetermined++
	case PlatformDupUnknown:
		st.NoTarget++
	}
	if *failures = nextFailureRun(*failures, hardFail); *failures >= platformDupSweepMaxConsecutiveFailures {
		s.logger.Warn("platform duplicate sweep: stopping the pass after repeated read failures",
			slog.Int("consecutive_failures", *failures))
		return true
	}
	return false
}

// policySkipReason reports, from the database and the local folder only, the
// reason the prune would refuse this artist's perceptual tier ("" when it
// would not, or when that cannot be told here). Without this the dry run would
// still download every backdrop for its exact tier, to reach an answer that
// was known before any platform request. The same checks run again inside the
// prune, so this is a shortcut and never the authority.
func (s *PlatformDupSweep) policySkipReason(ctx context.Context, a *artist.Artist) string {
	stored, err := s.p.artistGetter.GetByID(ctx, a.ID, artist.HydrateOpts{})
	if err != nil || stored == nil {
		return ""
	}
	if ids, idErr := s.p.artistService.GetPlatformIDs(ctx, a.ID); idErr != nil || len(ids) == 0 {
		return ""
	}
	if stored.Locked {
		return PruneSkipLockedArtist
	}
	var skip *pruneSkipError
	if _, optsErr := s.p.perceptualOptsFor(ctx, stored); errors.As(optsErr, &skip) {
		return skip.reason
	}
	return ""
}

// stillWanted re-reads the policy mid-pass. Any doubt (canceled, unreadable,
// switched off, another tolerance) ends the pass; the next one starts clean.
func (s *PlatformDupSweep) stillWanted(ctx context.Context, tol float64) bool {
	now, on, err := s.policy(ctx)
	if err != nil && !canceled(ctx) {
		s.logger.Warn("platform duplicate sweep: policy unreadable mid-pass; stopping the pass",
			slog.String("error", err.Error()))
	}
	return !canceled(ctx) && err == nil && on && now == tol
}

func canceled(ctx context.Context) bool { return ctx.Err() != nil }

func nextFailureRun(run int, failed bool) int {
	if failed {
		return run + 1
	}
	return 0
}

// classifyPlatformDup turns one dry run into a cache entry. Any doubt (an
// error, a policy skip, a failed or unhealthy connection) is Undetermined: a
// partial read can neither prove the artist clean nor be trusted as a finding.
// hardFail reports a failure that was not a policy skip.
func classifyPlatformDup(res PlatformBackdropPruneResult, runErr error) (e PlatformDupEntry, targets []string, hardFail bool) {
	for _, t := range res.Examined {
		targets = append(targets, backdropTargetKey(t.ConnectionID, t.PlatformArtistID))
	}
	inFailures := 0 // every policy skip but the locked artist is also in Failures
	for _, sk := range res.Skipped {
		e.Reasons = append(e.Reasons, sk.Reason)
		if sk.Reason != PruneSkipLockedArtist {
			inFailures++
		}
	}
	hardFail = runErr != nil || len(res.Failures) > inFailures
	if runErr != nil {
		e.Reasons = append(e.Reasons, PlatformDupReasonError)
	} else if hardFail {
		e.Reasons = append(e.Reasons, PlatformDupReasonReadFailed)
	}
	if len(res.Unhealthy) > 0 {
		e.Reasons = append(e.Reasons, PlatformDupReasonUnhealthy)
	}
	switch {
	case len(e.Reasons) > 0:
		e.State = PlatformDupUndetermined
	case len(res.Examined) == 0:
		// Nothing to read (unmapped, or no enabled image-write connection).
		// Stays Unknown; the entry only stops the sweep re-checking every pass.
		e.Reasons = []string{PlatformDupReasonNoTarget}
	case len(res.Plan) == 0:
		e.State = PlatformDupClean
	default:
		e.State = PlatformDupFound
		redundant := map[string]int{}
		for _, pl := range res.Plan {
			redundant[pl.ConnectionID]++
		}
		for _, t := range res.Examined {
			if n := redundant[t.ConnectionID]; n > 0 {
				e.Findings = append(e.Findings, PlatformDupFinding{
					ConnectionID: t.ConnectionID, Connection: t.Connection, Backdrops: t.Backdrops, Redundant: n,
				})
			}
		}
	}
	return e, targets, hardFail
}
