package rule

import (
	"context"
	"log/slog"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// EvaluateAndPersistHealth runs the rule engine against an artist and persists
// the resulting health score. Errors are logged but not propagated (non-blocking).
func EvaluateAndPersistHealth(ctx context.Context, engine *Engine, svc *artist.Service, a *artist.Artist, logger *slog.Logger) {
	if engine == nil {
		return
	}
	result, err := engine.Evaluate(ctx, a)
	if err != nil {
		logger.Warn("evaluating health score", "artist_id", a.ID, "artist", a.Name, "error", err)
		return
	}
	a.HealthScore = result.HealthScore
	if err := svc.Update(ctx, a); err != nil {
		logger.Warn("persisting health score", "artist_id", a.ID, "artist", a.Name, "error", err)
	}
}

// UpdateProviderFetchTimestamps records the current time as the last-fetched
// timestamp for each attempted provider. Only providers with a corresponding
// fetched_at column (audiodb, discogs, wikidata, lastfm) are persisted;
// unsupported providers are logged as warnings and skipped.
func UpdateProviderFetchTimestamps(ctx context.Context, svc *artist.Service, artistID string, attempted []provider.ProviderName, logger *slog.Logger) {
	for _, prov := range attempted {
		if err := svc.UpdateProviderFetchedAt(ctx, artistID, string(prov)); err != nil {
			logger.Warn("updating provider fetched_at",
				"artist_id", artistID,
				"provider", prov,
				"error", err)
		}
	}
}
