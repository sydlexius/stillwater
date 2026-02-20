package rule

import (
	"context"
	"fmt"
	"log/slog"

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
	RuleID  string `json:"rule_id"`
	Fixed   bool   `json:"fixed"`
	Message string `json:"message"`
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
	fixers        []Fixer
	logger        *slog.Logger
}

// NewPipeline creates a new fix pipeline.
func NewPipeline(engine *Engine, artistService *artist.Service, fixers []Fixer, logger *slog.Logger) *Pipeline {
	return &Pipeline{
		engine:        engine,
		artistService: artistService,
		fixers:        fixers,
		logger:        logger.With(slog.String("component", "fix-pipeline")),
	}
}

// RunRule evaluates a single rule against all non-excluded artists and attempts fixes.
func (p *Pipeline) RunRule(ctx context.Context, ruleID string) (*RunResult, error) {
	allArtists, _, err := p.artistService.List(ctx, artist.ListParams{
		Page:     1,
		PageSize: 10000,
		Sort:     "name",
	})
	if err != nil {
		return nil, fmt.Errorf("listing artists: %w", err)
	}

	result := &RunResult{}

	for i := range allArtists {
		if ctx.Err() != nil {
			break
		}

		a := &allArtists[i]
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

			if !v.Fixable {
				continue
			}

			fr := p.attemptFix(ctx, a, v)
			result.Results = append(result.Results, *fr)
			result.FixesAttempted++
			if fr.Fixed {
				result.FixesSucceeded++
			}
		}

		// Re-evaluate and persist health score
		p.updateHealthScore(ctx, a)
	}

	return result, nil
}

// RunAll evaluates all enabled rules against all non-excluded artists and attempts fixes.
func (p *Pipeline) RunAll(ctx context.Context) (*RunResult, error) {
	allArtists, _, err := p.artistService.List(ctx, artist.ListParams{
		Page:     1,
		PageSize: 10000,
		Sort:     "name",
	})
	if err != nil {
		return nil, fmt.Errorf("listing artists: %w", err)
	}

	result := &RunResult{}

	for i := range allArtists {
		if ctx.Err() != nil {
			break
		}

		a := &allArtists[i]
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
			if fr.Fixed {
				result.FixesSucceeded++
			}
		}

		// Re-evaluate and persist health score
		p.updateHealthScore(ctx, a)
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
