package rule

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

// Fixer attempts to automatically resolve a rule violation.
type Fixer interface {
	// CanFix returns true if this fixer handles the given violation's rule.
	CanFix(v *Violation) bool
	// Fix attempts to resolve the violation for the given artist.
	Fix(ctx context.Context, a *artist.Artist, v *Violation) (*FixResult, error)
}

// FixResult describes the outcome of a fix attempt.
type FixResult struct {
	RuleID     string           `json:"rule_id"`
	Fixed      bool             `json:"fixed"`
	Message    string           `json:"message"`
	Candidates []ImageCandidate `json:"candidates,omitempty"` // set when multiple candidates need user selection
}

// RunResult describes the outcome of running rules against multiple artists.
type RunResult struct {
	ArtistsProcessed int         `json:"artists_processed"`
	ViolationsFound  int         `json:"violations_found"`
	FixesAttempted   int         `json:"fixes_attempted"`
	FixesSucceeded   int         `json:"fixes_succeeded"`
	Results          []FixResult `json:"results"`
}

// Pipeline orchestrates rule evaluation and auto-fixing across artists.
type Pipeline struct {
	engine        *Engine
	artistService *artist.Service
	ruleService   *Service
	fixers        []Fixer
	logger        *slog.Logger
}

// NewPipeline creates a new fix pipeline.
func NewPipeline(engine *Engine, artistService *artist.Service, ruleService *Service, fixers []Fixer, logger *slog.Logger) *Pipeline {
	return &Pipeline{
		engine:        engine,
		artistService: artistService,
		ruleService:   ruleService,
		fixers:        fixers,
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
			if a.IsExcluded {
				continue
			}

			result.ArtistsProcessed++

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

				// Skip if automation is disabled
				if targetRule.AutomationMode == AutomationModeDisabled {
					continue
				}

				// Manual mode: persist without attempting fix
				if targetRule.AutomationMode == AutomationModeManual {
					rv := &RuleViolation{
						RuleID:     v.RuleID,
						ArtistID:   a.ID,
						ArtistName: a.Name,
						Severity:   v.Severity,
						Message:    v.Message,
						Fixable:    v.Fixable,
						Status:     ViolationStatusOpen,
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

			// Re-evaluate and persist health score
			p.updateHealthScore(ctx, a)
		}

		if len(page) < pageSize {
			break
		}
		params.Page++
	}

	return result, nil
}

// RunForArtist evaluates rules and attempts fixes for a single artist.
func (p *Pipeline) RunForArtist(ctx context.Context, a *artist.Artist) (*RunResult, error) {
	result := &RunResult{}

	if a.IsExcluded {
		return result, nil
	}

	result.ArtistsProcessed = 1

	eval, err := p.engine.Evaluate(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("evaluating artist %s: %w", a.Name, err)
	}

	for j := range eval.Violations {
		v := &eval.Violations[j]
		result.ViolationsFound++

		if !v.Fixable {
			continue
		}

		fr := p.attemptFix(ctx, a, v)
		result.Results = append(result.Results, *fr)
		result.FixesAttempted++

		status := ViolationStatusOpen
		if fr.Fixed {
			result.FixesSucceeded++
			status = ViolationStatusResolved
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
	return result, nil
}

// RunAll evaluates all enabled rules against all non-excluded artists and attempts fixes.
func (p *Pipeline) RunAll(ctx context.Context) (*RunResult, error) {
	result := &RunResult{}

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
			if a.IsExcluded {
				continue
			}

			result.ArtistsProcessed++

			eval, err := p.engine.Evaluate(ctx, a)
			if err != nil {
				p.logger.Warn("evaluating artist", "artist", a.Name, "error", err)
				continue
			}

			for j := range eval.Violations {
				v := &eval.Violations[j]
				result.ViolationsFound++

				if !v.Fixable {
					continue
				}

				fr := p.attemptFix(ctx, a, v)
				result.Results = append(result.Results, *fr)
				result.FixesAttempted++

				status := ViolationStatusOpen
				if fr.Fixed {
					result.FixesSucceeded++
					status = ViolationStatusResolved
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

			// Re-evaluate and persist health score
			p.updateHealthScore(ctx, a)
		}

		if len(page) < pageSize {
			break
		}
		params.Page++
	}

	return result, nil
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
				Message: fmt.Sprintf("fix failed: %v", err),
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

// updateHealthScore re-evaluates the artist and persists the score.
func (p *Pipeline) updateHealthScore(ctx context.Context, a *artist.Artist) {
	eval, err := p.engine.Evaluate(ctx, a)
	if err != nil {
		p.logger.Warn("re-evaluating health score", "artist", a.Name, "error", err)
		return
	}
	a.HealthScore = eval.HealthScore
	if err := p.artistService.Update(ctx, a); err != nil {
		p.logger.Warn("persisting health score", "artist", a.Name, "error", err)
	}
}
