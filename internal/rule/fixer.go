package rule

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
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

	ruleCacheMu sync.RWMutex
	ruleCache   map[string]*Rule
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
	h := p.getHistoryService()
	if h == nil {
		return
	}
	if err := h.Record(ctx, artistID, "rule_fix", "", fr.Message, "rule:"+fr.RuleID); err != nil {
		p.logger.Warn("recording rule auto-fix history",
			"rule_id", fr.RuleID, "artist_id", artistID, "error", err)
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
//
//nolint:gocognit // Single-rule pipeline run (cog 74): progress reporting against ArtistsTotal, scope-aware artist walk, per-artist evaluate-fix-persist with deferred resolved-status writes (#983 ordering), automation-mode dispatch (manual vs auto), and result aggregation; identical control-flow structure to RunAllScoped but pinned to one rule for surgical user runs from the UI -- the duplication itself is the structural smell. Refactor tracked in #1543.
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

	processArtist := func(a *artist.Artist) bool {
		var perRuleMetadata bool
		var perRuleImages []string
		var perRuleDirty bool
		// resolvedRows collects violation upserts that should land with
		// Status=ViolationStatusResolved. Issue #983: these are deferred
		// until AFTER updateHealthScore persists the mutated artist --
		// writing them inline meant the violation was marked resolved
		// even when the trailing artist Update failed, silently dropping
		// the fix.
		var resolvedRows []*RuleViolation
		// persistOK tracks whether every violation/health write for this
		// artist reached the DB. A single transient failure is enough to
		// leave the artist dirty for retry; silently stamping it clean
		// would hide the dropped violation until the next mutation.
		persistOK := true
		// startedAt is captured before evaluation so every rule_results
		// row written during this pass shares a timestamp (issue #699).
		startedAt := time.Now().UTC()

		eval, err := p.engine.Evaluate(ctx, a)
		if err != nil {
			p.logger.Warn("evaluating artist", "artist", a.Name, "error", err)
			return false
		}

		for j := range eval.Violations {
			v := &eval.Violations[j]
			if v.RuleID != ruleID {
				continue
			}
			result.ViolationsFound++

			// Manual mode: discover candidates but never auto-apply.
			// Only invoke fixers that support candidate discovery
			// without side effects. Side-effect fixers (LogoPaddingFixer,
			// NFOFixer, ExtraneousImagesFixer) are skipped.
			if targetRule.AutomationMode == AutomationModeManual {
				fixer := p.findFixer(v)
				if !v.Fixable || fixer == nil || !supportsCandidateDiscovery(fixer) {
					rv := &RuleViolation{
						RuleID:     v.RuleID,
						ArtistID:   a.ID,
						ArtistName: a.Name,
						Severity:   v.Severity,
						Message:    v.Message,
						Fixable:    v.Fixable && fixer != nil,
						Status:     ViolationStatusOpen,
					}
					if err := p.ruleService.UpsertViolation(ctx, rv); err != nil {
						p.logger.Warn("persisting manual-mode violation", "rule_id", ruleID, "artist", a.Name, "error", err)
						persistOK = false
					}
					continue
				}

				v.Config.DiscoveryOnly = true
				fr := p.attemptFix(ctx, a, v)
				result.Results = append(result.Results, *fr)
				result.FixesAttempted++

				status := ViolationStatusOpen
				if len(fr.Candidates) > 0 {
					status = ViolationStatusPendingChoice
				}

				rv := &RuleViolation{
					RuleID:     v.RuleID,
					ArtistID:   a.ID,
					ArtistName: a.Name,
					Severity:   v.Severity,
					Message:    v.Message,
					Fixable:    true,
					Status:     status,
					Candidates: fr.Candidates,
				}
				if err := p.ruleService.UpsertViolation(ctx, rv); err != nil {
					p.logger.Warn("persisting manual-mode violation", "rule_id", ruleID, "artist", a.Name, "error", err)
					persistOK = false
				}
				continue
			}

			// Auto mode (default): attempt fix if fixable
			if !v.Fixable {
				rv := &RuleViolation{
					RuleID:     v.RuleID,
					ArtistID:   a.ID,
					ArtistName: a.Name,
					Severity:   v.Severity,
					Message:    v.Message,
					Fixable:    false,
					Status:     ViolationStatusOpen,
				}
				if err := p.ruleService.UpsertViolation(ctx, rv); err != nil {
					p.logger.Warn("persisting unfixable violation", "rule_id", ruleID, "artist", a.Name, "error", err)
					persistOK = false
				}
				continue
			}

			fr := p.attemptFix(ctx, a, v)
			result.Results = append(result.Results, *fr)
			result.FixesAttempted++

			// Issue #983: defer the resolved upsert until AFTER
			// updateHealthScore persists the mutated artist. Writing
			// ViolationStatusResolved inline would silently drop the
			// fix when the trailing Update fails -- the violation row
			// would say "resolved" but the artist row would be
			// unchanged. Stash the resolved rows here and upsert them
			// only once we know the artist row landed.
			if fr.Fixed {
				result.FixesSucceeded++
				perRuleDirty = true
				if fr.ImageType != "" {
					perRuleImages = append(perRuleImages, fr.ImageType)
				} else {
					perRuleMetadata = true
				}
				// Issue #1106: emit a Recent Activity entry so users see
				// what the rule engine just did on their behalf. Best-effort;
				// recordRuleFixHistory warn-logs on failure and never fails
				// the surrounding fix flow.
				p.recordRuleFixHistory(ctx, a.ID, fr)
				resolvedRows = append(resolvedRows, &RuleViolation{
					RuleID:     v.RuleID,
					ArtistID:   a.ID,
					ArtistName: a.Name,
					Severity:   v.Severity,
					Message:    v.Message,
					Fixable:    true,
					Candidates: fr.Candidates,
				})
				continue
			}

			// Non-resolved statuses (open / pending_choice) are safe
			// to persist inline because they do not depend on the
			// artist row being updated: an open violation simply
			// records that work is still pending, and pending_choice
			// records the candidate list the user must pick from.
			status := ViolationStatusOpen
			if len(fr.Candidates) > 0 {
				status = ViolationStatusPendingChoice
			}
			rv := &RuleViolation{
				RuleID:     v.RuleID,
				ArtistID:   a.ID,
				ArtistName: a.Name,
				Severity:   v.Severity,
				Message:    v.Message,
				Fixable:    true,
				Status:     status,
				Candidates: fr.Candidates,
			}
			if err := p.ruleService.UpsertViolation(ctx, rv); err != nil {
				p.logger.Warn("persisting fix result violation", "rule_id", ruleID, "artist", a.Name, "error", err)
				persistOK = false
			}
		}

		// Issue #699 propagation fix: derive the pass/fail skip-set from
		// the POST-fix evaluation, not the pre-fix snapshot. A rule the
		// fixer just repaired still appears in the pre-fix Violations
		// slice, so using that set would suppress its pass row for this
		// run. updateHealthScore re-evaluates the artist anyway (to
		// recompute the health score), so we reuse that result.
		postEval, persistOKHealth := p.updateHealthScore(ctx, a, perRuleDirty)
		if !persistOKHealth {
			persistOK = false
		}
		// Issue #983: only mark violations resolved once the artist row
		// persist succeeded. When persistOKHealth is false the artist
		// mutation never reached the DB, so leaving the violations in
		// their pre-fix state (open) keeps them in the queue for the next
		// pass to retry instead of silently dropping the repair.
		if persistOKHealth {
			now := time.Now().UTC()
			for _, rv := range resolvedRows {
				rv.Status = ViolationStatusResolved
				rv.ResolvedAt = &now
				if err := p.ruleService.UpsertViolation(ctx, rv); err != nil {
					p.logger.Warn("persisting resolved violation", "rule_id", rv.RuleID, "artist", a.Name, "error", err)
					persistOK = false
				}
			}
		}
		if postEval != nil {
			postViolated := make(map[string]struct{}, len(postEval.Violations))
			for j := range postEval.Violations {
				postViolated[postEval.Violations[j].RuleID] = struct{}{}
			}
			// Single-rule scope: only persist the pass row for the
			// specific rule this invocation evaluated.
			passFilter := func(rid string) bool { return rid == ruleID }
			if !p.persistPassResults(ctx, a, postEval.RulesConsidered, postViolated, startedAt, passFilter) {
				persistOK = false
			}
		}
		p.publishAccumulated(ctx, a, perRuleMetadata, perRuleImages)
		return persistOK
	}

	// Single-rule run does not cover every enabled rule, so leave
	// rules_evaluated_at untouched. Otherwise running rule A would mark
	// the artist clean and rule B's RunRule pass would silently skip it.
	processed, err := p.walkScopedArtists(ctx, scope, false, processArtist)
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

	eval, err := p.engine.Evaluate(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("evaluating artist %s: %w", a.Name, err)
	}

	acc := &runForArtistAccum{persistOK: true}
	// Cache rule lookups so the per-violation dispatch and the
	// post-filter pass-row writer share a single set of DB reads.
	ruleCache := map[string]*Rule{}

	p.dispatchViolations(ctx, a, eval.Violations, categoryFilter, ruleCache, acc, result)
	p.finalizeArtistRun(ctx, a, ruleCache, acc, categoryFilter, startedAt)
	return result, nil
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
func (p *Pipeline) finalizeArtistRun(ctx context.Context, a *artist.Artist, ruleCache map[string]*Rule, acc *runForArtistAccum, categoryFilter string, startedAt time.Time) {
	postEval, persistOKHealth := p.updateHealthScore(ctx, a, acc.artistDirty)
	if !persistOKHealth {
		acc.persistOK = false
	}
	// Issue #983: only resolve violations once the artist row persist
	// succeeded. A failed Update leaves the mutation in memory; marking
	// the violation resolved would silently drop the fix.
	if persistOKHealth && !p.finalizeResolvedRows(ctx, a, acc.resolvedRows) {
		acc.persistOK = false
	}
	if postEval != nil && !p.writeFilteredPassResults(ctx, a, postEval, ruleCache, categoryFilter, startedAt) {
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
//
//nolint:gocognit // Full-sweep pipeline (cog 77): rule registry walk, per-rule artist iteration with scope-aware source, evaluate-fix-persist + #983 deferred resolved-status writes, automation-mode dispatch, per-rule and global counters; the rule-major iteration order is required so that all violations of a given rule see a coherent before/after view of the artist within a single pipeline pass. Refactor tracked in #1542.
func (p *Pipeline) RunAllScoped(ctx context.Context, scope RunScope) (*RunResult, error) {
	result := &RunResult{Scope: scope.String()}

	total, totalErr := p.artistService.CountEligibleArtists(ctx)
	if totalErr != nil {
		p.logger.Warn("counting eligible artists for run-all progress", "error", totalErr)
	}
	result.ArtistsTotal = total

	// Cache rule lookups to avoid repeated DB queries across artists.
	ruleCache := map[string]*Rule{}

	processArtist := func(a *artist.Artist) bool {
		var perArtistMetadata bool
		var perArtistImages []string
		var perArtistDirty bool
		// resolvedRows collects violation upserts that should land with
		// Status=ViolationStatusResolved. Issue #983: deferred until
		// AFTER updateHealthScore persists the mutated artist so a
		// failed artist Update does not silently mark the violation
		// resolved while the actual fix never reached the DB.
		var resolvedRows []*RuleViolation
		// See RunRuleScoped's processArtist: persistOK gates the
		// rules_evaluated_at stamp so a transient DB failure keeps the
		// artist in the dirty set for retry instead of masking dropped
		// violations until the next mutation.
		persistOK := true
		// startedAt captured pre-Evaluate so every rule_results pass row
		// written during this pass shares a timestamp (issue #699).
		startedAt := time.Now().UTC()

		eval, err := p.engine.Evaluate(ctx, a)
		if err != nil {
			p.logger.Warn("evaluating artist", "artist", a.Name, "error", err)
			return false
		}

		for j := range eval.Violations {
			v := &eval.Violations[j]
			result.ViolationsFound++

			// Look up rule to determine automation mode.
			r, ok := ruleCache[v.RuleID]
			if !ok {
				r, err = p.ruleService.GetByID(ctx, v.RuleID)
				if err != nil {
					p.logger.Warn("fetching rule for violation", "rule_id", v.RuleID, "artist", a.Name, "error", err)
					persistOK = false
					continue
				}
				ruleCache[v.RuleID] = r
			}

			// Manual mode: discover candidates but never auto-apply.
			// Only invoke fixers that support candidate discovery
			// without side effects.
			if r.AutomationMode == AutomationModeManual {
				fixer := p.findFixer(v)
				if !v.Fixable || fixer == nil || !supportsCandidateDiscovery(fixer) {
					rv := &RuleViolation{
						RuleID:     v.RuleID,
						ArtistID:   a.ID,
						ArtistName: a.Name,
						Severity:   v.Severity,
						Message:    v.Message,
						Fixable:    v.Fixable && fixer != nil,
						Status:     ViolationStatusOpen,
					}
					if err := p.ruleService.UpsertViolation(ctx, rv); err != nil {
						p.logger.Warn("persisting manual-mode violation", "rule_id", v.RuleID, "artist", a.Name, "error", err)
						persistOK = false
					}
					continue
				}

				v.Config.DiscoveryOnly = true
				fr := p.attemptFix(ctx, a, v)
				result.Results = append(result.Results, *fr)
				result.FixesAttempted++

				status := ViolationStatusOpen
				if len(fr.Candidates) > 0 {
					status = ViolationStatusPendingChoice
				}

				rv := &RuleViolation{
					RuleID:     v.RuleID,
					ArtistID:   a.ID,
					ArtistName: a.Name,
					Severity:   v.Severity,
					Message:    v.Message,
					Fixable:    true,
					Status:     status,
					Candidates: fr.Candidates,
				}
				if err := p.ruleService.UpsertViolation(ctx, rv); err != nil {
					p.logger.Warn("persisting manual-mode violation", "rule_id", v.RuleID, "artist", a.Name, "error", err)
					persistOK = false
				}
				continue
			}

			// Auto mode (default): persist unfixable as open, attempt fix for fixable
			if !v.Fixable {
				rv := &RuleViolation{
					RuleID:     v.RuleID,
					ArtistID:   a.ID,
					ArtistName: a.Name,
					Severity:   v.Severity,
					Message:    v.Message,
					Fixable:    false,
					Status:     ViolationStatusOpen,
				}
				if err := p.ruleService.UpsertViolation(ctx, rv); err != nil {
					p.logger.Warn("persisting unfixable violation", "rule_id", v.RuleID, "artist", a.Name, "error", err)
					persistOK = false
				}
				continue
			}

			fr := p.attemptFix(ctx, a, v)
			result.Results = append(result.Results, *fr)
			result.FixesAttempted++

			// Issue #983: defer the resolved upsert until AFTER
			// updateHealthScore persists the mutated artist. See the
			// matching comment in RunRuleScoped.processArtist.
			if fr.Fixed {
				result.FixesSucceeded++
				perArtistDirty = true
				if fr.ImageType != "" {
					perArtistImages = append(perArtistImages, fr.ImageType)
				} else {
					perArtistMetadata = true
				}
				// Issue #1106: emit a Recent Activity entry. See the
				// matching comment + helper docstring in
				// RunRuleScoped.processArtist.
				p.recordRuleFixHistory(ctx, a.ID, fr)
				resolvedRows = append(resolvedRows, &RuleViolation{
					RuleID:     v.RuleID,
					ArtistID:   a.ID,
					ArtistName: a.Name,
					Severity:   v.Severity,
					Message:    v.Message,
					Fixable:    true,
					Candidates: fr.Candidates,
				})
				continue
			}

			status := ViolationStatusOpen
			if len(fr.Candidates) > 0 {
				status = ViolationStatusPendingChoice
			}
			rv := &RuleViolation{
				RuleID:     v.RuleID,
				ArtistID:   a.ID,
				ArtistName: a.Name,
				Severity:   v.Severity,
				Message:    v.Message,
				Fixable:    true,
				Status:     status,
				Candidates: fr.Candidates,
			}
			if err := p.ruleService.UpsertViolation(ctx, rv); err != nil {
				p.logger.Warn("persisting fix result violation", "rule_id", v.RuleID, "artist", a.Name, "error", err)
				persistOK = false
			}
		}

		// Issue #699 propagation fix: derive the pass/fail skip-set from
		// the POST-fix evaluation returned by updateHealthScore so rules
		// repaired during this pass are recorded as passed=1 in the same
		// run. Using the pre-fix violation snapshot would suppress them
		// until the next evaluation.
		postEval, persistOKHealth := p.updateHealthScore(ctx, a, perArtistDirty)
		if !persistOKHealth {
			persistOK = false
		}
		// Issue #983: only resolve violations once the artist row
		// persisted cleanly. A failed Update leaves the mutation in
		// memory; marking the violation resolved anyway would silently
		// drop the fix.
		if persistOKHealth {
			now := time.Now().UTC()
			for _, rv := range resolvedRows {
				rv.Status = ViolationStatusResolved
				rv.ResolvedAt = &now
				if err := p.ruleService.UpsertViolation(ctx, rv); err != nil {
					p.logger.Warn("persisting resolved violation", "rule_id", rv.RuleID, "artist", a.Name, "error", err)
					persistOK = false
				}
			}
		}
		if postEval != nil {
			postViolated := make(map[string]struct{}, len(postEval.Violations))
			for j := range postEval.Violations {
				postViolated[postEval.Violations[j].RuleID] = struct{}{}
			}
			if !p.persistPassResults(ctx, a, postEval.RulesConsidered, postViolated, startedAt, nil) {
				persistOK = false
			}
		}
		p.publishAccumulated(ctx, a, perArtistMetadata, perArtistImages)
		return persistOK
	}

	// RunAll covers every enabled rule, so it owns rules_evaluated_at:
	// after this pass the artist is fully up-to-date and falls out of
	// the dirty set until the next mutation.
	processed, err := p.walkScopedArtists(ctx, scope, true, processArtist)
	if err != nil {
		return nil, err
	}
	result.ArtistsProcessed = processed
	// See RunRuleScoped for why artists_skipped is only computed for
	// scope=incremental.
	if scope == RunScopeIncremental && result.ArtistsTotal > processed {
		result.ArtistsSkipped = result.ArtistsTotal - processed
	}

	return result, nil
}

// walkScopedArtists invokes fn for every artist that matches the requested
// scope. When markEvaluated is true, rules_evaluated_at is stamped after
// each artist so they fall out of the dirty set on the next incremental
// pass. RunRuleScoped passes false because a single-rule sweep does not
// evaluate every rule and should not claim the artist is fully up-to-date;
// RunAllScoped passes true because it does cover every enabled rule.
//
// For scope=incremental, the dirty list is queried up front in a single
// SQL call -- the dirty filter index keeps this fast even when zero artists
// are dirty. For scope=all, the existing paginated List walk is preserved
// so memory usage stays bounded on large libraries.
//
// rules_evaluated_at is stamped with the artist's per-iteration start time
// (captured before fn runs), not time.Now() after fn returns. This protects
// against a race where an ArtistUpdated event arrives mid-process: the
// async DirtySubscriber stamps dirty_since with a "now" timestamp that
// must remain strictly greater than rules_evaluated_at, so the artist
// stays in the dirty set on the next pass and the in-flight mutation is
// not silently dropped.
// fn returns true only when the whole artist pass persisted cleanly:
// engine.Evaluate succeeded AND every violation upsert AND the trailing
// artist Update reached the DB. A false return means anything from the
// evaluate/upsert/update chain warn-logged a failure, and the walker then
// leaves the artist in the dirty set so the next pass retries. This is
// the protection against silent data loss flagged in the #698 review:
// without the stricter bool, a transient DB error on a later step would
// stamp the artist as evaluated and the dropped violation (or stale health
// score) would be hidden until the next mutation.
//
// processed counts artists fn was actually invoked on (regardless of return
// value), since both successes and failures consumed pipeline work.
//
//nolint:gocognit // Scope walker (cog 45): dirty/all/single-artist scopes with per-scope source enumeration (ListDirtyIDs vs ListEligible vs single ID) and per-artist evaluated-mark policy gated on fn outcome; the markEvaluated bool flips meaning per scope, which is the structural smell driving the cog score. Refactor tracked in #1551.
func (p *Pipeline) walkScopedArtists(ctx context.Context, scope RunScope, markEvaluated bool, fn func(a *artist.Artist) bool) (int, error) {
	if scope == RunScopeIncremental {
		ids, err := p.artistService.ListDirtyIDs(ctx)
		if err != nil {
			return 0, fmt.Errorf("listing dirty artists: %w", err)
		}
		processed := 0
		for _, id := range ids {
			if ctx.Err() != nil {
				return processed, ctx.Err()
			}
			a, err := p.artistService.GetByID(ctx, id)
			if err != nil {
				p.logger.Warn("loading dirty artist", "artist_id", id, "error", err)
				continue
			}
			// The dirty list filter excludes locked/excluded already, but
			// the row state may have changed between query and load.
			if a.IsExcluded || a.Locked {
				continue
			}
			startedAt := time.Now().UTC()
			ok := fn(a)
			// Only count + stamp artists that actually completed
			// evaluation. A false return means fn bailed (engine
			// error) and intentionally left the artist dirty for
			// retry; counting it as processed would over-report in
			// the "evaluated X of Y (Z unchanged)" summary and
			// stamping rules_evaluated_at would hide the next run.
			if ok {
				processed++
				if markEvaluated {
					p.markArtistEvaluated(ctx, a, startedAt)
				}
			}
		}
		return processed, nil
	}

	// scope=all: paginated walk over every artist, identical to the legacy path.
	const pageSize = 200
	params := artist.ListParams{Page: 1, PageSize: pageSize, Sort: "name"}
	processed := 0
	for ctx.Err() == nil {
		page, _, err := p.artistService.List(ctx, params)
		if err != nil {
			return processed, fmt.Errorf("listing artists: %w", err)
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
			startedAt := time.Now().UTC()
			ok := fn(a)
			// See the scope=incremental branch above: failed
			// evaluations must not count toward processed nor get
			// their rules_evaluated_at stamped.
			if ok {
				processed++
				if markEvaluated {
					p.markArtistEvaluated(ctx, a, startedAt)
				}
			}
		}
		if len(page) < pageSize {
			break
		}
		params.Page++
	}
	// Propagate ctx.Err() if the walk exited because of cancellation so
	// callers can distinguish a partial run from a clean completion.
	return processed, ctx.Err()
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
		// FixViolation operates on a single violation and does not own
		// rule_results writes (the pipeline's RunRule/RunAll paths do),
		// so we discard the post-fix evaluation result here.
		_, _ = p.updateHealthScore(ctx, a, false)
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

// updateHealthScore re-evaluates the artist and persists the score. Returns
// the post-fix evaluation (nil when Evaluate failed) and a bool that is true
// only when the artist row reached the DB cleanly. The walker uses the bool
// to decide whether to stamp rules_evaluated_at: a failed persist must leave
// the artist in the dirty set so the next pass retries.
//
// The returned EvaluationResult is consumed by the pass-row writer so
// rule_results reflects the POST-fix state of the artist -- a rule the
// fixer just repaired shows up as passed=1 in the same run, and a rule
// that started passing but failed mid-run is written as a fail (issue #699).
//
// When mustPersist is true, the artist is persisted even if health
// evaluation fails, to flush in-memory changes made by fixers. In that
// case the caller relies on the returned bool to detect the transient
// persist failure.
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
