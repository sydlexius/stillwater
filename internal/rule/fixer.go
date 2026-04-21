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

// Pipeline orchestrates rule evaluation and auto-fixing across artists.
type Pipeline struct {
	engine        *Engine
	artistService *artist.Service
	ruleService   *Service
	fixers        []Fixer
	publisher     *publish.Publisher
	logger        *slog.Logger

	ruleCacheMu sync.RWMutex
	ruleCache   map[string]*Rule
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

	processArtist := func(a *artist.Artist) bool {
		var perRuleMetadata bool
		var perRuleImages []string
		var perRuleDirty bool
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

			status := ViolationStatusOpen
			if fr.Fixed {
				result.FixesSucceeded++
				status = ViolationStatusResolved
				perRuleDirty = true
				if fr.ImageType != "" {
					perRuleImages = append(perRuleImages, fr.ImageType)
				} else {
					perRuleMetadata = true
				}
			} else if len(fr.Candidates) > 0 {
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
			if status == ViolationStatusResolved {
				now := time.Now().UTC()
				rv.ResolvedAt = &now
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

// runForArtistFiltered is the shared body of RunForArtist and
// RunImageRulesForArtist. An empty categoryFilter runs every violation;
// a non-empty value runs only violations whose Category matches exactly.
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

	var metadataFixed bool
	var fixedImageTypes []string
	var artistDirty bool // tracks whether the artist model was modified by a fixer
	// persistOK gates the per-artist rules_evaluated_at stamp the same way
	// the multi-artist walker does: any violation/health write failure must
	// leave the artist dirty so the next pass retries.
	persistOK := true

	eval, err := p.engine.Evaluate(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("evaluating artist %s: %w", a.Name, err)
	}

	// Cache rule lookups to avoid repeated DB queries.
	ruleCache := map[string]*Rule{}

	for j := range eval.Violations {
		v := &eval.Violations[j]
		if categoryFilter != "" && v.Category != categoryFilter {
			continue
		}
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

		// Auto mode: persist unfixable violations as open, attempt fixes for fixable ones.
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

		status := ViolationStatusOpen
		if fr.Fixed {
			result.FixesSucceeded++
			status = ViolationStatusResolved
			artistDirty = true
			if fr.ImageType != "" {
				fixedImageTypes = append(fixedImageTypes, fr.ImageType)
			} else {
				metadataFixed = true
			}
		} else if len(fr.Candidates) > 0 {
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
		if status == ViolationStatusResolved {
			now := time.Now().UTC()
			rv.ResolvedAt = &now
		}
		if err := p.ruleService.UpsertViolation(ctx, rv); err != nil {
			p.logger.Warn("persisting fix result violation", "rule_id", v.RuleID, "artist", a.Name, "error", err)
			persistOK = false
		}
	}

	// Issue #699 propagation fix: derive the pass/fail skip-set from the
	// POST-fix evaluation returned by updateHealthScore. A rule the fixer
	// just repaired would otherwise stay in the pre-fix violation snapshot
	// and be suppressed from the pass rows written below.
	postEval, persistOKHealth := p.updateHealthScore(ctx, a, artistDirty)
	if !persistOKHealth {
		persistOK = false
	}

	if postEval != nil {
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
		filterReady := true
		if categoryFilter != "" {
			allowedIDs := make(map[string]struct{}, len(postEval.RulesConsidered))
			for _, rid := range postEval.RulesConsidered {
				r, ok := ruleCache[rid]
				if !ok {
					fetched, err := p.ruleService.GetByID(ctx, rid)
					if err != nil {
						p.logger.Warn("fetching rule for pass filter",
							"rule_id", rid, "artist", a.Name, "error", err)
						filterReady = false
						persistOK = false
						break
					}
					ruleCache[rid] = fetched
					r = fetched
				}
				if r.Category == categoryFilter {
					allowedIDs[rid] = struct{}{}
				}
			}
			passFilter = func(rid string) bool {
				_, ok := allowedIDs[rid]
				return ok
			}
		}

		if filterReady {
			if !p.persistPassResults(ctx, a, postEval.RulesConsidered, postViolated, startedAt, passFilter) {
				persistOK = false
			}
		}
	}

	p.publishAccumulated(ctx, a, metadataFixed, fixedImageTypes)
	// Stamp rules_evaluated_at only when categoryFilter is empty (every
	// enabled rule was considered) AND every persistence step succeeded.
	// A transient write failure must keep the artist dirty so the next
	// pass retries; stamping it clean would hide the dropped state until
	// the next mutation.
	if categoryFilter == "" && persistOK {
		p.markArtistEvaluated(ctx, a, startedAt)
	}
	return result, nil
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

			status := ViolationStatusOpen
			if fr.Fixed {
				result.FixesSucceeded++
				status = ViolationStatusResolved
				perArtistDirty = true
				if fr.ImageType != "" {
					perArtistImages = append(perArtistImages, fr.ImageType)
				} else {
					perArtistMetadata = true
				}
			} else if len(fr.Candidates) > 0 {
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
			if status == ViolationStatusResolved {
				now := time.Now().UTC()
				rv.ResolvedAt = &now
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
