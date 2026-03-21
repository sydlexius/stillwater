package rule

import (
	"context"
	"database/sql"
	"log/slog"
	"math"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/platform"
)

// Engine evaluates rules against artists.
type Engine struct {
	service         *Service
	db              *sql.DB
	platformService *platform.Service
	libraryService  *library.Service
	checkers        map[string]Checker
	logger          *slog.Logger

	// sharedFSCache caches IsSharedFilesystem results by library ID during
	// a single evaluation run to avoid N+1 DB queries when multiple artists
	// share the same library. Cleared at the start of each Evaluate call.
	sharedFSCache map[string]bool
}

// NewEngine creates a rule evaluation engine with all built-in checkers registered.
func NewEngine(service *Service, db *sql.DB, platformService *platform.Service, libraryService *library.Service, logger *slog.Logger) *Engine {
	e := &Engine{
		service:         service,
		db:              db,
		platformService: platformService,
		libraryService:  libraryService,
		logger:          logger.With(slog.String("component", "rule-engine")),
		checkers: map[string]Checker{
			RuleNFOExists:             checkNFOExists,
			RuleNFOHasMBID:            checkNFOHasMBID,
			RuleThumbExists:           checkThumbExists,
			RuleThumbSquare:           checkThumbSquare,
			RuleThumbMinRes:           checkThumbMinRes,
			RuleFanartExists:          checkFanartExists,
			RuleLogoExists:            checkLogoExists,
			RuleBioExists:             checkBioExists,
			RuleFanartMinRes:          checkFanartMinRes,
			RuleFanartAspect:          checkFanartAspect,
			RuleLogoMinRes:            checkLogoMinRes,
			RuleBannerExists:          checkBannerExists,
			RuleBannerMinRes:          checkBannerMinRes,
			RuleArtistIDMismatch:      checkArtistIDMismatch,
			RuleLogoTrimmable:         checkLogoTrimmable,
			RuleDirectoryNameMismatch: checkDirectoryNameMismatch,
			RuleMetadataQuality:       checkMetadataQuality,
			RuleLogoPadding:           checkLogoPadding,
		},
	}
	e.checkers[RuleExtraneousImages] = e.makeExtraneousImagesChecker()
	e.checkers[RuleImageDuplicate] = e.makeImageDuplicateChecker()
	e.checkers[RuleBackdropSequencing] = e.makeBackdropSequencingChecker()
	return e
}

// Evaluate runs all enabled rules against an artist and returns the results.
func (e *Engine) Evaluate(ctx context.Context, a *artist.Artist) (*EvaluationResult, error) {
	// Clear per-evaluation shared-filesystem cache so each top-level Evaluate
	// call gets fresh data while avoiding N+1 queries within the same run.
	e.sharedFSCache = nil

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

// IsSharedFilesystem reports whether the given artist's library has the
// shared_filesystem flag set. Returns false if the library service is nil
// or the artist has no library ID. Returns true (fail closed) on DB errors
// to prevent destructive operations when the database is unavailable.
//
// Results are cached per library ID for the duration of a single evaluation
// run (cache is cleared at the start of each Evaluate call) to avoid N+1
// DB queries when multiple checkers call this for the same artist.
func (e *Engine) IsSharedFilesystem(ctx context.Context, a *artist.Artist) bool {
	if e.libraryService == nil || a.LibraryID == "" {
		return false
	}

	// Check the per-evaluation cache first.
	if e.sharedFSCache != nil {
		if cached, ok := e.sharedFSCache[a.LibraryID]; ok {
			return cached
		}
	}

	lib, err := e.libraryService.GetByID(ctx, a.LibraryID)
	if err != nil {
		// Fail closed: assume shared filesystem when the DB is unavailable
		// to prevent destructive operations (e.g. deleting "extraneous" images
		// that a platform actually owns).
		e.logger.Warn("library lookup failed; assuming shared filesystem",
			slog.String("library_id", a.LibraryID),
			slog.String("error", err.Error()))
		e.cacheSharedFS(a.LibraryID, true)
		return true
	}

	e.cacheSharedFS(a.LibraryID, lib.SharedFilesystem)
	return lib.SharedFilesystem
}

// cacheSharedFS stores a shared-filesystem lookup result in the per-evaluation
// cache, lazily initializing the map on first use.
func (e *Engine) cacheSharedFS(libraryID string, shared bool) {
	if e.sharedFSCache == nil {
		e.sharedFSCache = make(map[string]bool)
	}
	e.sharedFSCache[libraryID] = shared
}

// calculateHealthScore returns the percentage of rules passed, rounded to 1 decimal.
func calculateHealthScore(passed, total int) float64 {
	if total == 0 {
		return 100.0
	}
	score := (float64(passed) / float64(total)) * 100.0
	return math.Round(score*10) / 10
}
