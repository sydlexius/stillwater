package mbidcheck

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	// Not guarded by a mutex because Run is not safe for concurrent use -- see
	// its doc comment. The scheduler is the only caller and is single-threaded.
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

	// SkippedTransient counts verdicts deliberately not persisted: provider
	// outages, unreadable paths, cancellation. High here with everything else
	// near zero is the signature of a sweep that could not reach MusicBrainz,
	// and is why a quiet ledger must never be read as a clean library.
	SkippedTransient int

	// SkippedNoMBID counts population rows whose artist turned out to carry no
	// stored id by the time it was loaded (it was cleared between the query and
	// the read). Not an error; the artist simply left the population.
	SkippedNoMBID int

	// Errored counts artists that could not be processed at all: the record
	// failed to load, the resolver reported a programmer error, or the ledger
	// write failed. Each is logged individually at ERROR.
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

// Run performs one pass and returns its counters.
//
// It returns an error when the pass could not proceed at all (the population
// query failed) and when the pass was ABANDONED PART-WAY because ctx was
// canceled -- in that second case the counters are still valid for what it
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
// NOT safe for concurrent use: it advances the shared cursor. The scheduler is
// the only production caller and is single-threaded.
func (s *Sweep) Run(ctx context.Context) (Counters, error) {
	var c Counters

	population, err := s.population.ListMBIDPopulation(ctx)
	if err != nil {
		return c, fmt.Errorf("mbidcheck: listing the sweep population: %w", err)
	}

	started := time.Now()
	slice, wrapped := s.selectSlice(population, s.cfg.maxPerPass())

	var canceled bool
	for _, row := range slice {
		// Between items rather than only at the top: a canceled sweep should
		// stop at the next artist, not after finishing the whole slice, and a
		// pass abandoned mid-way still logs what it managed.
		if ctx.Err() != nil {
			canceled = true
			s.logger.Info("mbid re-validation pass canceled part-way",
				slog.Int("checked", c.Checked),
				slog.String("last_artist_id", s.cursor))
			break
		}
		s.checkOne(ctx, row, &c)
		s.cursor = row.ArtistID
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
func (s *Sweep) checkOne(ctx context.Context, row artist.MBIDPath, c *Counters) {
	// ProviderIDs only: the resolver reads Name, SortName, Path and
	// MusicBrainzID, and the id lives in the artist_provider_ids side table.
	// Images and library membership are not read by anything downstream, so
	// hydrating them would be per-artist queries bought for nothing.
	a, err := s.artists.GetByID(ctx, row.ArtistID, artist.HydrateOpts{ProviderIDs: true})
	if err != nil {
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
	case artist.MBIDOutcomeNotCheckable:
		c.NotCheckable++
	}
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
	attrs := make([]any, 0, 12+len(catalogueBucketNames))
	attrs = append(attrs,
		slog.Int("population", populationSize),
		slog.Int("attempted", sliceSize),
		slog.Int("checked", c.Checked),
		slog.Int("validated", c.Validated),
		slog.Int("failed", c.Failed),
		slog.Int("not_checkable", c.NotCheckable),
		slog.Int("skipped_transient", c.SkippedTransient),
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
