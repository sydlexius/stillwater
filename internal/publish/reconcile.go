package publish

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	img "github.com/sydlexius/stillwater/internal/image"
)

// newArtistStateGetter constructs a connection.ArtistStateGetter for the
// given connection type. Returns nil for unsupported types (e.g. Lidarr).
func newArtistStateGetter(conn *connection.Connection, logger *slog.Logger) connection.ArtistStateGetter {
	switch conn.Type {
	case connection.TypeEmby:
		return emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)
	case connection.TypeJellyfin:
		return jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)
	default:
		return nil
	}
}

// platformHasImageType reports whether the platform state indicates the given
// image type is present on the platform.
func platformHasImageType(state *connection.ArtistPlatformState, imageType string) bool {
	switch imageType {
	case "thumb":
		return state.HasThumb
	case "logo":
		return state.HasLogo
	case "banner":
		return state.HasBanner
	default:
		return false
	}
}

// artworkNeeds groups the boolean flags that indicate which image types must
// be pushed to bring a platform mirror up to date.
type artworkNeeds struct {
	fanart bool
	thumb  bool
	logo   bool
	banner bool
}

func (n artworkNeeds) any() bool {
	return n.fanart || n.thumb || n.logo || n.banner
}

// detectMissingArtwork queries each platform connection for the artist and
// sets a flag for each image type present locally but absent on the mirror.
// Per-connection errors are logged and skipped; the returned needs struct
// represents the union across all connections.
func (p *Publisher) detectMissingArtwork(
	ctx context.Context,
	artistID, dir string,
	platformIDs []artist.PlatformID,
) artworkNeeds {
	var needs artworkNeeds
	for _, pid := range platformIDs {
		conn, connErr := p.connectionService.GetByID(ctx, pid.ConnectionID)
		if connErr != nil {
			p.logger.Warn("artwork reconciler: getting connection",
				slog.String("artist_id", artistID),
				slog.String("connection_id", pid.ConnectionID),
				slog.Any("error", connErr))
			continue
		}
		if !conn.Enabled || conn.Status != "ok" || !conn.GetFeatureImageWrite() {
			continue
		}

		stateGetter := newArtistStateGetter(conn, p.logger)
		if stateGetter == nil {
			continue
		}

		state, stateErr := stateGetter.GetArtistDetail(ctx, pid.PlatformArtistID)
		if stateErr != nil {
			p.logger.Warn("artwork reconciler: fetching platform state",
				slog.String("artist_id", artistID),
				slog.String("connection", conn.Name),
				slog.Any("error", stateErr))
			continue
		}

		p.accumulateNeeds(ctx, artistID, dir, state, &needs)
	}
	return needs
}

// accumulateNeeds merges per-connection platform state into the shared needs
// struct, checking local file presence for each image type not yet flagged.
func (p *Publisher) accumulateNeeds(
	ctx context.Context,
	artistID, dir string,
	state *connection.ArtistPlatformState,
	needs *artworkNeeds,
) {
	if !needs.fanart {
		primary := p.getActiveFanartPrimary(ctx)
		fanartPaths, discoverErr := img.DiscoverFanart(dir, primary)
		if discoverErr != nil {
			p.logger.Warn("artwork reconciler: discovering fanart",
				slog.String("artist_id", artistID),
				slog.String("dir", dir),
				slog.Any("error", discoverErr))
		} else if len(fanartPaths) > 0 && state.BackdropCount < len(fanartPaths) {
			needs.fanart = true
		}
	}
	for _, imageType := range []string{"thumb", "logo", "banner"} {
		if platformHasImageType(state, imageType) {
			continue
		}
		patterns := p.getActiveNamingConfig(ctx, imageType)
		if _, found := img.FindExistingImage(dir, patterns); !found {
			continue
		}
		switch imageType {
		case "thumb":
			needs.thumb = true
		case "logo":
			needs.logo = true
		case "banner":
			needs.banner = true
		}
	}
}

// syncMissingArtwork calls the existing sync methods for each image type
// flagged as needed by detectMissingArtwork.
func (p *Publisher) syncMissingArtwork(ctx context.Context, a *artist.Artist, needs artworkNeeds) {
	if needs.fanart {
		if warnings := p.syncAllFanartToPlatforms(ctx, a, true); len(warnings) > 0 {
			p.logger.Warn("artwork reconciler: fanart sync warnings",
				slog.String("artist_id", a.ID),
				slog.Any("warnings", warnings))
		}
	}
	for _, imageType := range []string{"thumb", "logo", "banner"} {
		var needed bool
		switch imageType {
		case "thumb":
			needed = needs.thumb
		case "logo":
			needed = needs.logo
		case "banner":
			needed = needs.banner
		}
		if !needed {
			continue
		}
		if warnings := p.syncImageToPlatforms(ctx, a, imageType, true); len(warnings) > 0 {
			p.logger.Warn("artwork reconciler: image sync warnings",
				slog.String("artist_id", a.ID),
				slog.String("image_type", imageType),
				slog.Any("warnings", warnings))
		}
	}
}

// ReconcileArtworkToPlatforms iterates all artists that have at least one
// platform mapping and pushes any locally-present artwork that is missing on
// the connected mirror. It is intentionally idempotent: uploading an image
// that already exists on the platform is a no-op on the Emby/Jellyfin side.
//
// Per-artist errors are logged and skipped; the run continues to remaining
// artists. The conflict gate (AllowImageWrite) is checked once per artist
// before any upload; a blocked gate skips that artist silently.
//
// Only connections with FeatureImageWrite=true receive proactive uploads.
func (p *Publisher) ReconcileArtworkToPlatforms(ctx context.Context) {
	if p == nil {
		return
	}

	artistIDs, err := p.artistService.ListArtistsWithPlatformMappings(ctx)
	if err != nil {
		p.logger.Error("artwork reconciler: listing artists with platform mappings",
			slog.Any("error", err))
		return
	}
	if len(artistIDs) == 0 {
		return
	}

	if p.imageWriteGate == nil {
		p.logger.Warn("artwork reconciler: no image write gate wired; conflict ledger will NOT be consulted before uploads")
	}

	p.logger.Info("artwork reconciler: starting run",
		slog.Int("artist_count", len(artistIDs)))

	var (
		checked         int
		synced          int
		skippedGated    int
		skippedNoGetter int
		skippedLoadErr  int
		skippedNoPIDs   int
	)
	for _, artistID := range artistIDs {
		if ctx.Err() != nil {
			break
		}
		checked++

		if p.imageWriteGate != nil {
			if gateErr := p.imageWriteGate.AllowImageWrite(ctx); gateErr != nil {
				p.logger.Debug("artwork reconciler: image write gated, skipping artist",
					slog.String("artist_id", artistID),
					slog.Any("reason", gateErr))
				skippedGated++
				continue
			}
		}

		if p.artistGetter == nil {
			p.logger.Warn("artwork reconciler: no artist getter wired, cannot load artist",
				slog.String("artist_id", artistID))
			skippedNoGetter++
			continue
		}

		a, err := p.artistGetter.GetByID(ctx, artistID)
		if err != nil {
			p.logger.Warn("artwork reconciler: loading artist",
				slog.String("artist_id", artistID),
				slog.Any("error", err))
			skippedLoadErr++
			continue
		}

		dir := p.ImageDir(a)
		if dir == "" {
			continue
		}

		platformIDs, err := p.artistService.GetPlatformIDs(ctx, artistID)
		if err != nil {
			p.logger.Warn("artwork reconciler: getting platform IDs",
				slog.String("artist_id", artistID),
				slog.Any("error", err))
			skippedNoPIDs++
			continue
		}

		needs := p.detectMissingArtwork(ctx, artistID, dir, platformIDs)
		if !needs.any() {
			continue
		}

		synced++
		p.syncMissingArtwork(ctx, a, needs)
	}

	p.logger.Info("artwork reconciler: run complete",
		slog.Int("checked", checked),
		slog.Int("synced", synced),
		slog.Int("skipped_gated", skippedGated),
		slog.Int("skipped_load_err", skippedLoadErr),
		slog.Int("skipped_no_getter", skippedNoGetter),
		slog.Int("skipped_no_platform_ids", skippedNoPIDs))
}

// StartArtworkReconciler runs ReconcileArtworkToPlatforms once at startup
// (after startupDelay) and then on a fixed interval until the context is
// canceled. The ticker follows the same pattern as StartExistsFlagScanner in
// internal/maintenance/maintenance.go.
//
// startupDelay is a parameter so tests can drive it in milliseconds rather
// than waiting the full production delay.
func (p *Publisher) StartArtworkReconciler(ctx context.Context, interval, startupDelay time.Duration) {
	if p == nil {
		return
	}
	p.logger.Info("artwork reconciler started",
		slog.String("interval", interval.String()),
		slog.String("startup_delay", startupDelay.String()))

	select {
	case <-ctx.Done():
		p.logger.Info("artwork reconciler stopped before first run")
		return
	case <-time.After(startupDelay):
	}

	p.runReconcileWithRecover(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("artwork reconciler stopped")
			return
		case <-ticker.C:
			p.runReconcileWithRecover(ctx)
		}
	}
}

// runReconcileWithRecover wraps ReconcileArtworkToPlatforms in a panic
// guard so a bug in the reconciler does not crash the whole process.
func (p *Publisher) runReconcileWithRecover(ctx context.Context) {
	defer func() {
		if v := recover(); v != nil {
			p.logger.Error("artwork reconciler: panic recovered",
				slog.Any("panic", v),
				slog.String("stack", string(debug.Stack())))
		}
	}()
	p.ReconcileArtworkToPlatforms(ctx)
}
