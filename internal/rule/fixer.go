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
// interface. Fixers that write to disk unconditionally (LogoPaddingFixer,
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

// RunResult describes the outcome of running rules against multiple artists.
type RunResult struct {
	ArtistsProcessed int         `json:"artists_processed"`
	ViolationsFound  int         `json:"violations_found"`
	FixesAttempted   int         `json:"fixes_attempted"`
	FixesSucceeded   int         `json:"fixes_succeeded"`
	Results          []FixResult `json:"results"`
}

// PipelineRunner abstracts the rule pipeline so consumers can be tested
// with stubs instead of requiring a full Engine, Service, and Fixer chain.
type PipelineRunner interface {
	RunForArtist(ctx context.Context, a *artist.Artist) (*RunResult, error)
	RunRule(ctx context.Context, ruleID string) (*RunResult, error)
	RunAll(ctx context.Context) (*RunResult, error)
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

// RunRule evaluates a single rule against all non-excluded artists and attempts fixes.
func (p *Pipeline) RunRule(ctx context.Context, ruleID string) (*RunResult, error) {
	result := &RunResult{}

	// Fetch the rule once to check automation mode for all violations.
	targetRule, err := p.ruleService.GetByID(ctx, ruleID)
	if err != nil {
		return nil, fmt.Errorf("getting rule %s: %w", ruleID, err)
	}

	const pageSize = 200
	params := artist.ListParams{Page: 1, PageSize: pageSize, Sort: "name"}

	for ctx.Err() == nil {
		page, _, err := p.artistService.List(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("listing artists: %w", err)
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

			result.ArtistsProcessed++
			var perRuleMetadata bool
			var perRuleImages []string

			eval, err := p.engine.Evaluate(ctx, a)
			if err != nil {
				p.logger.Warn("evaluating artist", "artist", a.Name, "error", err)
				continue
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
					}
					continue
				}

				// Auto mode (default): attempt fix if fixable
				if !v.Fixable {
					// Persist unfixable violation as open
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
					}
					continue
				}

				fr := p.attemptFix(ctx, a, v)
				result.Results = append(result.Results, *fr)
				result.FixesAttempted++

				// Persist violation with appropriate status after fix attempt
				status := ViolationStatusOpen
				if fr.Fixed {
					result.FixesSucceeded++
					status = ViolationStatusResolved
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
				}
			}

			// Re-evaluate and persist health score, then publish changes.
			p.updateHealthScore(ctx, a)
			p.publishAccumulated(ctx, a, perRuleMetadata, perRuleImages)
		}

		if len(page) < pageSize {
			break
		}
		params.Page++
	}

	return result, nil
}

// RunForArtist evaluates rules and attempts fixes for a single artist,
// respecting each rule's AutomationMode.
func (p *Pipeline) RunForArtist(ctx context.Context, a *artist.Artist) (*RunResult, error) {
	result := &RunResult{}

	if a.IsExcluded || a.Locked {
		return result, nil
	}

	result.ArtistsProcessed = 1

	var metadataFixed bool
	var fixedImageTypes []string

	eval, err := p.engine.Evaluate(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("evaluating artist %s: %w", a.Name, err)
	}

	// Cache rule lookups to avoid repeated DB queries.
	ruleCache := map[string]*Rule{}

	for j := range eval.Violations {
		v := &eval.Violations[j]
		result.ViolationsFound++

		// Look up rule to determine automation mode.
		r, ok := ruleCache[v.RuleID]
		if !ok {
			r, err = p.ruleService.GetByID(ctx, v.RuleID)
			if err != nil {
				p.logger.Warn("fetching rule for violation", "rule_id", v.RuleID, "artist", a.Name, "error", err)
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
		}
	}

	p.updateHealthScore(ctx, a)
	p.publishAccumulated(ctx, a, metadataFixed, fixedImageTypes)
	return result, nil
}

// RunAll evaluates all enabled rules against all non-excluded artists and attempts fixes.
func (p *Pipeline) RunAll(ctx context.Context) (*RunResult, error) {
	result := &RunResult{}

	const pageSize = 200
	params := artist.ListParams{Page: 1, PageSize: pageSize, Sort: "name"}

	// Cache rule lookups to avoid repeated DB queries across artists.
	ruleCache := map[string]*Rule{}

	for ctx.Err() == nil {
		page, _, err := p.artistService.List(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("listing artists: %w", err)
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

			result.ArtistsProcessed++
			var perArtistMetadata bool
			var perArtistImages []string

			eval, err := p.engine.Evaluate(ctx, a)
			if err != nil {
				p.logger.Warn("evaluating artist", "artist", a.Name, "error", err)
				continue
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
				}
			}

			// Re-evaluate and persist health score, then publish changes.
			p.updateHealthScore(ctx, a)
			p.publishAccumulated(ctx, a, perArtistMetadata, perArtistImages)
		}

		if len(page) < pageSize {
			break
		}
		params.Page++
	}

	return result, nil
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
		p.updateHealthScore(ctx, a)
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

// updateHealthScore re-evaluates the artist and persists the score.
func (p *Pipeline) updateHealthScore(ctx context.Context, a *artist.Artist) {
	eval, err := p.engine.Evaluate(ctx, a)
	if err != nil {
		p.logger.Warn("re-evaluating health score", "artist", a.Name, "error", err)
		return
	}
	a.HealthScore = eval.HealthScore
	now := time.Now().UTC()
	a.HealthEvaluatedAt = &now
	if err := p.artistService.Update(ctx, a); err != nil {
		p.logger.Warn("persisting health score", "artist", a.Name, "error", err)
	}
}
