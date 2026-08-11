package mbidcheck

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

// The rate-limited background sweep that drives the resolver (#2810).
//
// # WHY A SWEEP RATHER THAN A RULE
//
// Checking one stored id costs roughly two MusicBrainz requests, and the
// shared singleton limiter caps the product at one request per second across
// every provider call Stillwater makes. A library of a few thousand artists is
// therefore hours of wall-clock work, which rules out doing it synchronously
// inside Run Rules and rules out doing all of it in one pass.
//
// This file owns the pacing and the bookkeeping. It owns no classification
// whatsoever: every verdict comes from Resolver.Resolve, and the one policy
// decision made here is WHETHER TO PERSIST a verdict.
//
// # THAT ONE POLICY DECISION
//
// Result.Transient means "this verdict reflects a condition on OUR side": a
// provider outage, a timeout, a canceled sweep, an unreadable library path, an
// album source that broke its own contract. A transient verdict is NEVER
// written. Not over a prior verdict, and not as a first verdict either.
//
// Over a prior verdict it would destroy evidence: a MusicBrainz outage lasting
// one sweep would rewrite every real "failed" finding to "not checkable,
// provider unavailable", and the operator's queue would empty itself overnight
// on account of a network blip. That is the single worst thing this feature
// could do, so the skip is unconditional rather than a comparison against what
// is already stored.
//
// Skipping it as a FIRST verdict too is the less obvious half, and it buys a
// property worth having: because EvidenceUnknown is always transient and
// EvidenceNone never is, EVERY no_local_albums row that reaches the ledger
// means "this artist genuinely has no albums on disk". The ledger's reason
// vocabulary is frozen in a SQL CHECK and cannot carry the tri-state
// artist.AlbumEvidence, so without this the two would be indistinguishable
// once persisted and a permanent state would be re-checked forever while a
// retryable one looked settled. An artist skipped this way simply has no row,
// which reads as "never checked" -- true, and the honest answer.
//
// # WHERE THE COUNTERS COME IN
//
// A sweep that checked nothing must not look like a sweep that found nothing.
// Every pass logs a summary carrying the outcome counts AND a distribution of
// catalogue-match percentages, so an operator can see both what was found and
// whether the threshold that produced it is set anywhere near right.

// Population supplies the artists to check. *artist.Service satisfies it.
//
// Narrow on purpose: the sweep needs exactly one query and must not acquire
// the ability to write through this seam.
type Population interface {
	ListMBIDPopulation(ctx context.Context) ([]artist.MBIDPath, error)
}

// ArtistGetter loads one artist record. *artist.Service satisfies it.
type ArtistGetter interface {
	GetByID(ctx context.Context, id string, opts ...artist.HydrateOpts) (*artist.Artist, error)
}

// Ledger persists verdicts. artist.MBIDValidationRepository satisfies it.
//
// Only Upsert is required. The sweep never reads the ledger back: the
// skip-don't-clear policy is unconditional, so it has no decision that depends
// on what is already stored, and a read-modify-write would add a race for no
// gain.
type Ledger interface {
	Upsert(ctx context.Context, v *artist.MBIDValidation) error
}

// Flagger raises the operator-review entry for a failed verdict.
// *rule.Service satisfies it via RaiseMBIDValidationFailure.
//
// A plain-string seam, so this package does not import internal/rule and does
// not get a vote on the rule id, the severity, or -- the one that matters --
// whether the entry is fixable. There is no automated fix for this finding and
// #2810's acceptance criteria forbid one, so that decision stays on the rule
// side where it cannot be passed in.
//
// Optional: a nil Flagger means verdicts reach the ledger and nothing else,
// which is what the sweep does in a build with no rule service wired.
type Flagger interface {
	RaiseMBIDValidationFailure(ctx context.Context, artistID, artistName, message string) error
}

// Default sweep pacing. All three are overridable through Config.
const (
	// DefaultInterval is how often a pass runs. Daily: a stored id changes
	// rarely, and the pass is bounded by MusicBrainz's rate limit rather than
	// by anything local.
	DefaultInterval = 24 * time.Hour

	// DefaultStartupDelay keeps the first pass off the boot path, where
	// migrations and the initial library scan are already contending.
	DefaultStartupDelay = 5 * time.Minute

	// DefaultMaxPerPass bounds one pass. At roughly two requests per artist
	// against a 1 req/sec shared cap, 200 artists is about seven minutes of
	// limiter time -- enough to make progress, small enough that the sweep is
	// never the reason another provider call waits for long.
	DefaultMaxPerPass = 200
)

// Config is the sweep's tunable surface. The zero value is usable: every field
// falls back to its documented default.
type Config struct {
	// Interval between passes. Non-positive means DefaultInterval.
	Interval time.Duration

	// StartupDelay before the first pass. Non-positive means
	// DefaultStartupDelay.
	StartupDelay time.Duration

	// MaxPerPass caps how many artists one pass checks. Non-positive means
	// DefaultMaxPerPass.
	MaxPerPass int
}

func (c Config) interval() time.Duration {
	if c.Interval <= 0 {
		return DefaultInterval
	}
	return c.Interval
}

func (c Config) startupDelay() time.Duration {
	if c.StartupDelay <= 0 {
		return DefaultStartupDelay
	}
	return c.StartupDelay
}

func (c Config) maxPerPass() int {
	if c.MaxPerPass <= 0 {
		return DefaultMaxPerPass
	}
	return c.MaxPerPass
}

// Sweep re-validates stored MusicBrainz ids in the background.
type Sweep struct {
	population Population
	artists    ArtistGetter
	ledger     Ledger
	resolver   *Resolver
	cfg        Config
	logger     *slog.Logger

	// flaggerMu guards flagger, which is the ONE field written after
	// construction.
	//
	// A mutex rather than a documented "wire it before you launch the
	// goroutine" contract, because that contract fights the setter's own
	// justification: SetFlagger exists precisely BECAUSE the rule service is
	// built late, so the shape it invites is a wiring author calling it after
	// `go Start(ctx)` has already begun a pass. That is a genuine data race
	// (write here, read in flag), it is reachable through the documented
	// wiring, and it only manifests once a FAILED verdict reaches flag -- so a
	// smoke test over a healthy library would never surface it.
	flaggerMu sync.RWMutex
	flagger   Flagger

	// cursor is the artist id the NEXT pass starts after.
	//
	// Without it, MaxPerPass would make the sweep re-check the same first N
	// artists forever while the rest of the library was never checked at all --
	// a scheduler that looks busy and covers a fixed slice. The population is
	// ordered by artist id, so advancing past the last id checked and wrapping
	// to the start on exhaustion walks the whole library in ceil(N/max) passes.
	//
	// In memory rather than persisted, deliberately: a restart re-checking some
	// artists early costs a few requests, while a persisted cursor is a schema
	// change and a new way for the sweep to get permanently stuck at a bad row.
	//
	// Not guarded by a mutex because Run is called from one goroutine only --
	// see its doc comment.
	cursor string
}

// NewSweep builds a sweep. Every dependency is required; a nil one is a
// programming error and panics, because a sweep missing its ledger or its
// population would run to completion, log a tidy summary, and persist nothing.
func NewSweep(population Population, artists ArtistGetter, ledger Ledger, resolver *Resolver, cfg Config, logger *slog.Logger) *Sweep {
	switch {
	case population == nil:
		panic("mbidcheck.NewSweep: nil population")
	case artists == nil:
		panic("mbidcheck.NewSweep: nil artist getter")
	case ledger == nil:
		panic("mbidcheck.NewSweep: nil ledger")
	case resolver == nil:
		panic("mbidcheck.NewSweep: nil resolver")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Sweep{
		population: population,
		artists:    artists,
		ledger:     ledger,
		resolver:   resolver,
		cfg:        cfg,
		logger:     logger.With(slog.String("component", "mbid-revalidate")),
	}
}

// SetFlagger attaches the surface that turns a FAILED verdict into an
// operator-review entry. Optional; without it the sweep still writes the
// ledger, which is the record of what was checked.
//
// A setter rather than a constructor parameter because the rule service is
// built after the artist service in main.go's wiring order, matching the
// late-wiring shape the rule fixers already use. A nil argument is ignored so
// a call made out of order cannot silently disconnect a working flagger.
//
// Safe to call at any time, including while Start's goroutine is mid-pass:
// the field is guarded. See the flaggerMu comment for why that guard is a
// mutex rather than a "wire it first" instruction.
//
// "Nil" here means nil in EITHER of the two ways an interface can be nil. A
// caller holding a `var svc *rule.Service` that was never built passes a
// non-nil interface wrapping a nil pointer, for which `f != nil` is TRUE, so a
// plain check would store it and flag would call a method on a nil receiver on
// the first failed verdict -- a panic in a background goroutine, hours after
// the wiring ran. The reflect check is the only way to see through the
// interface box.
func (s *Sweep) SetFlagger(f Flagger) {
	if f == nil {
		return
	}
	if v := reflect.ValueOf(f); v.Kind() == reflect.Pointer && v.IsNil() {
		return
	}
	s.flaggerMu.Lock()
	defer s.flaggerMu.Unlock()
	s.flagger = f
}

// currentFlagger reads the flagger under its guard. Returns nil when none is
// wired, which is a supported configuration (see Flagger).
func (s *Sweep) currentFlagger() Flagger {
	s.flaggerMu.RLock()
	defer s.flaggerMu.RUnlock()
	return s.flagger
}

// Counters is one pass's tally, returned by Run and logged as its summary.
//
// It exists to answer two different questions with one record. The first is
// operational: did this pass do anything, and did it find anything. The second
// is about the CONFIGURATION, and is the reason for the percentage
// distribution: DefaultCatalogueMatchPercent has never been run against a real
// library, so an operator needs to see how the artists it actually compared
// were distributed before deciding whether 25 is right. A pass reporting
// "18 failed, all of them at 0% against an empty remote catalogue" says the
// threshold is doing exactly what it was designed for; one reporting "300
// failed, most between 10% and 24%" says it is too high for that library.
type Counters struct {
	// Checked is how many artists the resolver returned a verdict for.
	Checked int

	// Validated, Failed and NotCheckable partition the PERSISTED verdicts.
	Validated    int
	Failed       int
	NotCheckable int

	// SkippedTransient counts verdicts that did not reach the ledger for a
	// reason on OUR side: provider outages, unreadable paths, and a LEDGER
	// write abandoned mid-flight by cancellation or a deadline. High here with
	// everything else near zero is the signature of a sweep that could not
	// reach MusicBrainz, and is why a quiet ledger must never be read as a
	// clean library. An abandoned operator-review entry, whose verdict DID
	// reach the ledger, is SkippedFlag rather than this: counting it here would
	// blunt that signature with a condition that lost no evidence.
	SkippedTransient int

	// SkippedFlag counts operator-review entries abandoned mid-flight by
	// cancellation or a deadline, for a verdict that DID reach the ledger.
	//
	// Deliberately not folded into SkippedTransient. By the time the queue
	// entry is attempted the verdict is durably persisted, so the evidence
	// survives and only the review entry's visibility is lost -- a materially
	// different condition from a verdict that never landed at all, and one that
	// must not blunt SkippedTransient's "the sweep could not reach
	// MusicBrainz" signature by inflating it from an unrelated cause. High
	// here means the pass stopped while raising findings; re-running covers
	// them, since the ledger rows they came from are already durable.
	SkippedFlag int

	// SkippedNoMBID counts population rows whose artist turned out to carry no
	// stored id by the time it was loaded (it was cleared between the query and
	// the read). Not an error; the artist simply left the population.
	SkippedNoMBID int

	// Errored counts artists that could not be processed at all: the record
	// failed to load, the resolver reported a programmer error, or the ledger
	// write failed for a real reason. Each is logged individually at ERROR.
	//
	// A write abandoned because the context was canceled or its deadline
	// expired is NOT counted here: that is our own shutdown, not a fault of the
	// artist, and blaming individual artists for it would make a routine stop
	// read as a library-wide failure. It goes to SkippedTransient (a ledger
	// write) or SkippedFlag (an operator-review entry) instead.
	Errored int

	// ZeroRemoteCatalogue counts the headline finding: the id resolves, the
	// operator holds albums, and MusicBrainz lists no releases at all. Broken
	// out because it is qualitatively different from a low overlap percentage
	// and is the shape the production snapshot showed.
	ZeroRemoteCatalogue int

	// CatalogueBuckets distributes catalogue-match percentage over the artists
	// that were actually compared, in the order named by catalogueBucketNames.
	// Only comparisons that ran are counted; a not-measured verdict carries no
	// percentage and contributes to none of these.
	CatalogueBuckets [5]int
}

// catalogueBucketNames labels Counters.CatalogueBuckets, and the boundaries are
// chosen around the default threshold of 25 so a summary shows what moving it
// would do: "0" is the total-disagreement case the feature exists to catch,
// "1-24" is everything a lower threshold would stop flagging, and "25-49" is
// what a higher one would start flagging.
var catalogueBucketNames = [5]string{
	"catalogue_pct_0",
	"catalogue_pct_1_24",
	"catalogue_pct_25_49",
	"catalogue_pct_50_74",
	"catalogue_pct_75_100",
}

// catalogueBucket maps a 0-100 percentage onto a CatalogueBuckets index.
func catalogueBucket(pct float64) int {
	switch {
	case pct <= 0:
		return 0
	case pct < 25:
		return 1
	case pct < 50:
		return 2
	case pct < 75:
		return 3
	default:
		return 4
	}
}

// Start runs a pass after the configured startup delay and then on the
// configured interval, until ctx is canceled.
//
// Mirrors maintenance.Service.StartExistsFlagScanner: the startup pass matters
// because an operator who has just restarted to pick up this feature should not
// wait a full day to see whether their library holds a misidentified id.
//
// Blocks until ctx is done; the caller launches it with `go`.
func (s *Sweep) Start(ctx context.Context) {
	interval := s.cfg.interval()
	delay := s.cfg.startupDelay()

	s.logger.Info("mbid re-validation sweep started",
		slog.String("interval", interval.String()),
		slog.String("startup_delay", delay.String()),
		slog.Int("max_per_pass", s.cfg.maxPerPass()))

	// NewTimer with an explicit Stop, not time.After: on the cancellation path
	// time.After leaves the timer armed for the whole delay, and this delay is
	// minutes in production (hours in a test). Go 1.23+ makes an unreferenced
	// timer collectable, so this is not a classic leak -- but stopping it hands
	// the runtime timer back at once instead of waiting on a GC cycle, and it
	// removes the question rather than leaving a reader to reason about it.
	startup := time.NewTimer(delay)
	defer startup.Stop()

	select {
	case <-ctx.Done():
		s.logger.Info("mbid re-validation sweep stopped before its first pass")
		return
	case <-startup.C:
	}

	s.runOnce(ctx, "initial mbid re-validation pass failed")

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("mbid re-validation sweep stopped")
			return
		case <-ticker.C:
			s.runOnce(ctx, "mbid re-validation pass failed")
		}
	}
}

// runOnce performs one pass and reports its outcome at the RIGHT level.
//
// Run deliberately returns ctx.Err() for a pass abandoned part-way, and a pass
// is minutes of limiter-paced work, so a service stopping mid-pass is the
// NORMAL case rather than a fault. Treating every non-nil error as a failure
// would put an ERROR line in the log on every clean shutdown -- the same
// "a condition on OUR side reported as a fault" defect this feature has had to
// correct at each of its write sites, here at the pass level.
//
// isCanceled rather than an == comparison, and the distinction is load-bearing
// here: the two cancellation paths return DIFFERENT shapes. A pass abandoned
// mid-flight comes back WRAPPED ("mbidcheck: listing the sweep population:
// context canceled"), where errors.Is is true but == is false, while a
// pre-canceled context yields the bare sentinel. An == check would quiet only
// the second, which is the case that barely happens.
func (s *Sweep) runOnce(ctx context.Context, failureMsg string) {
	_, err := s.Run(ctx)
	if err == nil {
		return
	}
	if isCanceled(err) {
		// Info, not Debug: a shutdown that cut a pass short is worth one line
		// at the level an operator reads, because it says the cycle was
		// incomplete and the library is not fully covered.
		s.logger.Info("mbid re-validation pass stopped before finishing", slog.Any("reason", err))
		return
	}
	s.logger.Error(failureMsg, slog.Any("error", err))
}

// Run performs one pass and returns its counters.
//
// It returns an error when the pass could not proceed at all (the population
// query failed) and whenever ctx was canceled at ANY point during the pass --
// including during the last row's own work, where there is no next iteration
// to notice it. In that second case the counters are still valid for what it
// managed, and the error is ctx.Err(), so errors.Is(err, context.Canceled)
// distinguishes a normal shutdown from a real failure. A per-artist failure is
// counted, logged, and stepped over: one unreadable artist must not abort a
// pass that still has hundreds of usable ones ahead of it.
//
// Reporting cancellation is deliberate, and it is the same principle the rest
// of this feature is built on: a partial pass must never be indistinguishable
// from a complete one. The Counters cannot express it -- a pass that stopped
// after 3 artists and a pass whose whole slice was 3 artists produce identical
// tallies -- so the return value is the only place the distinction can live. A
// caller that treats cancellation as routine (the scheduler, on shutdown)
// checks errors.Is and stays quiet; a caller that needs to know the cycle was
// incomplete can, which it could not if this returned nil.
//
// NOT safe for concurrent use: it advances the shared cursor. Call it from one
// goroutine only. Start satisfies that; a second caller would race.
func (s *Sweep) Run(ctx context.Context) (Counters, error) {
	var c Counters

	population, err := s.population.ListMBIDPopulation(ctx)
	if err != nil {
		return c, fmt.Errorf("mbidcheck: listing the sweep population: %w", err)
	}

	started := time.Now()
	slice, wrapped := s.selectSlice(population, s.cfg.maxPerPass())

	for _, row := range slice {
		// Between items rather than only at the top: a canceled sweep should
		// stop at the next artist, not after finishing the whole slice. This
		// check exists to STOP WORK PROMPTLY; it is not what decides whether the
		// pass was abandoned.
		if ctx.Err() != nil {
			break
		}
		s.checkOne(ctx, row, &c)

		// The cursor advances on every row checkOne returned from, including the
		// row cancellation landed in the middle of. checkOne always reaches a
		// terminal decision for its row -- persisted, deliberately skipped, or
		// counted -- and after the fact there is no way to tell "canceled before
		// any work" from "canceled after the write landed". Rewinding on the
		// doubt would be worse than the doubt: a sweep canceled at the same
		// point every pass (a service restarted on a timer, say) would re-check
		// that one artist forever and never advance, which is the stuck-cursor
		// failure the cursor exists to prevent. A row whose write WAS cut short
		// leaves no ledger entry, so it is simply re-checked on the next cycle.
		s.cursor = row.ArtistID
	}

	// Cancellation is read AFTER the loop, not from the in-loop break.
	//
	// The between-items check can only fire when there IS a next iteration, so a
	// context canceled during the FINAL row's work -- inside the resolver call
	// or inside the ledger write -- let the loop end normally and left the pass
	// claiming a complete cycle: cursor cleared, population_exhausted true, no
	// error returned. That is the same "abandoned tail reported as clean" defect
	// this cursor logic already guards, surviving on the one path the in-loop
	// check cannot reach.
	//
	// One post-loop read covers every path because context.Err is monotonic:
	// once it is non-nil it stays non-nil, so this subsumes the in-loop
	// observation rather than merely duplicating it.
	canceled := ctx.Err() != nil
	if canceled {
		s.logger.Info("mbid re-validation pass canceled part-way",
			slog.Int("checked", c.Checked),
			slog.String("last_artist_id", s.cursor))
	}

	// The population is exhausted only when the slice both REACHED its end
	// (wrapped) and was actually PROCESSED to that end (not canceled). Those
	// are two different facts and conflating them is a way to lose artists
	// permanently: selectSlice reports the slice's SHAPE, and a pass abandoned
	// mid-slice has a tail nobody looked at. Clearing the cursor on that would
	// send the next pass back to the head of the population, so with a
	// MaxPerPass smaller than the library and cancellation recurring near the
	// tail, the same artists would be skipped every single pass while the
	// summary reported a clean, exhausted sweep -- the "unknown rendered as
	// clean" failure this whole feature exists to prevent, reproduced inside
	// it.
	exhausted := wrapped && !canceled
	if exhausted {
		// Wrap, so the NEXT pass starts from the top of the population rather
		// than idling past the end.
		s.cursor = ""
	}

	s.logSummary(c, len(population), len(slice), exhausted, time.Since(started))
	if canceled {
		return c, ctx.Err()
	}
	return c, nil
}

// selectSlice returns the next up-to-limit rows after the cursor, and whether
// the population was exhausted (so the cursor should wrap).
//
// The population is ordered by artist id, so "after the cursor" is a simple
// scan. An id that has since been deleted does not strand the sweep: the
// comparison is `>`, not equality, so the next larger id is picked up.
func (s *Sweep) selectSlice(population []artist.MBIDPath, limit int) ([]artist.MBIDPath, bool) {
	start := 0
	if s.cursor != "" {
		for start < len(population) && population[start].ArtistID <= s.cursor {
			start++
		}
	}
	if start >= len(population) {
		// The cursor is past the end (or the population shrank). Restart from
		// the top THIS pass rather than burning one doing nothing.
		start = 0
	}
	end := start + limit
	if end >= len(population) {
		return population[start:], true
	}
	return population[start:end], false
}

// checkOne resolves and persists a single artist's verdict, updating c.
//
// It reports nothing back about cancellation and does not need to: Run reads
// ctx.Err() after the loop, so a pass cut short inside this function is
// surfaced there. What checkOne owes is that a canceled step never lands in a
// counter or a log line that blames the artist for it.
func (s *Sweep) checkOne(ctx context.Context, row artist.MBIDPath, c *Counters) {
	// ProviderIDs only: the resolver reads Name, SortName, Path and
	// MusicBrainzID, and the id lives in the artist_provider_ids side table.
	// Images and library membership are not read by anything downstream, so
	// hydrating them would be per-artist queries bought for nothing.
	a, err := s.artists.GetByID(ctx, row.ArtistID, artist.HydrateOpts{ProviderIDs: true})
	if err != nil {
		// Same rule as the ledger write below: a read cut short by shutdown is
		// our condition, not this artist's. Reached earlier in the pass than the
		// write is, so on a cancellation it is the one that fires for the whole
		// remaining slice.
		if isCanceled(err) {
			c.SkippedTransient++
			s.logger.Debug("mbid re-validation abandoned an artist load during shutdown",
				slog.String("artist_id", row.ArtistID),
				slog.Any("error", err))
			return
		}
		c.Errored++
		s.logger.Error("mbid re-validation could not load an artist",
			slog.String("artist_id", row.ArtistID),
			slog.Any("error", err))
		return
	}

	res, err := s.resolver.Resolve(ctx, a)
	if err != nil {
		// Resolve's error channel carries programmer errors only; every
		// operational condition comes back as a Result. ErrNoStoredMBID is the
		// benign one: the id was cleared between the population query and this
		// read, so the artist has simply left the population.
		if errors.Is(err, ErrNoStoredMBID) {
			c.SkippedNoMBID++
			return
		}
		c.Errored++
		s.logger.Error("mbid re-validation could not classify an artist",
			slog.String("artist_id", row.ArtistID),
			slog.Any("error", err))
		return
	}
	c.Checked++

	if res.Validation.CatalogueMatchPercent != nil {
		c.CatalogueBuckets[catalogueBucket(*res.Validation.CatalogueMatchPercent)]++
	}
	if res.ZeroRemoteCatalogue() {
		c.ZeroRemoteCatalogue++
	}

	// THE SKIP. See this file's header: a verdict that describes our own
	// conditions never reaches the ledger, so a provider outage cannot overwrite
	// a real finding and a persisted no_local_albums row always means the
	// artist genuinely has none.
	if res.Transient {
		c.SkippedTransient++
		return
	}

	if err := s.ledger.Upsert(ctx, &res.Validation); err != nil {
		// A write cut short by shutdown is not this artist's fault, and it is
		// the same rule the resolver applies to a provider outage: a condition
		// on OUR side is never attributed to the artist. Counting it as Errored
		// and logging at ERROR turns a routine stop into a burst of one error
		// line per remaining artist, each naming an artist that is fine.
		//
		// It lands in SkippedTransient, whose documented meaning already covers
		// cancellation, so the pass still reports that a verdict did not get
		// written rather than silently losing it. Run's post-loop ctx.Err()
		// check is what turns this into the pass-level error; no signal has to
		// be threaded back from here.
		//
		if isCanceled(err) {
			c.SkippedTransient++
			s.logger.Debug("mbid re-validation abandoned a verdict write during shutdown",
				slog.String("artist_id", row.ArtistID),
				slog.Any("error", err))
			return
		}
		c.Errored++
		s.logger.Error("mbid re-validation could not persist a verdict",
			slog.String("artist_id", row.ArtistID),
			slog.String("outcome", string(res.Validation.Outcome)),
			slog.Any("error", err))
		return
	}

	switch res.Validation.Outcome {
	case artist.MBIDOutcomeValidated:
		c.Validated++
	case artist.MBIDOutcomeFailed:
		c.Failed++
		s.flag(ctx, a, res, c)
	case artist.MBIDOutcomeNotCheckable:
		c.NotCheckable++
	}
}

// isCanceled reports whether err is our own shutdown rather than a fault of
// the artist being processed.
//
// errors.Is, never == and never a string match: a repository wraps its driver
// errors, so the sentinel arrives buried, and the two comparisons that would
// work on a bare error silently stop matching the moment anything wraps it.
func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// flag raises the operator-review entry for a failed verdict.
//
// ONLY a failed verdict reaches here, and that restriction is the point.
// A validated verdict has nothing to review. A not-checkable one is an honest
// "unknown" -- an id that resolves to nobody may well be correct and merely
// stale, and an artist with no albums on disk simply could not be compared --
// and putting either in the review queue would bury the real findings under
// states that mean nothing is wrong. Those live in the ledger, which is where
// an operator goes to ask what was checked rather than what was found.
//
// A failure to raise is logged and counted, not returned: the verdict is
// already durably in the ledger at this point, so losing the queue entry costs
// visibility rather than evidence, and it is not worth abandoning the pass for.
func (s *Sweep) flag(ctx context.Context, a *artist.Artist, res Result, c *Counters) {
	f := s.currentFlagger()
	if f == nil {
		return
	}
	if err := f.RaiseMBIDValidationFailure(ctx, a.ID, a.Name, s.flagMessage(res)); err != nil {
		// The same rule the ledger write applies, on the third and last write
		// this pass performs. RaiseMBIDValidationFailure runs a transaction on
		// this same ctx, so a shutdown mid-pass fails it for every remaining
		// failed verdict at once: without this branch a routine stop produces
		// one ERROR line per artist, each naming an artist that is fine, and
		// inflates Errored against the invariant Counters.Errored documents.
		if isCanceled(err) {
			// SkippedFlag, not SkippedTransient: the verdict itself reached the
			// ledger a moment ago (Upsert succeeded and Failed was counted), so
			// this is a lost review entry, not a verdict that never landed.
			c.SkippedFlag++
			s.logger.Debug("mbid re-validation abandoned an operator-review entry during shutdown",
				slog.String("artist_id", a.ID),
				slog.Any("error", err))
			return
		}
		c.Errored++
		s.logger.Error("mbid re-validation could not raise an operator-review entry",
			slog.String("artist_id", a.ID),
			slog.Any("error", err))
	}
}

// flagMessage is the operator-facing sentence for a failed verdict.
//
// The zero-remote-catalogue case gets its own wording, read from
// Result.ZeroRemoteCatalogue rather than from the detail prose. It is
// qualitatively different from a low overlap percentage -- the id belongs to
// someone with no releases at all -- and it is the shape the production
// snapshot showed, so an operator scanning a queue should be able to recognize
// it without opening the row.
func (s *Sweep) flagMessage(res Result) string {
	if res.ZeroRemoteCatalogue() {
		return fmt.Sprintf(
			"The stored MusicBrainz ID resolves to %q, which lists no releases at all, while %d album(s) are on disk. The ID most likely belongs to a different artist of the same name. Nothing has been changed.",
			res.Validation.ResolvedName, res.LocalAlbumCount)
	}
	// The reason is a MACHINE identifier (catalogue_mismatch, name_mismatch,
	// ...) frozen in a SQL CHECK, so it must never reach an operator: it puts
	// an internal code with underscores in the Action Queue. Detail already
	// states the same fact in English, with the measured numbers, so the code
	// added nothing but noise. Read Detail; never re-introduce Reason here.
	return fmt.Sprintf(
		"The stored MusicBrainz ID failed re-validation: %s. Nothing has been changed.",
		res.Validation.Detail)
}

// logSummary emits the one line per pass an operator reads.
//
// At INFO including when nothing was found, because "the sweep ran and found
// nothing" and "the sweep did not run" must be distinguishable in a log, and a
// summary that only appears on findings makes them identical.
//
// exhausted is the caller's verdict on whether the pass reached the end of the
// population AND processed it, never merely the shape of the slice: an
// abandoned tail must not be reported as a completed cycle.
func (s *Sweep) logSummary(c Counters, populationSize, sliceSize int, exhausted bool, elapsed time.Duration) {
	attrs := make([]any, 0, 13+len(catalogueBucketNames))
	attrs = append(attrs,
		slog.Int("population", populationSize),
		slog.Int("attempted", sliceSize),
		slog.Int("checked", c.Checked),
		slog.Int("validated", c.Validated),
		slog.Int("failed", c.Failed),
		slog.Int("not_checkable", c.NotCheckable),
		slog.Int("skipped_transient", c.SkippedTransient),
		slog.Int("skipped_flag", c.SkippedFlag),
		slog.Int("skipped_no_mbid", c.SkippedNoMBID),
		slog.Int("errored", c.Errored),
		slog.Int("zero_remote_catalogue", c.ZeroRemoteCatalogue),
		slog.Bool("population_exhausted", exhausted),
		slog.String("elapsed", elapsed.Round(time.Millisecond).String()),
	)
	for i, name := range catalogueBucketNames {
		attrs = append(attrs, slog.Int(name, c.CatalogueBuckets[i]))
	}
	s.logger.Info("mbid re-validation pass complete", attrs...)
}
