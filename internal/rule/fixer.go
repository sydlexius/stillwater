package rule

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/event"
	"github.com/sydlexius/stillwater/internal/publish"
)

// Fixer attempts to automatically resolve a rule violation.
type Fixer interface {
	// CanFix returns true if this fixer handles the given violation's rule.
	CanFix(v *Violation) bool
	// Fix attempts to resolve the violation for the given artist.
	Fix(ctx context.Context, a *artist.Artist, v *Violation) (*FixResult, error)
}

// CandidateDiscoverer is an optional interface that fixers may implement to
// indicate they support candidate discovery without side effects. In manual
// automation mode the pipeline only invokes Fix on fixers that implement this
// interface. Fixers that may write to disk as part of fixing (LogoPaddingFixer,
// NFOFixer, ExtraneousImagesFixer) must NOT implement it.
type CandidateDiscoverer interface {
	SupportsCandidateDiscovery() bool
}

// FixResult describes the outcome of a fix attempt.
type FixResult struct {
	RuleID     string           `json:"rule_id"`
	Fixed      bool             `json:"fixed"`
	Dismissed  bool             `json:"dismissed,omitempty"` // true when violation was auto-dismissed (e.g. orphaned artist)
	Message    string           `json:"message"`
	Candidates []ImageCandidate `json:"candidates,omitempty"` // set when multiple candidates need user selection
	SavedPath  string           `json:"-"`                    // set by image fixers for post-Update provenance recording
	ImageType  string           `json:"-"`                    // image type for provenance recording (matches SavedPath)
}

// RunScope controls which artists "Run Rules" walks. Incremental (the
// default for the user-facing button) only re-evaluates artists that have
// mutated since their last evaluation, which makes the operation near-
// instant on a stable library. Full re-evaluates every non-excluded,
// non-locked artist regardless of dirty state -- exposed as the
// "Re-evaluate All" admin escape hatch (#698).
type RunScope int

const (
	// RunScopeIncremental processes only artists that the dirty tracker
	// flags as changed since their last rules_evaluated_at timestamp.
	RunScopeIncremental RunScope = iota

	// RunScopeAll processes every eligible artist, mirroring the legacy
	// behavior. Use sparingly: this is the multi-minute path on large
	// libraries.
	RunScopeAll
)

// String reports a stable label for the scope, suitable for logs and the
// run-status JSON payload.
func (s RunScope) String() string {
	switch s {
	case RunScopeAll:
		return "all"
	case RunScopeIncremental:
		return "incremental"
	default:
		return "incremental"
	}
}

// RunResult describes the outcome of running rules against multiple artists.
// ArtistsSkipped is the "unchanged" denominator exposed to incremental runs
// ("evaluating X of Y (Z unchanged)"); it is only populated for
// RunScopeIncremental and uses omitempty so scope=all responses do not
// mislabel failed evaluations as "skipped".
type RunResult struct {
	ArtistsProcessed int         `json:"artists_processed"`
	ArtistsTotal     int         `json:"artists_total"`
	ArtistsSkipped   int         `json:"artists_skipped,omitempty"`
	Scope            string      `json:"scope"`
	ViolationsFound  int         `json:"violations_found"`
	FixesAttempted   int         `json:"fixes_attempted"`
	FixesSucceeded   int         `json:"fixes_succeeded"`
	Results          []FixResult `json:"results"`
}

// PipelineRunner abstracts the rule pipeline so consumers can be tested
// with stubs instead of requiring a full Engine, Service, and Fixer chain.
type PipelineRunner interface {
	RunForArtist(ctx context.Context, a *artist.Artist) (*RunResult, error)
	RunImageRulesForArtist(ctx context.Context, a *artist.Artist) (*RunResult, error)
	RunRule(ctx context.Context, ruleID string) (*RunResult, error)
	RunAll(ctx context.Context) (*RunResult, error)
	// RunAllScoped is the dirty-aware variant of RunAll. Pass
	// RunScopeIncremental for the user-facing "Run Rules" button (only
	// evaluates artists with pending mutations) or RunScopeAll for the
	// "Re-evaluate All" admin path. RunAll is preserved for callers that
	// have not been updated and delegates to RunAllScoped(ctx, RunScopeIncremental)
	// to match the user-facing default.
	RunAllScoped(ctx context.Context, scope RunScope) (*RunResult, error)
	// RunRuleScoped is the dirty-aware variant of RunRule.
	RunRuleScoped(ctx context.Context, ruleID string, scope RunScope) (*RunResult, error)
	FixViolation(ctx context.Context, violationID string) (*FixResult, error)
	// SetArtistWorkers tunes how many artists a RunAll/RunRule pass evaluates
	// concurrently. Wired from the settings handler so the value is editable
	// at runtime; the next pass reads it via getArtistWorkers.
	SetArtistWorkers(n int)
	// ArtistWorkers reports the currently configured concurrency so the
	// settings UI can render the value actually in effect (which already
	// reflects SW_RULE_ENGINE_ARTIST_WORKERS applied at startup).
	ArtistWorkers() int
}

// Compile-time assertion: *Pipeline implements PipelineRunner.
var _ PipelineRunner = (*Pipeline)(nil)

// WriteGate answers whether rule auto-fixes that produce image or NFO disk
// writes are currently allowed. Implemented by internal/conflict.Gate via a
// trivial adapter in the API wiring layer so the rule package stays free of
// a dependency on the conflict package (avoids an import cycle).
//
// Allow* methods return nil when the write is permitted and a non-nil error
// (typically *conflict.BlockedError) when the conflict gate is engaged --
// either write-back or round-trip. The rule pipeline treats any non-nil
// return as "skip the fix and leave the violation open" -- the banner
// already tells the user why, so there is no need to surface the gate
// error any further.
type WriteGate interface {
	AllowImageWrite(ctx context.Context) error
	AllowNFOWrite(ctx context.Context) error
}

// Pipeline orchestrates rule evaluation and auto-fixing across artists.
type Pipeline struct {
	engine        *Engine
	artistService *artist.Service
	ruleService   *Service
	fixers        []Fixer
	publisher     *publish.Publisher
	logger        *slog.Logger

	// writeGateMu guards writeGate. SetWriteGate is documented as
	// idempotent and safe to call after construction ("replace"), so
	// reads on the hot fix path must lock even though the common case
	// is a single init-time write. Same pattern as ruleCacheMu below.
	writeGateMu sync.RWMutex
	writeGate   WriteGate

	// historyServiceMu guards historyService for the same reason as
	// writeGateMu: SetHistoryService can be called after construction so
	// the auto-fix history-recording read path on every successful fix
	// must lock against a concurrent setter. Issue #1106.
	historyServiceMu sync.RWMutex
	historyService   *artist.HistoryService

	// eventBusMu guards eventBus for the same reason as historyServiceMu:
	// SetEventBus can be called after construction, and the read happens on the
	// auto-fix hot path (recordRuleFixHistory) which runs concurrently across
	// artist workers.
	eventBusMu sync.RWMutex
	eventBus   *event.Bus

	ruleCacheMu sync.RWMutex
	ruleCache   map[string]*Rule

	// orchestratorMu guards orchestrator. SetOrchestrator is the
	// canonical wiring path (main.go installs it after construction),
	// kept symmetric with SetWriteGate / SetHistoryService so the
	// NewPipeline signature stays stable for the many test call sites.
	// Reads happen on the per-artist hot path that builds the
	// EvaluationContext (#1133), so the read lock is mandatory rather
	// than optional. Stored as the EvalProvider interface so tests can
	// exercise this exact wiring with a stub orchestrator without
	// needing the test to live in the same package as a concrete
	// *provider.Orchestrator constructor.
	orchestratorMu sync.RWMutex
	orchestrator   EvalProvider

	// artistWorkersMu guards artistWorkers. SetArtistWorkers is wired from
	// main.go after construction (mirrors SetOrchestrator/SetWriteGate so the
	// NewPipeline signature stays stable for the wide set of test call sites).
	// iterateArtistsByScope reads it once per pass to size its worker pool, so
	// a read lock guards against a concurrent setter. A value <= 1 selects the
	// original strictly-sequential walk; > 1 enables a bounded pool. Issue #1730.
	artistWorkersMu sync.RWMutex
	artistWorkers   int
}

// SetOrchestrator installs (or replaces) the EvalProvider the pipeline
// uses to construct a per-artist EvaluationContext (#1133). The
// EvaluationContext coalesces upstream fetches so multiple rules on the
// same artist share a single provider call per (artist, provider)
// combination per evaluation pass. The production caller in main.go
// passes a *provider.Orchestrator (which satisfies EvalProvider);
// integration tests can pass a counting stub here to exercise this
// exact wiring rather than a bypass seam. Passing nil disables
// coalescing -- the fixers fall through to their bare orchestrator
// references, which preserves the legacy uncoalesced behavior for tests
// that have not wired this setter.
func (p *Pipeline) SetOrchestrator(o EvalProvider) {
	p.orchestratorMu.Lock()
	p.orchestrator = o
	p.orchestratorMu.Unlock()
}

// withEvalContext returns ctx augmented with a fresh per-artist
// EvaluationContext when the pipeline has an orchestrator installed.
// Without an orchestrator we return ctx unchanged; the fixers see no
// EvaluationContext and fall back to their bare orchestrator references.
//
// The returned counters function reads the fetch/dedup totals at the end
// of the pass so the pipeline can warn-log them for the W4 (#1135)
// telemetry-gated decision. When no eval context is created the
// function returns zeros.
func (p *Pipeline) withEvalContext(ctx context.Context, a *artist.Artist) (context.Context, func() (uint64, uint64)) {
	p.orchestratorMu.RLock()
	prov := p.orchestrator
	p.orchestratorMu.RUnlock()

	if prov == nil {
		return ctx, func() (uint64, uint64) { return 0, 0 }
	}
	ec := NewEvaluationContext(a, prov, p.logger)
	return WithEvaluationContext(ctx, ec), ec.Counters
}

// SetWriteGate installs (or replaces) the conflict gate the pipeline
// consults before running auto-mode image/NFO fixers. Passing nil disables
// the gate -- callers that never configure the gate behave exactly as
// before. See WriteGate for semantics.
func (p *Pipeline) SetWriteGate(g WriteGate) {
	p.writeGateMu.Lock()
	p.writeGate = g
	p.writeGateMu.Unlock()
}

// getWriteGate returns the currently installed WriteGate under the
// writeGateMu read lock so attemptFix and any future consumer can read it
// safely without racing a concurrent SetWriteGate.
func (p *Pipeline) getWriteGate() WriteGate {
	p.writeGateMu.RLock()
	defer p.writeGateMu.RUnlock()
	return p.writeGate
}

// SetHistoryService installs (or replaces) the artist history service the
// pipeline uses to emit a Recent Activity entry on every successful auto-fix.
// Passing nil disables history recording -- callers that never configure a
// history service behave exactly as before. Issue #1106.
//
// Setter form (rather than a NewPipeline parameter) keeps the existing
// constructor signature stable for the wide set of test call sites.
func (p *Pipeline) SetHistoryService(h *artist.HistoryService) {
	p.historyServiceMu.Lock()
	p.historyService = h
	p.historyServiceMu.Unlock()
}

// getHistoryService returns the currently installed HistoryService under the
// historyServiceMu read lock so the auto-fix recorder can read it safely
// without racing a concurrent SetHistoryService.
func (p *Pipeline) getHistoryService() *artist.HistoryService {
	p.historyServiceMu.RLock()
	defer p.historyServiceMu.RUnlock()
	return p.historyService
}

// SetEventBus installs (or replaces) the event bus the pipeline publishes to on
// every successful auto-fix, so the next/ dashboard's live activity rail reflects
// rule fixes (single Fix and Run-rules) without a manual reload (#1804). Passing
// nil disables emission -- callers that never configure one behave exactly as
// before. Mirrors BulkExecutor.SetEventBus (the established event-bus injection
// in this package) and the Pipeline's own SetHistoryService setter form, which
// keeps the NewPipeline signature stable for existing test call sites.
func (p *Pipeline) SetEventBus(bus *event.Bus) {
	p.eventBusMu.Lock()
	p.eventBus = bus
	p.eventBusMu.Unlock()
}

// getEventBus returns the installed event bus under the read lock so
// recordRuleFixHistory can read it without racing a concurrent setter.
func (p *Pipeline) getEventBus() *event.Bus {
	p.eventBusMu.RLock()
	defer p.eventBusMu.RUnlock()
	return p.eventBus
}

// SetArtistWorkers configures how many artists RunAll/RunRule passes evaluate
// concurrently (issue #1730). main.go wires this from
// config.RuleEngine.ArtistWorkers after construction (default 2). A value
// <= 1 preserves the original strictly-sequential walk; a higher value caps
// the bounded worker pool that iterateArtistsByScope builds. The shared
// per-provider rate limiter still bounds total request throughput, so raising
// this only overlaps per-artist fetch latency.
//
// Setter form (rather than a NewPipeline parameter) keeps the constructor
// signature stable for the wide set of existing test call sites.
func (p *Pipeline) SetArtistWorkers(n int) {
	p.artistWorkersMu.Lock()
	p.artistWorkers = n
	p.artistWorkersMu.Unlock()
}

// ArtistWorkers returns the normalized worker count currently in effect (at
// least 1). It is the exported, interface-level accessor used by the settings
// UI; internal callers use getArtistWorkers directly.
func (p *Pipeline) ArtistWorkers() int {
	return p.getArtistWorkers()
}

// getArtistWorkers returns the configured worker count under the read lock,
// normalized so callers can treat the result as "at least 1". A stored value
// of 0 (never set) or any negative value collapses to 1, i.e. sequential.
func (p *Pipeline) getArtistWorkers() int {
	p.artistWorkersMu.RLock()
	n := p.artistWorkers
	p.artistWorkersMu.RUnlock()
	if n < 1 {
		return 1
	}
	return n
}

// recordRuleFixHistory emits a single Recent Activity entry for a successful
// auto-fix. The entry uses the canonical "rule:<rule_id>" source and a
// dedicated "rule_fix" pseudo-field name so the existing activity feed UI:
//
//   - Renders the source label as "Rule: <rule_id>" (history.source.rule_named).
//   - Hides the Revert button (artist.IsTrackableField returns false for
//     "rule_fix"), which is intentional: most rule auto-fixes mutate the
//     filesystem (NFO file, image file, directory rename, extraneous-file
//     delete) and cannot be safely undone via the field-revert path. The
//     dedicated FixViolation undo flow (W4.B) handles single-violation
//     reverts where supported.
//
// Errors are warn-logged and never propagated: the history entry is
// supplementary audit data and must not fail the actual fix.
//
// Issue #1106.
func (p *Pipeline) recordRuleFixHistory(ctx context.Context, artistID string, fr *FixResult) {
	if fr == nil || !fr.Fixed {
		return
	}
	// History entry (best-effort audit trail). Guarded INDEPENDENTLY of the live
	// push below so a missing history service does not also suppress the rail row.
	if h := p.getHistoryService(); h != nil {
		if err := h.Record(ctx, artistID, "rule_fix", "", fr.Message, "rule:"+fr.RuleID); err != nil {
			p.logger.Warn("recording rule auto-fix history",
				"rule_id", fr.RuleID, "artist_id", artistID, "error", err)
		}
	}
	// Push a live activity row so the next/ dashboard rail reflects the fix
	// without a manual reload (#1804). Best-effort and independent of the history
	// write above: a nil bus (the default) or a dropped event never affects the
	// fix. kind "set" matches how the server-rendered initial-load row classifies
	// this same rule_fix change (empty old -> non-empty new => "set", see
	// activityChangeKind), so a live row and its post-reload counterpart show the
	// same icon/label. event.NewActivityRecent builds the same activity.recent
	// envelope the manual field-edit handlers emit, so the rail-row contract has
	// one source.
	if bus := p.getEventBus(); bus != nil {
		bus.Publish(event.NewActivityRecent("set", fr.Message, artistID))
	}
}

// NewPipeline creates a new fix pipeline.
func NewPipeline(engine *Engine, artistService *artist.Service, ruleService *Service, fixers []Fixer, publisher *publish.Publisher, logger *slog.Logger) *Pipeline {
	return &Pipeline{
		engine:        engine,
		artistService: artistService,
		ruleService:   ruleService,
		fixers:        fixers,
		publisher:     publisher,
		logger:        logger.With(slog.String("component", "fix-pipeline")),
	}
}

// RunRule evaluates a single rule against all eligible artists and attempts
// fixes. Defaults to incremental scope -- only artists flagged as dirty are
// processed. Callers that need the legacy "every artist" behavior should
// use RunRuleScoped(ctx, ruleID, RunScopeAll).
func (p *Pipeline) RunRule(ctx context.Context, ruleID string) (*RunResult, error) {
	return p.RunRuleScoped(ctx, ruleID, RunScopeIncremental)
}

// RunRuleScoped evaluates a single rule against artists determined by scope.
// Returns ArtistsTotal/ArtistsSkipped on the result so the UI can report
// "evaluating 12 of 800 (788 unchanged)" for the incremental path.
func (p *Pipeline) RunRuleScoped(ctx context.Context, ruleID string, scope RunScope) (*RunResult, error) {
	result := &RunResult{Scope: scope.String()}

	// Fetch the rule once to check automation mode for all violations.
	targetRule, err := p.ruleService.GetByID(ctx, ruleID)
	if err != nil {
		return nil, fmt.Errorf("getting rule %s: %w", ruleID, err)
	}

	// Capture totals up front so progress reporting always shows the
	// denominator even when scope=incremental skips most artists.
	total, totalErr := p.artistService.CountEligibleArtists(ctx)
	if totalErr != nil {
		p.logger.Warn("counting eligible artists for run-rule progress", "error", totalErr)
	}
	result.ArtistsTotal = total

	processArtist := func(a *artist.Artist) (artistContribution, bool) {
		return p.processArtistForRunRule(ctx, a, ruleID, targetRule)
	}

	// Single-rule run does not cover every enabled rule, so leave
	// rules_evaluated_at untouched. Otherwise running rule A would mark
	// the artist clean and rule B's RunRule pass would silently skip it.
	processed, err := p.walkArtistsNoMark(ctx, scope, result, processArtist)
	if err != nil {
		return nil, err
	}
	result.ArtistsProcessed = processed
	// artists_skipped represents "unchanged" artists on an incremental run.
	// For scope=all the denominator equals the processed set (plus failures),
	// and reporting Total-Processed would mislabel failed evaluations as
	// skipped. Leave the field zero (omitempty hides it) in that case.
	if scope == RunScopeIncremental && result.ArtistsTotal > processed {
		result.ArtistsSkipped = result.ArtistsTotal - processed
	}

	return result, nil
}

// processArtistForRunRule is the per-artist work unit for RunRuleScoped. It
// mirrors processArtistForRunAll (#2137) but is pinned to a single rule: the
// dispatch loop skips every violation whose RuleID does not match, the
// automation-mode lookup uses the already-fetched targetRule instead of the
// per-pipeline rule cache, and the post-fix pass row is only written for
// ruleID (a single-rule sweep must not claim the artist passed rules it
// never evaluated this run). It reuses the same automation-mode strategies
// (processManualViolation / processAutoFixViolation), the same
// runForArtistAccum bookkeeping, and the same #983 deferred-resolved-rows
// ordering as the RunAllScoped path.
func (p *Pipeline) processArtistForRunRule(ctx context.Context, a *artist.Artist, ruleID string, targetRule *Rule) (artistContribution, bool) {
	var contrib artistContribution
	acc := &runForArtistAccum{persistOK: true}
	// startedAt captured pre-Evaluate so every rule_results row written
	// during this pass shares a timestamp (issue #699).
	startedAt := time.Now().UTC()

	// Issue #1133: per-artist EvaluationContext for fetch coalescing.
	// Shadows the outer ctx so every downstream call inside this method
	// inherits the coalescer without a rename.
	ctx, counters := p.withEvalContext(ctx, a)
	defer p.logEvalCounters(a, counters)

	// Issue #2476: evaluate ONLY the requested rule. Selecting a rule used to
	// scope which fixer acted while every enabled checker still ran, so asking
	// for a purely local rule (byte-identical image de-dupe) also fired the
	// provider-backed checkers and queried MusicBrainz once per artist.
	only := map[string]bool{ruleID: true}

	eval, err := p.engine.EvaluateScoped(ctx, a, only)
	if err != nil {
		p.logger.Warn("evaluating artist", "artist", a.Name, "rule_id", ruleID, "error", err)
		return contrib, false
	}

	// No RuleID filter here on purpose: the evaluation considered only ruleID,
	// so every violation it returned belongs to it. Re-filtering would mask a
	// regression in the scoping rather than catch it.
	for j := range eval.Violations {
		contrib.violationsFound++
		acc.mergeIntoContrib(p.dispatchViolation(ctx, a, &eval.Violations[j], targetRule), &contrib)
	}

	// Issue #699 propagation fix: derive the pass/fail skip-set from the
	// POST-fix evaluation, not the pre-fix snapshot. A rule the fixer just
	// repaired still appears in the pre-fix Violations slice, so using that
	// set would suppress its pass row for this run.
	//
	// This re-evaluation is scoped too. It used to be a full unscoped Evaluate
	// inside updateHealthScore, which is why a single-rule run produced two
	// back-to-back provider queries per artist rather than one (#2476).
	postEval, err := p.engine.EvaluateScoped(ctx, a, only)
	if err != nil {
		p.logger.Warn("re-evaluating artist after fixes",
			"artist", a.Name, "rule_id", ruleID, "error", err)
		postEval = nil
	}

	persistOKHealth := p.persistHealthAfterRun(ctx, a, postEval, acc.artistDirty)
	if !persistOKHealth {
		acc.persistOK = false
	}
	// Issue #983: only resolve violations once the artist row persisted
	// cleanly. A failed Update leaves the mutation in memory; marking the
	// violation resolved anyway would silently drop the fix.
	if persistOKHealth && !p.finalizeResolvedRows(ctx, a, acc.resolvedRows) {
		acc.persistOK = false
	}
	// Gate pass rows on the artist row having persisted: rule_results must not
	// lead the stored artist by claiming passed=1 from in-memory fix state that
	// never reached the DB (CR review-body 4144589645).
	if persistOKHealth && postEval != nil {
		postViolated := make(map[string]struct{}, len(postEval.Violations))
		for j := range postEval.Violations {
			postViolated[postEval.Violations[j].RuleID] = struct{}{}
		}
		// Single-rule scope: only persist the pass row for the specific
		// rule this invocation evaluated.
		passFilter := func(rid string) bool { return rid == ruleID }
		if !p.persistPassResults(ctx, a, postEval.RulesConsidered, postViolated, startedAt, passFilter) {
			acc.persistOK = false
		}
	}
	p.publishAccumulated(ctx, a, acc.metadataFixed, acc.fixedImageTypes)
	return contrib, acc.persistOK
}

// RunForArtist evaluates rules and attempts fixes for a single artist,
// respecting each rule's AutomationMode. All categories are considered.
func (p *Pipeline) RunForArtist(ctx context.Context, a *artist.Artist) (*RunResult, error) {
	return p.runForArtistFiltered(ctx, a, "")
}

// RunImageRulesForArtist is the fetch-images counterpart to RunForArtist:
// it runs only violations whose rule category is "image", so callers like
// the bulk-actions fetch_images path cannot accidentally mutate non-image
// metadata/NFO state via auto-mode fixers for other categories.
func (p *Pipeline) RunImageRulesForArtist(ctx context.Context, a *artist.Artist) (*RunResult, error) {
	return p.runForArtistFiltered(ctx, a, "image")
}

// violationOutcome is the per-violation delta produced by a strategy
// (processManualViolation / processAutoFixViolation). The orchestrator
// merges these into the accumulating per-artist state and the RunResult.
type violationOutcome struct {
	// fr is non-nil when the strategy invoked a fixer; the orchestrator
	// appends it to RunResult.Results and bumps FixesAttempted.
	fr *FixResult
	// resolvedRow is non-nil only on a successful auto-fix and carries the
	// violation row that will be marked Status=ViolationStatusResolved
	// AFTER updateHealthScore persists the mutated artist (#983).
	resolvedRow *RuleViolation
	// fixed mirrors fr.Fixed; lifted so the orchestrator does not need to
	// reach back into the FixResult struct to update FixesSucceeded.
	fixed bool
	// imageFix is true when a successful fix produced an image write, and
	// imageType carries the type so publishAccumulated can sync the right
	// canonical filenames. When false (and fixed is true), the fix touched
	// metadata and the orchestrator sets metadataFixed.
	imageFix  bool
	imageType string
	// persistFailed is true when any violation upsert or fixer-side write
	// failed; the orchestrator folds this into the artist-level flag that
	// gates rules_evaluated_at. The polarity is inverted (compared to the
	// runForArtistAccum.persistOK flag) so the zero value of a freshly
	// constructed violationOutcome means "no failure recorded" rather
	// than the dangerous "every write failed" default that would silently
	// disable rules_evaluated_at stamping for future strategy authors who
	// return a bare violationOutcome{} without setting the field.
	persistFailed bool
}

// runForArtistAccum is the in-flight per-artist state runForArtistFiltered
// builds up as it iterates violations. Threading it through mergeOutcome
// keeps the orchestrator loop body short enough to clear the gocognit
// gate (the load-bearing reason for splitting this out, not the named
// type itself).
type runForArtistAccum struct {
	metadataFixed   bool
	fixedImageTypes []string
	artistDirty     bool
	resolvedRows    []*RuleViolation
	persistOK       bool
}

// mergeOutcome folds one violation's delta into the accumulator and the
// run-level result. It owns the per-violation bookkeeping that previously
// inflated runForArtistFiltered's cognitive complexity past the gocognit
// gate.
func (acc *runForArtistAccum) mergeOutcome(out violationOutcome, result *RunResult) {
	if out.fr != nil {
		result.Results = append(result.Results, *out.fr)
		result.FixesAttempted++
	}
	if out.persistFailed {
		acc.persistOK = false
	}
	if out.fixed {
		result.FixesSucceeded++
		acc.artistDirty = true
		if out.imageFix {
			acc.fixedImageTypes = append(acc.fixedImageTypes, out.imageType)
		} else {
			acc.metadataFixed = true
		}
	}
	if out.resolvedRow != nil {
		acc.resolvedRows = append(acc.resolvedRows, out.resolvedRow)
	}
}

// mergeIntoContrib folds one violation's delta into acc and contrib for
// the RunAllScoped pass path, where per-artist results accumulate into an
// artistContribution (merged under a mutex by the walker) rather than
// directly into a *RunResult.
func (acc *runForArtistAccum) mergeIntoContrib(out violationOutcome, contrib *artistContribution) {
	if out.fr != nil {
		contrib.results = append(contrib.results, *out.fr)
		contrib.fixesAttempted++
	}
	if out.persistFailed {
		acc.persistOK = false
	}
	if out.fixed {
		contrib.fixesSucceeded++
		acc.artistDirty = true
		if out.imageFix {
			acc.fixedImageTypes = append(acc.fixedImageTypes, out.imageType)
		} else {
			acc.metadataFixed = true
		}
	}
	if out.resolvedRow != nil {
		acc.resolvedRows = append(acc.resolvedRows, out.resolvedRow)
	}
}

// runForArtistFiltered is the shared body of RunForArtist and
// RunImageRulesForArtist. An empty categoryFilter runs every violation; a
// non-empty value runs only violations whose Category matches exactly.
//
// The function is a strategy dispatcher: for each violation it looks up the
// rule's AutomationMode, hands off to processManualViolation or
// processAutoFixViolation, then merges the returned delta into the
// per-artist accumulator. The deferred-resolved-rows ordering required by
// #983 lives entirely inside this orchestrator: processAutoFixViolation
// hands back the row to defer, and the orchestrator only stamps it
// Resolved after updateHealthScore persists the artist.
func (p *Pipeline) runForArtistFiltered(ctx context.Context, a *artist.Artist, categoryFilter string) (*RunResult, error) {
	result := &RunResult{}

	if a.IsExcluded || a.Locked {
		return result, nil
	}

	result.ArtistsProcessed = 1
	// Capture before evaluation for the same race-protection reason as
	// the multi-artist walker: a concurrent dirty mark must remain
	// strictly greater than rules_evaluated_at.
	startedAt := time.Now().UTC()

	// Issue #1133 (M54 W2A): wrap ctx with a per-artist EvaluationContext
	// so multiple fixers needing the same provider payload share one
	// upstream call. The context lifetime is the artist's evaluation
	// pass; cleanup is via the defer'd telemetry log, which also serves
	// as the W4 (#1135) gating signal.
	evalCtx, counters := p.withEvalContext(ctx, a)
	defer p.logEvalCounters(a, counters)

	// Issue #2476: scope the evaluation to the requested category. Like the
	// single-rule path, this used to evaluate EVERY enabled rule and then
	// discard the violations outside the category, so RunImageRulesForArtist
	// ("fetch images for this artist") also ran the provider-backed checkers and
	// queried MusicBrainz. A nil scope (empty categoryFilter) means the
	// whole-artist run, which legitimately evaluates everything.
	scope, err := p.engine.ScopeForCategory(evalCtx, a, categoryFilter)
	if err != nil {
		return nil, fmt.Errorf("resolving rule scope for artist %s: %w", a.Name, err)
	}

	eval, err := p.engine.EvaluateScoped(evalCtx, a, scope)
	if err != nil {
		return nil, fmt.Errorf("evaluating artist %s: %w", a.Name, err)
	}

	acc := &runForArtistAccum{persistOK: true}
	// Cache rule lookups so the per-violation dispatch and the
	// post-filter pass-row writer share a single set of DB reads.
	ruleCache := map[string]*Rule{}

	p.dispatchViolations(evalCtx, a, eval.Violations, categoryFilter, ruleCache, acc, result)
	p.finalizeArtistRun(evalCtx, a, ruleCache, acc, categoryFilter, scope, startedAt)
	return result, nil
}

// logEvalCounters emits the per-artist provider_fetch_total /
// provider_fetch_dedup_total signals at the end of an evaluation pass.
// These are the gating signals for the W4 (#1135) telemetry-gated
// decision: if a substantial fraction of fetches dedup, the coalescing
// layer earns its keep; if not, the milestone closes #1135 without
// implementing batch endpoints.
//
// We log at debug to avoid bloating production logs; a future metrics
// scrape can pick up the structured keys directly.
func (p *Pipeline) logEvalCounters(a *artist.Artist, counters func() (uint64, uint64)) {
	if counters == nil {
		return
	}
	fetches, dedups := counters()
	if fetches == 0 && dedups == 0 {
		// No provider calls touched the eval ctx; nothing to report.
		return
	}
	p.logger.Debug("evaluation context provider counters",
		slog.String("artist_id", a.ID),
		slog.String("artist", a.Name),
		slog.Uint64("provider_fetch_total", fetches),
		slog.Uint64("provider_fetch_dedup_total", dedups),
	)
}

// logPassCounters emits the pass-level PassContext counter summary at the
// end of a RunAllScoped invocation. This captures the Phase 2 (#1134)
// signal: provider_cache_hit_total measures how many provider calls were
// served from the pass-scoped LRU rather than from the network or even
// the per-artist EvaluationContext. Eviction and invalidation counts are
// informational for diagnosing cache-size tuning and rule-fix side-effect
// patterns.
//
// We log at Info (not Debug) because this is a pass-level summary that
// fire once per Run-All invocation, not once per artist, and is the
// primary telemetry signal for the W4 (#1135) decision point.
func (p *Pipeline) logPassCounters(pc *PassContext) {
	if pc == nil {
		return
	}
	hits, evictions, invalidations := pc.Counters()
	if hits == 0 && evictions == 0 && invalidations == 0 {
		// Nothing to report -- pass had no pass-cache activity.
		return
	}
	p.logger.Info("pass context provider-fetch cache summary",
		slog.Uint64("provider_cache_hit_total", hits),
		slog.Uint64("pass_cache_eviction_total", evictions),
		slog.Uint64("pass_cache_invalidation_total", invalidations),
	)
}

// dispatchViolations is the strategy-dispatch loop pulled out of
// runForArtistFiltered. It walks the violation list (skipping any that do
// not match categoryFilter), looks up each rule, hands off to the
// automation-mode strategy, and merges the outcome into acc and result.
func (p *Pipeline) dispatchViolations(ctx context.Context, a *artist.Artist, violations []Violation, categoryFilter string, ruleCache map[string]*Rule, acc *runForArtistAccum, result *RunResult) {
	for j := range violations {
		v := &violations[j]
		if categoryFilter != "" && v.Category != categoryFilter {
			continue
		}
		result.ViolationsFound++

		r, lookupOK := p.lookupRule(ctx, a, v.RuleID, ruleCache)
		if !lookupOK {
			acc.persistOK = false
			continue
		}
		acc.mergeOutcome(p.dispatchViolation(ctx, a, v, r), result)
	}
}

// finalizeArtistRun owns the post-loop persistence chain:
//
//   - updateHealthScore re-evaluates the artist and persists the row.
//     Required FIRST because the deferred-resolved-rows logic (#983)
//     can only fire once we know the artist row reached the DB.
//   - finalizeResolvedRows stamps the deferred rows with Resolved status
//     ONLY when updateHealthScore reported persistOKHealth.
//   - writeFilteredPassResults writes the per-rule pass rows, honoring
//     categoryFilter so RunImageRulesForArtist does not claim the artist
//     "passes" metadata rules it never actually ran.
//   - publishAccumulated emits SSE for the platform sync.
//   - markArtistEvaluated stamps rules_evaluated_at only when every
//     persistence step succeeded AND the run covered every rule (i.e.
//     categoryFilter was empty).
func (p *Pipeline) finalizeArtistRun(ctx context.Context, a *artist.Artist, ruleCache map[string]*Rule, acc *runForArtistAccum, categoryFilter string, scope map[string]bool, startedAt time.Time) {
	// Re-evaluate at the SAME scope the run used. A whole-artist run (nil scope)
	// evaluates everything and its HealthScore is authoritative; a category run
	// evaluates only that category and its health is derived offline, so neither
	// path re-runs a checker the operator did not ask for (#2476).
	postEval, err := p.engine.EvaluateScoped(ctx, a, scope)
	if err != nil {
		p.logger.Warn("re-evaluating health score", "artist", a.Name, "error", err)
		postEval = nil
	}

	persistOKHealth := p.persistHealthAfterRun(ctx, a, postEval, acc.artistDirty)
	if !persistOKHealth {
		acc.persistOK = false
	}
	// Issue #983: only resolve violations once the artist row persist
	// succeeded. A failed Update leaves the mutation in memory; marking
	// the violation resolved would silently drop the fix.
	if persistOKHealth && !p.finalizeResolvedRows(ctx, a, acc.resolvedRows) {
		acc.persistOK = false
	}
	// Gate pass rows on the artist row having persisted. Previously this was
	// implicit -- updateHealthScore returned a nil eval on a persist failure, so
	// the postEval != nil check below doubled as the persist gate. The scoped
	// rework separates the evaluation from the write, so the gate has to be
	// stated: rule_results must never claim passed=1 from in-memory fix state
	// that failed to reach the artist row.
	if persistOKHealth && postEval != nil &&
		!p.writeFilteredPassResults(ctx, a, postEval, ruleCache, categoryFilter, startedAt) {
		acc.persistOK = false
	}
	p.publishAccumulated(ctx, a, acc.metadataFixed, acc.fixedImageTypes)
	if categoryFilter == "" && acc.persistOK {
		p.markArtistEvaluated(ctx, a, startedAt)
	}
}

// lookupRule returns the cached *Rule for ruleID, populating ruleCache on
// a miss. Returns ok=false (and warn-logs) on a GetByID failure so the
// caller can drop the violation from this pass and fold the failure into
// persistOK.
func (p *Pipeline) lookupRule(ctx context.Context, a *artist.Artist, ruleID string, ruleCache map[string]*Rule) (*Rule, bool) {
	if r, ok := ruleCache[ruleID]; ok {
		return r, true
	}
	r, err := p.ruleService.GetByID(ctx, ruleID)
	if err != nil {
		p.logger.Warn("fetching rule for violation", "rule_id", ruleID, "artist", a.Name, "error", err)
		return nil, false
	}
	ruleCache[ruleID] = r
	return r, true
}

// dispatchViolation routes a violation to the strategy keyed by the
// rule's AutomationMode. Pulling the dispatch out of the loop keeps the
// orchestrator under the gocognit gate at threshold 30.
func (p *Pipeline) dispatchViolation(ctx context.Context, a *artist.Artist, v *Violation, r *Rule) violationOutcome {
	if r.AutomationMode == AutomationModeManual {
		return p.processManualViolation(ctx, a, v)
	}
	return p.processAutoFixViolation(ctx, a, v)
}

// processManualViolation is the manual-automation strategy: discover
// candidates without applying them, then persist a violation row whose
// Status reflects whether candidates were found. Returns the delta that
// runForArtistFiltered merges into its per-artist accumulator.
//
// Manual mode never invokes side-effect fixers (LogoPaddingFixer,
// NFOFixer, ExtraneousImagesFixer); when no fixer implements
// CandidateDiscoverer the row is persisted as open with Fixable
// reflecting only the canonical-fixer presence.
func (p *Pipeline) processManualViolation(ctx context.Context, a *artist.Artist, v *Violation) violationOutcome {
	fixer := p.findFixer(v)
	if !v.Fixable || fixer == nil || !supportsCandidateDiscovery(fixer) {
		ok := p.persistViolation(ctx, a, v, v.Fixable && fixer != nil, ViolationStatusOpen, nil, "manual-mode violation")
		return violationOutcome{persistFailed: !ok}
	}

	v.Config.DiscoveryOnly = true
	fr := p.attemptFix(ctx, a, v)

	status := ViolationStatusOpen
	if len(fr.Candidates) > 0 {
		status = ViolationStatusPendingChoice
	}
	ok := p.persistViolation(ctx, a, v, true, status, fr.Candidates, "manual-mode violation")
	return violationOutcome{fr: fr, persistFailed: !ok}
}

// processAutoFixViolation is the auto-automation strategy: persist
// unfixable violations as open, attempt fixes on fixable ones, defer
// resolved-status upserts per #983, and emit Recent Activity history per
// #1106. Returns the delta that runForArtistFiltered merges into its
// per-artist accumulator -- crucially, a non-nil resolvedRow when a fix
// succeeded so the orchestrator can stamp Resolved only AFTER
// updateHealthScore persists the artist (the load-bearing #983 ordering).
func (p *Pipeline) processAutoFixViolation(ctx context.Context, a *artist.Artist, v *Violation) violationOutcome {
	if !v.Fixable {
		ok := p.persistViolation(ctx, a, v, false, ViolationStatusOpen, nil, "unfixable violation")
		return violationOutcome{persistFailed: !ok}
	}

	fr := p.attemptFix(ctx, a, v)
	out := violationOutcome{fr: fr}
	if fr.Fixed {
		out.fixed = true
		if fr.ImageType != "" {
			out.imageFix = true
			out.imageType = fr.ImageType
		}
		// Issue #1106: emit a Recent Activity entry. recordRuleFixHistory
		// warn-logs on failure and never fails the surrounding fix flow.
		p.recordRuleFixHistory(ctx, a.ID, fr)
		// Issue #983: stash the row but do not write Resolved yet -- the
		// orchestrator only stamps Resolved after updateHealthScore
		// persists the mutated artist.
		out.resolvedRow = &RuleViolation{
			RuleID:     v.RuleID,
			ArtistID:   a.ID,
			ArtistName: a.Name,
			Severity:   v.Severity,
			Message:    v.Message,
			Fixable:    true,
			Candidates: fr.Candidates,
		}
		return out
	}

	status := ViolationStatusOpen
	if len(fr.Candidates) > 0 {
		status = ViolationStatusPendingChoice
	}
	if !p.persistViolation(ctx, a, v, true, status, fr.Candidates, "fix result violation") {
		out.persistFailed = true
	}
	return out
}

// persistViolation is the shared upsert used by both automation modes.
// Returns false (and warn-logs) on DB failure so the caller can fold the
// failure into its persistOK flag.
func (p *Pipeline) persistViolation(ctx context.Context, a *artist.Artist, v *Violation, fixable bool, status string, candidates []ImageCandidate, logCtx string) bool {
	rv := &RuleViolation{
		RuleID:     v.RuleID,
		ArtistID:   a.ID,
		ArtistName: a.Name,
		Severity:   v.Severity,
		Message:    v.Message,
		Fixable:    fixable,
		Status:     status,
		Candidates: candidates,
	}
	if err := p.ruleService.UpsertViolation(ctx, rv); err != nil {
		p.logger.Warn("persisting "+logCtx, "rule_id", v.RuleID, "artist", a.Name, "error", err)
		return false
	}
	return true
}

// finalizeResolvedRows stamps every deferred row with
// Status=ViolationStatusResolved and a fresh ResolvedAt, then upserts. The
// caller invokes this only AFTER updateHealthScore has persisted the
// artist (#983 ordering). Returns true when every upsert succeeded.
func (p *Pipeline) finalizeResolvedRows(ctx context.Context, a *artist.Artist, resolvedRows []*RuleViolation) bool {
	ok := true
	now := time.Now().UTC()
	for _, rv := range resolvedRows {
		rv.Status = ViolationStatusResolved
		rv.ResolvedAt = &now
		if err := p.ruleService.UpsertViolation(ctx, rv); err != nil {
			p.logger.Warn("persisting resolved violation", "rule_id", rv.RuleID, "artist", a.Name, "error", err)
			ok = false
		}
	}
	return ok
}

// writeFilteredPassResults writes the post-fix pass rows for the artist,
// honoring categoryFilter so RunImageRulesForArtist does not claim the
// artist "passes" metadata rules it never actually ran. ruleCache is the
// per-artist cache the orchestrator built during dispatch; this function
// extends it with any rules considered post-fix that the loop did not
// visit. Returns false on any persistence failure (rule fetch, pass
// upsert) so the caller can fold the failure into persistOK.
func (p *Pipeline) writeFilteredPassResults(ctx context.Context, a *artist.Artist, postEval *EvaluationResult, ruleCache map[string]*Rule, categoryFilter string, startedAt time.Time) bool {
	postViolated := make(map[string]struct{}, len(postEval.Violations))
	for j := range postEval.Violations {
		postViolated[postEval.Violations[j].RuleID] = struct{}{}
	}

	// When categoryFilter is set we only mirror the category into
	// rule_results so RunImageRulesForArtist does not claim the artist
	// "passes" metadata rules it never actually ran. Precomputing the
	// allowed-ID set (vs evaluating the category per passFilter call)
	// lets us treat a GetByID failure as a persistence failure instead
	// of silently dropping the rule from the pass set, which would
	// leave the artist clean but without a pass row (CR #3114616841).
	var passFilter func(rid string) bool
	if categoryFilter != "" {
		allowedIDs, ok := p.allowedRulesForCategory(ctx, a, postEval.RulesConsidered, ruleCache, categoryFilter)
		if !ok {
			return false
		}
		passFilter = func(rid string) bool {
			_, present := allowedIDs[rid]
			return present
		}
	}
	return p.persistPassResults(ctx, a, postEval.RulesConsidered, postViolated, startedAt, passFilter)
}

// allowedRulesForCategory builds the set of rule IDs from
// rulesConsidered whose Category matches categoryFilter, extending
// ruleCache with any fresh fetches. Returns ok=false on a GetByID
// failure so writeFilteredPassResults treats it as a persistence
// failure (CR #3114616841) instead of silently dropping the rule from
// the pass set.
func (p *Pipeline) allowedRulesForCategory(ctx context.Context, a *artist.Artist, rulesConsidered []string, ruleCache map[string]*Rule, categoryFilter string) (map[string]struct{}, bool) {
	allowedIDs := make(map[string]struct{}, len(rulesConsidered))
	for _, rid := range rulesConsidered {
		r, ok := ruleCache[rid]
		if !ok {
			fetched, err := p.ruleService.GetByID(ctx, rid)
			if err != nil {
				p.logger.Warn("fetching rule for pass filter",
					"rule_id", rid, "artist", a.Name, "error", err)
				return nil, false
			}
			ruleCache[rid] = fetched
			r = fetched
		}
		if string(r.Category) == categoryFilter {
			allowedIDs[rid] = struct{}{}
		}
	}
	return allowedIDs, true
}

// processArtistForRunAll is the per-artist work unit for RunAllScoped.
// It evaluates the artist, dispatches every violation through the
// automation-mode strategies (reusing processManualViolation /
// processAutoFixViolation, the same helpers runForArtistFiltered uses),
// then finalizes health (#699), deferred resolved rows (#983), pass rows,
// and platform publishing. The returned artistContribution is merged into
// the shared RunResult by walkArtistsWithMark under a mutex, keeping this
// hot path lock-free. rules_evaluated_at is NOT stamped here;
// walkArtistsWithMark owns that step so the timestamp aligns with the
// per-iteration startedAt captured by iterateArtistsByScope.
func (p *Pipeline) processArtistForRunAll(ctx context.Context, a *artist.Artist) (artistContribution, bool) {
	var contrib artistContribution
	acc := &runForArtistAccum{persistOK: true}
	// startedAt captured pre-Evaluate so every rule_results row written
	// during this pass shares a timestamp (issue #699).
	startedAt := time.Now().UTC()

	// Issue #1133: per-artist EvaluationContext for fetch coalescing.
	// Shadows the outer ctx so every downstream call inside this method
	// inherits the coalescer without a rename.
	ctx, counters := p.withEvalContext(ctx, a)
	defer p.logEvalCounters(a, counters)

	eval, err := p.engine.Evaluate(ctx, a)
	if err != nil {
		p.logger.Warn("evaluating artist", "artist", a.Name, "error", err)
		return contrib, false
	}

	for j := range eval.Violations {
		v := &eval.Violations[j]
		contrib.violationsFound++
		// getCachedRule is pipeline-level and RWMutex-guarded, safe for
		// concurrent artist workers (issue #1730).
		r, lookupErr := p.getCachedRule(ctx, v.RuleID)
		if lookupErr != nil {
			p.logger.Warn("fetching rule for violation", "rule_id", v.RuleID, "artist", a.Name, "error", lookupErr)
			acc.persistOK = false
			continue
		}
		acc.mergeIntoContrib(p.dispatchViolation(ctx, a, v, r), &contrib)
	}

	// Issue #699: derive pass/fail from the POST-fix evaluation so rules
	// repaired during this pass are recorded as passed=1 in the same run.
	postEval, persistOKHealth := p.updateHealthScore(ctx, a, acc.artistDirty)
	if !persistOKHealth {
		acc.persistOK = false
	}
	// Issue #983: only resolve violations once the artist row persisted
	// cleanly. A failed Update leaves the mutation in memory; marking the
	// violation resolved anyway would silently drop the fix.
	if persistOKHealth && !p.finalizeResolvedRows(ctx, a, acc.resolvedRows) {
		acc.persistOK = false
	}
	if postEval != nil {
		postViolated := make(map[string]struct{}, len(postEval.Violations))
		for j := range postEval.Violations {
			postViolated[postEval.Violations[j].RuleID] = struct{}{}
		}
		if !p.persistPassResults(ctx, a, postEval.RulesConsidered, postViolated, startedAt, nil) {
			acc.persistOK = false
		}
	}
	p.publishAccumulated(ctx, a, acc.metadataFixed, acc.fixedImageTypes)
	return contrib, acc.persistOK
}

// RunAll evaluates all enabled rules against eligible artists and attempts
// fixes. Defaults to incremental scope -- only artists flagged as dirty are
// processed. Use RunAllScoped(ctx, RunScopeAll) for a forced full sweep.
func (p *Pipeline) RunAll(ctx context.Context) (*RunResult, error) {
	return p.RunAllScoped(ctx, RunScopeIncremental)
}

// RunAllScoped evaluates every enabled rule against the artists determined
// by scope (incremental or all). The result reports both the number of
// artists processed and the total eligible count so progress UIs can show
// "evaluating 12 of 800 (788 unchanged)".
func (p *Pipeline) RunAllScoped(ctx context.Context, scope RunScope) (*RunResult, error) {
	result := &RunResult{Scope: scope.String()}
	// passStart drives the pass_wall_clock_ms telemetry emitted at the end so
	// the artist-worker setting (#1730) can be evaluated against real
	// end-to-end pass duration.
	passStart := time.Now()

	total, totalErr := p.artistService.CountEligibleArtists(ctx)
	if totalErr != nil {
		p.logger.Warn("counting eligible artists for run-all progress", "error", totalErr)
	}
	result.ArtistsTotal = total

	// Issue #1134 (M54 W3): pass-scoped provider-fetch cache. Construct a
	// PassContext for the lifetime of this RunAllScoped invocation and
	// plumb it onto the context so every per-artist EvaluationContext built
	// inside processArtistForRunAll finds it via passContextFromContext. When
	// an artist is re-evaluated later in the same pass (e.g. dirtied by a
	// prior fix), its new EvaluationContext will find the cached provider
	// payload in the PassContext instead of issuing a fresh network call.
	// The PassContext falls out of scope when this function returns; there
	// is no cross-pass sharing.
	//
	// The nil check on p.orchestrator ensures we only construct a
	// PassContext when the pipeline is actually wired for coalescing.
	// Without an orchestrator the EvaluationContext path is skipped
	// entirely, so building a PassContext would be dead weight.
	p.orchestratorMu.RLock()
	hasOrch := p.orchestrator != nil
	p.orchestratorMu.RUnlock()
	if hasOrch {
		passCtx := NewPassContext(DefaultPassCacheSize, p.logger)
		ctx = WithPassContext(ctx, passCtx)
		defer p.logPassCounters(passCtx)
	}

	processArtist := func(a *artist.Artist) (artistContribution, bool) {
		return p.processArtistForRunAll(ctx, a)
	}

	// RunAll covers every enabled rule, so it owns rules_evaluated_at:
	// after this pass the artist is fully up-to-date and falls out of
	// the dirty set until the next mutation.
	processed, err := p.walkArtistsWithMark(ctx, scope, result, processArtist)
	if err != nil {
		return nil, err
	}
	result.ArtistsProcessed = processed
	// See RunRuleScoped for why artists_skipped is only computed for
	// scope=incremental.
	if scope == RunScopeIncremental && result.ArtistsTotal > processed {
		result.ArtistsSkipped = result.ArtistsTotal - processed
	}

	p.logRunSummary(scope, result, time.Since(passStart))
	return result, nil
}

// logRunSummary emits the pass-level execution summary at Info once per
// RunAllScoped invocation. Alongside the violation/fix counters it reports the
// configured artist-worker count and the wall-clock duration so the #1730
// parallelism setting can be evaluated from logs without a profiler. Per-artist
// counters stay at Debug (see logEvalCounters); this is the single pass-level
// line operators watch when tuning SW_RULE_ENGINE_ARTIST_WORKERS.
func (p *Pipeline) logRunSummary(scope RunScope, result *RunResult, wallClock time.Duration) {
	p.logger.Info("rule pass summary",
		slog.String("scope", scope.String()),
		slog.Int("artist_workers", p.getArtistWorkers()),
		slog.Int("artists_processed", result.ArtistsProcessed),
		slog.Int("artists_total", result.ArtistsTotal),
		slog.Int("violations_found", result.ViolationsFound),
		slog.Int("fixes_attempted", result.FixesAttempted),
		slog.Int("fixes_succeeded", result.FixesSucceeded),
		slog.Int64("pass_wall_clock_ms", wallClock.Milliseconds()),
	)
}

// artistContribution holds the per-artist counters and fix results a single
// processArtist invocation produces. Returning it (rather than mutating the
// shared run-level *RunResult) is what lets the artist walkers evaluate
// artists concurrently: the hot per-artist path touches only this local
// struct, and the walker folds each contribution into the run result under a
// single mutex. Issue #1730.
type artistContribution struct {
	violationsFound int
	fixesAttempted  int
	fixesSucceeded  int
	results         []FixResult
}

// dispatchArtist sends a single artist to fn either directly (sequential) or
// via the errgroup pool (parallel). When g is nil the call is synchronous.
// fn must be goroutine-safe when g != nil.
func dispatchArtist(g *errgroup.Group, a *artist.Artist, startedAt time.Time, fn func(*artist.Artist, time.Time)) {
	if g == nil {
		fn(a, startedAt)
		return
	}
	g.Go(func() error {
		fn(a, startedAt)
		return nil
	})
}

// iterateArtistsByScope enumerates artists matching scope and invokes fn for
// each eligible (non-excluded, non-locked) artist. Concurrency is governed by
// workers: a value of 1 produces a strictly sequential walk; a higher value
// dispatches artists to a bounded errgroup pool whose SetLimit caps concurrent
// goroutines so the scope=all page advance is also throttled, keeping memory
// usage bounded on large libraries.
//
// fn is the per-artist work unit. It must be safe to call concurrently when
// workers > 1. The processed count is the caller's responsibility via fn's
// side effects.
//
// Returns any enumeration error (ListDirtyIDs or List failure). Context
// cancellation is propagated as ctx.Err() so callers can distinguish a partial
// run from a clean completion.
func (p *Pipeline) iterateArtistsByScope(ctx context.Context, scope RunScope, workers int, fn func(a *artist.Artist, startedAt time.Time)) error {
	// With more than one worker, dispatch through an errgroup whose SetLimit
	// caps concurrency. fn never returns an error (failures are warn-logged
	// and surface via the ok bool inside fn), so g.Wait cannot report an error.
	var g *errgroup.Group
	if workers > 1 {
		g = &errgroup.Group{}
		g.SetLimit(workers)
	}
	if scope == RunScopeIncremental {
		return p.iterateIncremental(ctx, g, fn)
	}
	return p.iterateAll(ctx, g, fn)
}

// iterateIncremental implements the scope=incremental artist walk. It queries
// the dirty-ID list in a single SQL call (the dirty filter index keeps this
// fast even when zero artists are dirty), then loads and dispatches each
// non-excluded, non-locked artist. The row state is re-checked on load because
// it may have changed between the ListDirtyIDs query and the GetByID fetch.
func (p *Pipeline) iterateIncremental(ctx context.Context, g *errgroup.Group, fn func(*artist.Artist, time.Time)) error {
	ids, err := p.artistService.ListDirtyIDs(ctx)
	if err != nil {
		return fmt.Errorf("listing dirty artists: %w", err)
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		a, err := p.artistService.GetByID(ctx, id)
		if err != nil {
			p.logger.Warn("loading dirty artist", "artist_id", id, "error", err)
			continue
		}
		if a.IsExcluded || a.Locked {
			continue
		}
		dispatchArtist(g, a, time.Now().UTC(), fn)
	}
	// Drain in-flight workers before the caller reads its counters.
	if g != nil {
		_ = g.Wait()
	}
	return ctx.Err()
}

// iterateAll implements the scope=all artist walk. It uses a paginated List so
// memory usage stays bounded on large libraries. Each &page[i] is a stable
// pointer into that page's backing array, so reassigning page on the next
// iteration does not disturb workers still processing a previous page.
func (p *Pipeline) iterateAll(ctx context.Context, g *errgroup.Group, fn func(*artist.Artist, time.Time)) error {
	const pageSize = 200
	params := artist.ListParams{Page: 1, PageSize: pageSize, Sort: "name"}
	for ctx.Err() == nil {
		page, _, err := p.artistService.List(ctx, params)
		if err != nil {
			if g != nil {
				_ = g.Wait()
			}
			return fmt.Errorf("listing artists: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for i := range page {
			if ctx.Err() != nil {
				break
			}
			a := &page[i]
			if a.IsExcluded || a.Locked {
				continue
			}
			dispatchArtist(g, a, time.Now().UTC(), fn)
		}
		if len(page) < pageSize {
			break
		}
		params.Page++
	}
	// Drain in-flight workers, then propagate ctx.Err() so callers can
	// distinguish a partial run from a clean completion.
	if g != nil {
		_ = g.Wait()
	}
	return ctx.Err()
}

// mergeContribution folds contrib into result under mu and increments
// processed when ok is true. Extracted to keep the two walker closures
// below identical in their merge logic.
func mergeContribution(mu *sync.Mutex, processed *int, result *RunResult, contrib artistContribution, ok bool) {
	mu.Lock()
	result.ViolationsFound += contrib.violationsFound
	result.FixesAttempted += contrib.fixesAttempted
	result.FixesSucceeded += contrib.fixesSucceeded
	result.Results = append(result.Results, contrib.results...)
	// Only count artists that actually completed evaluation. A false ok
	// means fn bailed (engine error) and intentionally left the artist dirty
	// for retry; counting it would over-report the "evaluated X of Y
	// (Z unchanged)" summary and stamping rules_evaluated_at would hide the
	// next pass.
	if ok {
		*processed++
	}
	mu.Unlock()
}

// walkArtistsNoMark invokes fn for every artist matching scope and folds
// each contribution into result, but does NOT stamp rules_evaluated_at.
// Used by RunRuleScoped: a single-rule sweep does not cover every enabled
// rule and must not claim the artist is fully up-to-date, otherwise a
// subsequent RunRule pass for a different rule would silently skip it.
//
// processed counts artists fn was invoked on (successes and failures both
// consumed pipeline work). See iterateArtistsByScope for enumeration and
// concurrency semantics.
func (p *Pipeline) walkArtistsNoMark(ctx context.Context, scope RunScope, result *RunResult, fn func(a *artist.Artist) (artistContribution, bool)) (int, error) {
	workers := p.getArtistWorkers()
	var mu sync.Mutex
	processed := 0

	runOne := func(a *artist.Artist, startedAt time.Time) {
		contrib, ok := fn(a)
		mergeContribution(&mu, &processed, result, contrib, ok)
		// No rules_evaluated_at stamp: this walker intentionally omits it.
	}

	if err := p.iterateArtistsByScope(ctx, scope, workers, runOne); err != nil {
		return processed, err
	}
	return processed, nil
}

// walkArtistsWithMark invokes fn for every artist matching scope, folds each
// contribution into result, and stamps rules_evaluated_at after each
// successful evaluation. Used by RunAllScoped: it covers every enabled rule
// so after this pass the artist is fully up-to-date and falls out of the
// dirty set until the next mutation.
//
// rules_evaluated_at is stamped with the per-iteration start time captured
// before fn runs. This protects against a race where an ArtistUpdated event
// arrives mid-process: the async DirtySubscriber stamps dirty_since with a
// "now" timestamp that must remain strictly greater than rules_evaluated_at,
// so the artist stays in the dirty set on the next pass and the in-flight
// mutation is not silently dropped.
//
// The stamp runs outside the result mutex so workers do not serialize on the
// DB write. markArtistEvaluated is safe to call concurrently (distinct artist
// IDs, idempotent, warn-logs on error).
//
// processed counts artists fn was invoked on (successes and failures both
// consumed pipeline work). See iterateArtistsByScope for enumeration and
// concurrency semantics.
func (p *Pipeline) walkArtistsWithMark(ctx context.Context, scope RunScope, result *RunResult, fn func(a *artist.Artist) (artistContribution, bool)) (int, error) {
	workers := p.getArtistWorkers()
	var mu sync.Mutex
	processed := 0

	runOne := func(a *artist.Artist, startedAt time.Time) {
		contrib, ok := fn(a)
		mergeContribution(&mu, &processed, result, contrib, ok)
		if ok {
			p.markArtistEvaluated(ctx, a, startedAt)
		}
	}

	if err := p.iterateArtistsByScope(ctx, scope, workers, runOne); err != nil {
		return processed, err
	}
	return processed, nil
}

// markArtistEvaluated stamps rules_evaluated_at on the artist after a
// successful pass through the pipeline. Pass the per-iteration start time
// so a concurrent dirty mark (event-driven) stays > rules_evaluated_at and
// is preserved for the next pass. Errors are logged but never propagated:
// the artist will simply remain in the dirty set and be re-evaluated next
// time, which is the safe failure mode.
func (p *Pipeline) markArtistEvaluated(ctx context.Context, a *artist.Artist, startedAt time.Time) {
	if err := p.artistService.MarkRulesEvaluated(ctx, a.ID, startedAt); err != nil {
		p.logger.Warn("marking artist rules-evaluated",
			"artist_id", a.ID,
			"artist", a.Name,
			"error", err)
	}
}

// FixViolation applies the recommended fix for a single persisted violation.
// For pending_choice violations with image candidates, it returns a non-fixed
// FixResult directing the caller to use the apply-candidate endpoint instead.
// For other fixable violations, it runs the appropriate fixer from the
// registered chain.
func (p *Pipeline) FixViolation(ctx context.Context, violationID string) (*FixResult, error) {
	rv, err := p.ruleService.GetViolationByID(ctx, violationID)
	if err != nil {
		return nil, fmt.Errorf("loading violation %s: %w", violationID, err)
	}

	if !rv.Fixable {
		return &FixResult{RuleID: rv.RuleID, Fixed: false, Message: "violation is not fixable"}, nil
	}

	if rv.Status == ViolationStatusDismissed || rv.Status == ViolationStatusResolved {
		return &FixResult{RuleID: rv.RuleID, Fixed: false, Message: "violation is already " + rv.Status}, nil
	}

	if rv.Status == ViolationStatusPendingChoice && len(rv.Candidates) > 0 {
		return &FixResult{RuleID: rv.RuleID, Fixed: false, Message: "candidate selection required"}, nil
	}

	a, err := p.artistService.GetByID(ctx, rv.ArtistID)
	if err != nil {
		// Auto-dismiss orphaned violations whose artist no longer exists.
		if errors.Is(err, artist.ErrNotFound) {
			if dErr := p.ruleService.DismissViolation(ctx, rv.ID); dErr != nil {
				p.logger.Warn("failed to dismiss orphaned violation", "id", rv.ID, "error", dErr)
				return &FixResult{RuleID: rv.RuleID, Fixed: false, Message: "artist deleted; dismiss failed"}, nil
			}
			return &FixResult{RuleID: rv.RuleID, Dismissed: true, Message: "artist deleted; violation dismissed"}, nil
		}
		return nil, fmt.Errorf("loading artist %s: %w", rv.ArtistID, err)
	}

	if a.Locked {
		return &FixResult{RuleID: rv.RuleID, Fixed: false, Message: "artist is locked"}, nil
	}

	r, err := p.getCachedRule(ctx, rv.RuleID)
	if err != nil {
		return nil, fmt.Errorf("loading rule %s: %w", rv.RuleID, err)
	}

	// Build transient violation for the fixer chain.
	v := &Violation{
		RuleID:   rv.RuleID,
		Severity: rv.Severity,
		Message:  rv.Message,
		Fixable:  rv.Fixable,
		Config:   r.Config,
	}

	// Issue #1133: even single-violation FixViolation runs under an
	// EvaluationContext so the post-fix updateHealthScore re-evaluate
	// (which may issue checker-side provider calls) shares the cache
	// the fixer just primed. The win is small (typically a single
	// fetch), but the unified telemetry tag matters for the W4 (#1135)
	// gating signal.
	ctx, counters := p.withEvalContext(ctx, a)
	defer p.logEvalCounters(a, counters)

	fr := p.attemptFix(ctx, a, v)

	if fr.Fixed {
		if err := p.artistService.Update(ctx, a); err != nil {
			return nil, fmt.Errorf("updating artist after fix: %w", err)
		}
		// Record image provenance after Update() creates the artist_images row.
		if fr.SavedPath != "" {
			recordSavedImageProvenance(ctx, p.artistService, a.ID, fr.ImageType, fr.SavedPath, p.logger)
		}
		if err := p.ruleService.ResolveViolation(ctx, rv.ID); err != nil {
			return nil, fmt.Errorf("resolving violation after fix: %w", err)
		}
		// Refresh the health score after the fix, scoped to the rule that was
		// actually fixed. This used to be a full unscoped Evaluate, so repairing
		// a single thumbnail from the UI issued a MusicBrainz query for that
		// artist -- the same unrequested-provider-call defect as the run paths,
		// one artist at a time (#2476).
		//
		// A fixer CAN affect other rules (the thumb fixer can create a
		// byte-identical duplicate; the rename fixer changes a.Path). Those
		// knock-on effects are no longer re-detected here, and that is not a
		// regression: FixViolation does not own rule_results writes (the
		// RunRule/RunAll paths do), so the old full re-evaluation never recorded
		// them either. It only moved the health score. The score now inherits the
		// staleness of the persisted rows instead, and the next full pass corrects
		// it -- which is the same guarantee the score had before.
		//
		// The post-fix evaluation is therefore used only to rescore, not to
		// persist pass rows.
		postEval, err := p.engine.EvaluateScoped(ctx, a, map[string]bool{rv.RuleID: true})
		if err != nil {
			p.logger.Warn("re-evaluating health score after fix",
				"artist", a.Name, "rule_id", rv.RuleID, "error", err)
			postEval = nil
		}
		_ = p.persistHealthAfterRun(ctx, a, postEval, false)
		p.publishAfterFix(ctx, a, fr)
	}

	return fr, nil
}

// getCachedRule returns a rule by ID, using an in-memory cache to avoid
// repeated DB queries during batch operations like fix-all.
func (p *Pipeline) getCachedRule(ctx context.Context, ruleID string) (*Rule, error) {
	p.ruleCacheMu.RLock()
	if r, ok := p.ruleCache[ruleID]; ok {
		p.ruleCacheMu.RUnlock()
		return r, nil
	}
	p.ruleCacheMu.RUnlock()

	r, err := p.ruleService.GetByID(ctx, ruleID)
	if err != nil {
		return nil, err
	}

	p.ruleCacheMu.Lock()
	if p.ruleCache == nil {
		p.ruleCache = make(map[string]*Rule)
	}
	p.ruleCache[ruleID] = r
	p.ruleCacheMu.Unlock()

	return r, nil
}

// ClearRuleCache discards all cached rule lookups. Call this after rule
// configuration changes to ensure subsequent FixViolation calls see fresh data.
func (p *Pipeline) ClearRuleCache() {
	p.ruleCacheMu.Lock()
	p.ruleCache = nil
	p.ruleCacheMu.Unlock()
}

// persistPassResults writes a passed=1 rule_results row for every rule the
// engine considered that did not appear in the violation set. The pipeline
// owns this write (not Engine.Evaluate) because only the pipeline knows when
// an evaluation is authoritative enough to persist outcomes; browse-path GET
// evaluations (handleEvaluateArtist) must not double as writers. Issue #699
// slice 1.
//
// consideredFilter is applied before the violation diff so single-rule runs
// (RunRule / RunRuleScoped) only write pass rows for the specific rule they
// evaluated, not for every rule the engine happened to consider.
//
// Issue #1105: this function is also the natural site for "rule passed, so
// resolve any stale open violation row" reconciliation. When a rule was
// considered AND did not appear in the new violation set, any open or
// pending_choice rule_violations row for that (rule, artist) pair is stale
// and is transitioned to resolved. Dismissed and already-resolved rows are
// left untouched (see ResolveViolationIfActive).
//
// Returns true when every pass write succeeded. Failures are warn-logged and
// fold into the caller's persistOK flag so the artist stays dirty and the
// next pass retries, mirroring how violation-write failures are handled.
func (p *Pipeline) persistPassResults(
	ctx context.Context,
	a *artist.Artist,
	rulesConsidered []string,
	violated map[string]struct{},
	evaluatedAt time.Time,
	consideredFilter func(ruleID string) bool,
) bool {
	ok := true
	for _, rid := range rulesConsidered {
		if consideredFilter != nil && !consideredFilter(rid) {
			continue
		}
		if _, isViolation := violated[rid]; isViolation {
			continue
		}
		if err := p.ruleService.UpsertRuleResultPass(ctx, a.ID, rid, evaluatedAt); err != nil {
			p.logger.Warn("persisting pass result",
				"rule_id", rid, "artist", a.Name, "error", err)
			ok = false
		}
		// Issue #1105: resolve any stale open violation row. A rule that
		// was considered but did not produce a violation in this pass means
		// either the auto-fix succeeded (the resolvedRows path already
		// covered that) OR the underlying condition was corrected
		// out-of-band (user dropped a file in place, scanner refreshed
		// metadata, etc.). The latter has no in-memory marker, so without
		// this reconciliation the dashboard keeps reporting a cleared
		// violation as open indefinitely.
		if _, err := p.ruleService.ResolveViolationIfActive(ctx, rid, a.ID); err != nil {
			p.logger.Warn("resolving stale violation after pass",
				"rule_id", rid, "artist", a.Name, "error", err)
			ok = false
		}
	}
	return ok
}

// findFixer returns the first registered fixer that can handle the violation, or nil.
func (p *Pipeline) findFixer(v *Violation) Fixer {
	for _, f := range p.fixers {
		if f.CanFix(v) {
			return f
		}
	}
	return nil
}

// supportsCandidateDiscovery reports whether a fixer supports being called in
// manual mode without side effects. Returns true only if the fixer implements
// CandidateDiscoverer and returns true from SupportsCandidateDiscovery.
func supportsCandidateDiscovery(f Fixer) bool {
	cd, ok := f.(CandidateDiscoverer)
	return ok && cd.SupportsCandidateDiscovery()
}

// attemptFix tries each registered fixer for the violation.
func (p *Pipeline) attemptFix(ctx context.Context, a *artist.Artist, v *Violation) *FixResult {
	// If a conflict gate is installed, refuse to run auto-fixers whose
	// category would land a file on disk (image, nfo) while write-back or
	// round-trip gating is active. The violation is kept open so the user
	// can see it; the banner explains why the fixer did not run. Without a
	// gate we fall through to the original behavior, preserving test
	// harnesses that do not wire the conflict service. DiscoveryOnly fixes
	// surface candidate lists without touching disk, so they are allowed
	// through even when the gate is closed.
	if g := p.getWriteGate(); g != nil && !v.Config.DiscoveryOnly {
		switch v.Category {
		case "image":
			if err := g.AllowImageWrite(ctx); err != nil {
				return &FixResult{RuleID: v.RuleID, Fixed: false, Message: "image write gated by conflict banner"}
			}
		case "nfo":
			if err := g.AllowNFOWrite(ctx); err != nil {
				return &FixResult{RuleID: v.RuleID, Fixed: false, Message: "nfo write gated by conflict banner"}
			}
		}
	}
	for _, f := range p.fixers {
		if !f.CanFix(v) {
			continue
		}
		fr, err := f.Fix(ctx, a, v)
		if err != nil {
			p.logger.Warn("fix attempt failed",
				"rule", v.RuleID, "artist", a.Name, "error", err)
			return &FixResult{
				RuleID:  v.RuleID,
				Fixed:   false,
				Message: "fix failed",
			}
		}
		// Issue #1108: fixers mutate the filesystem (delete extraneous
		// images, write NFOs, save trimmed logos, rename directories) but
		// do not invalidate the rule engine's FSCache. Within the cache's
		// 60s TTL the next Evaluate call would read a stale directory
		// listing -- the deleted file still appears, the new file is still
		// missing -- and resurrect the violation we just fixed. Invalidate
		// the artist directory here so every fixer benefits without
		// threading the cache into each fixer constructor. DiscoveryOnly
		// fixes do not write to disk and do not need invalidation.
		if fr != nil && fr.Fixed && !v.Config.DiscoveryOnly && a.Path != "" {
			if cache := p.engine.FSCache(); cache != nil {
				cache.InvalidatePath(a.Path)
			}
		}
		return fr
	}
	return &FixResult{
		RuleID:  v.RuleID,
		Fixed:   false,
		Message: "no fixer available",
	}
}

// publishAfterFix publishes metadata or image changes to platforms after a
// single fix succeeds. Used by FixViolation for individual fixes.
func (p *Pipeline) publishAfterFix(ctx context.Context, a *artist.Artist, fr *FixResult) {
	if p.publisher == nil || !fr.Fixed {
		return
	}
	if fr.ImageType != "" {
		p.publisher.SyncImageToPlatforms(ctx, a, fr.ImageType)
	} else {
		p.publisher.PublishMetadata(ctx, a)
	}
}

// publishAccumulated publishes metadata and/or image changes to platforms
// after processing all violations for an artist. Used by RunForArtist and
// RunAll to batch publishing per-artist rather than per-violation.
func (p *Pipeline) publishAccumulated(ctx context.Context, a *artist.Artist, metadataFixed bool, imageTypes []string) {
	if p.publisher == nil {
		return
	}
	if metadataFixed {
		p.publisher.PublishMetadata(ctx, a)
	}
	seen := make(map[string]bool, len(imageTypes))
	for _, it := range imageTypes {
		if seen[it] {
			continue
		}
		seen[it] = true
		p.publisher.SyncImageToPlatforms(ctx, a, it)
	}
}

// persistHealthAfterRun recomputes and persists the artist's health score from
// the run's POST-fix evaluation, then writes the artist row.
//
// For an UNSCOPED run the evaluation already scored every eligible rule, so its
// HealthScore is authoritative and is taken directly. That keeps the whole-artist
// path byte-for-byte what it was.
//
// For a SCOPED run it is not authoritative: health means passed/total across all
// eligible rules and a scoped evaluation only saw some of them. The score is
// therefore derived offline, from the fresh results plus the artist's persisted
// rule_results.
//
// This is the other half of #2476. Scoping the run's own evaluation is not
// enough on its own: the post-fix health recompute used to run a SECOND full
// Evaluate, and because health spans every enabled rule, that second pass re-ran
// the provider-backed checkers no matter which rule was requested. That is why
// one click on a local image rule produced two back-to-back MusicBrainz queries
// per artist instead of none.
//
// mustPersist forces the artist row to be written even when no score could be
// derived, so in-memory mutations made by fixers are still flushed.
// The returned bool is AUTHORITATIVE, not merely "the write succeeded". It is
// false whenever this run cannot vouch for the artist's post-fix state, which is
// what stops the caller from stamping rules_evaluated_at, resolving violation
// rows, or writing pass rows on the strength of a run that did not complete.
func (p *Pipeline) persistHealthAfterRun(ctx context.Context, a *artist.Artist, postEval *EvaluationResult, mustPersist bool) bool {
	// A FAILED post-fix evaluation is never authoritative. The old
	// updateHealthScore encoded this by returning a nil result with
	// authoritative=false, and callers leaned on the nil to gate their writes.
	// Splitting the evaluation out of this function made that gate implicit, and
	// dropping it here would let a failed run stamp rules_evaluated_at, which
	// removes the artist from the dirty set with stale rule_results and means no
	// incremental pass ever looks at it again.
	if postEval == nil {
		if mustPersist {
			// Still flush the fixer's in-memory mutations; just do not claim the
			// run was authoritative.
			if err := p.artistService.UpdateAfterRuleEvaluation(ctx, a); err != nil {
				p.logger.Error("persisting artist after fixes", "artist", a.Name, "error", err)
			}
		}
		return false
	}

	score, ok := postEval.HealthScore, true
	if postEval.Scoped {
		score, ok = p.offlineHealthScore(ctx, a, freshResultsFrom(postEval))
	}

	if ok {
		a.HealthScore = score
		// HealthEvaluatedAt is deliberately NOT stamped on the scoped path. The
		// score there is a merge of this run's fresh results with persisted
		// rule_results of arbitrary age, so its freshness is bounded by the last
		// FULL evaluation, not by now. Stamping it would assert a freshness the
		// score does not have, and would also hide the artist from the health
		// subscriber's bootstrap, which is what establishes the baseline in the
		// first place.
		if !postEval.Scoped {
			now := time.Now().UTC()
			a.HealthEvaluatedAt = &now
		}
	} else if !mustPersist {
		// Nothing to score and no fixer mutation to flush: touching the row would
		// only bump updated_at for nothing. The evaluation itself succeeded, so
		// the run IS authoritative for the rules it was asked to run.
		return true
	}

	// UpdateAfterRuleEvaluation (not Update) for the same reason as the full
	// path: a regular Update would stamp dirty_since and race the walker's
	// rules_evaluated_at stamp at second-precision boundaries.
	if err := p.artistService.UpdateAfterRuleEvaluation(ctx, a); err != nil {
		p.logger.Error("persisting artist after fixes", "artist", a.Name, "error", err)
		return false
	}
	return true
}

// offlineHealthScore derives the artist's health score from the rules this run
// just evaluated plus the persisted results of every other eligible rule. It
// runs no checkers and makes no provider calls.
//
// ok is false when at least one eligible rule has neither a fresh result nor a
// persisted row. That means the artist has never had a complete evaluation, so
// no honest score exists yet, and the caller must leave the stored one alone.
// We refuse rather than score the subset: artist scan treats health_score >= 100
// as "compliant", so a score computed over some of the rules would quietly
// corrupt the compliant/non-compliant split for every downstream consumer.
//
// The denominator comes from EligibleRuleIDs, never from the count of persisted
// rows. A rule enabled since the last full pass has no row, and a rule disabled
// since then still has one; scoring against the rows would drift from what an
// evaluation would actually produce.
func (p *Pipeline) offlineHealthScore(ctx context.Context, a *artist.Artist, fresh map[string]bool) (float64, bool) {
	eligible, err := p.engine.EligibleRuleIDs(ctx, a)
	if err != nil {
		p.logger.Warn("health recompute: listing eligible rules",
			"artist", a.Name, "error", err)
		return 0, false
	}

	rows, err := p.ruleService.GetRuleResultsForArtist(ctx, a.ID)
	if err != nil {
		p.logger.Warn("health recompute: reading persisted rule results",
			"artist", a.Name, "error", err)
		return 0, false
	}
	persisted := make(map[string]bool, len(rows))
	for i := range rows {
		persisted[rows[i].RuleID] = rows[i].Passed
	}

	var passed, total int
	var missing []string
	for _, id := range eligible {
		total++
		if ok, seen := fresh[id]; seen {
			if ok {
				passed++
			}
			continue
		}
		if ok, seen := persisted[id]; seen {
			if ok {
				passed++
			}
			continue
		}
		missing = append(missing, id)
	}

	if len(missing) > 0 {
		// Loud, not silent. A health score that quietly stops updating is the
		// failure mode this codebase keeps producing; the operator gets told
		// what is missing and how to fix it.
		p.logger.Info("health score left unchanged: artist has no complete evaluation baseline",
			"artist", a.Name,
			"eligible_rules", total,
			"never_evaluated_rules", len(missing),
			"first_missing_rule", missing[0],
			"remedy", "run all rules once for this artist to establish a baseline",
		)
		return 0, false
	}

	return calculateHealthScore(passed, total), true
}

// freshResultsFrom turns a scoped evaluation into the rule_id -> passed map that
// the offline health recompute consumes. Every rule the evaluation considered is
// present, so a rule that was evaluated and failed is recorded as false rather
// than being mistaken for "not evaluated".
func freshResultsFrom(eval *EvaluationResult) map[string]bool {
	if eval == nil {
		return nil
	}
	violated := make(map[string]struct{}, len(eval.Violations))
	for i := range eval.Violations {
		violated[eval.Violations[i].RuleID] = struct{}{}
	}
	fresh := make(map[string]bool, len(eval.RulesConsidered))
	for _, id := range eval.RulesConsidered {
		_, bad := violated[id]
		fresh[id] = !bad
	}
	return fresh
}

func (p *Pipeline) updateHealthScore(ctx context.Context, a *artist.Artist, mustPersist bool) (*EvaluationResult, bool) {
	eval, err := p.engine.Evaluate(ctx, a)
	// authoritative is only true when the post-fix evaluation succeeded;
	// returning true after a failed Evaluate would let callers stamp
	// rules_evaluated_at and treat the run as clean even though eval is nil
	// and no pass rows can be written this pass.
	authoritative := err == nil
	if err != nil {
		p.logger.Warn("re-evaluating health score", "artist", a.Name, "error", err)
		if !mustPersist {
			return nil, false
		}
	} else {
		a.HealthScore = eval.HealthScore
		now := time.Now().UTC()
		a.HealthEvaluatedAt = &now
	}
	// UpdateAfterRuleEvaluation (not Update) so the pipeline's own writeback
	// does not stamp dirty_since. The walker is about to stamp
	// rules_evaluated_at with startedAt, and a regular Update would race
	// that stamp at second-precision boundaries (see service.go docs and
	// #698 follow-up: the scheduler test flaked on CI when dirty_since
	// happened to round into the next second after startedAt).
	if err := p.artistService.UpdateAfterRuleEvaluation(ctx, a); err != nil {
		p.logger.Error("persisting artist after fixes", "artist", a.Name, "error", err)
		// Return nil eval so callers that gate pass-row writes on
		// `postEval != nil` cannot upsert passed=1 from in-memory fix
		// state that never reached the artist row (CR review-body
		// 4144589645). rule_results must not lead the stored artist.
		return nil, false
	}
	return eval, authoritative
}
