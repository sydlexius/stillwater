package rule

import (
	"context"
	"database/sql"
	"log/slog"
	"math"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/platform"
)

// Engine evaluates rules against artists.
type Engine struct {
	service         *Service
	db              *sql.DB
	platformService *platform.Service
	checkers        map[string]Checker
	logger          *slog.Logger
}

// NewEngine creates a rule evaluation engine with all built-in checkers registered.
func NewEngine(service *Service, db *sql.DB, platformService *platform.Service, logger *slog.Logger) *Engine {
	e := &Engine{
		service:         service,
		db:              db,
		platformService: platformService,
		logger:          logger.With(slog.String("component", "rule-engine")),
		checkers: map[string]Checker{
			RuleNFOExists:        checkNFOExists,
			RuleNFOHasMBID:       checkNFOHasMBID,
			RuleThumbExists:      checkThumbExists,
			RuleThumbSquare:      checkThumbSquare,
			RuleThumbMinRes:      checkThumbMinRes,
			RuleFanartExists:     checkFanartExists,
			RuleLogoExists:       checkLogoExists,
			RuleBioExists:        checkBioExists,
			RuleFanartMinRes:     checkFanartMinRes,
			RuleFanartAspect:     checkFanartAspect,
			RuleLogoMinRes:       checkLogoMinRes,
			RuleBannerExists:     checkBannerExists,
			RuleBannerMinRes:     checkBannerMinRes,
			RuleArtistIDMismatch: checkArtistIDMismatch,
		},
	}
	e.checkers[RuleExtraneousImages] = e.makeExtraneousImagesChecker()
	return e
}

// Evaluate runs all enabled rules against an artist and returns the results.
func (e *Engine) Evaluate(ctx context.Context, a *artist.Artist) (*EvaluationResult, error) {
	// Classical artists in skip mode get a perfect score with no evaluation
	if a.IsClassical && GetClassicalMode(ctx, e.db) == ClassicalModeSkip {
		return &EvaluationResult{
			ArtistID:    a.ID,
			ArtistName:  a.Name,
			HealthScore: 100.0,
		}, nil
	}

	rules, err := e.service.List(ctx)
	if err != nil {
		return nil, err
	}

	result := &EvaluationResult{
		ArtistID:   a.ID,
		ArtistName: a.Name,
	}

	for _, r := range rules {
		if !r.Enabled {
			continue
		}

		checker, ok := e.checkers[r.ID]
		if !ok {
			e.logger.Debug("no checker registered for rule", slog.String("rule_id", r.ID))
			continue
		}

		result.RulesTotal++

		v := checker(a, r.Config)
		if v != nil {
			// Use severity from rule config if the checker did not set it
			if v.Severity == "" {
				v.Severity = r.Config.Severity
			}
			v.Config = r.Config
			result.Violations = append(result.Violations, *v)
		} else {
			result.RulesPassed++
		}
	}

	result.HealthScore = calculateHealthScore(result.RulesPassed, result.RulesTotal)

	return result, nil
}

// EvaluateAll runs all enabled rules against multiple artists.
func (e *Engine) EvaluateAll(ctx context.Context, artists []artist.Artist) ([]EvaluationResult, error) {
	var results []EvaluationResult
	for i := range artists {
		if ctx.Err() != nil {
			return results, ctx.Err()
		}
		r, err := e.Evaluate(ctx, &artists[i])
		if err != nil {
			return nil, err
		}
		results = append(results, *r)
	}
	return results, nil
}

// calculateHealthScore returns the percentage of rules passed, rounded to 1 decimal.
func calculateHealthScore(passed, total int) float64 {
	if total == 0 {
		return 100.0
	}
	score := (float64(passed) / float64(total)) * 100.0
	return math.Round(score*10) / 10
}
