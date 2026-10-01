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
		if conn == nil {
			p.logger.Warn("artwork reconciler: missing connection",
				slog.String("artist_id", artistID),
				slog.String("connection_id", pid.ConnectionID))
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
		fanartPaths, discoverErr := img.DiscoverFanart(ctx, dir, primary)
		if discoverErr != nil {
			p.logger.Warn("artwork reconciler: discovering fanart",
				slog.String("artist_id", artistID),
				slog.String("dir", dir),
				slog.Any("error", discoverErr))
		} else if len(fanartPaths) > 0 && state.BackdropCount < len(fanartPaths) &&
			state.BackdropCount < p.distinctLocalFanart(ctx, fanartPaths) {
			// The file-count check is a cheap pre-filter; only an artist that
			// passes it pays for reading its local set. See distinctLocalFanart.
			needs.fanart = true
		}
	}
	for _, imageType := range []string{"thumb", "logo", "banner"} {
		if platformHasImageType(state, imageType) {
			continue
		}
		patterns := p.getActiveNamingConfig(ctx, imageType)
		if _, found := img.FindExistingImage(ctx, dir, patterns); !found {
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

// distinctLocalFanart returns how many DISTINCT images (by sha256) the local
// fanart set holds, which is the most backdrops a platform needs to carry
// every local image (#3144).
//
// The reconciler's deficit check used to compare the platform's count against
// the local FILE count. The platform backdrop prune (backdrop_prune.go) leaves
// one copy of each byte-identical image, so on an artist whose local set holds
// a duplicate the prune necessarily ends below the file count -- and the next
// reconciler pass read that as damage and re-pushed the whole set, restoring
// the copies the prune had just removed. Comparing against distinct content
// makes the two agree: a pruned platform holding every distinct local image is
// not a deficit, while a platform holding FEWER backdrops than there are
// distinct local images still is. It is a COUNT, not an identity check: a
// platform holding enough backdrops but a different image in place of a local
// one is not detected (see ReconcileArtworkToPlatforms).
//
// A file that cannot be hashed (read error, over the size bound, canceled ctx)
// counts as DISTINCT. It cannot be proven a duplicate, and overcounting only
// errs toward the reconciler's historical behavior (repair), never toward
// leaving a real deficit unrepaired.
func (p *Publisher) distinctLocalFanart(ctx context.Context, fanartPaths []string) int {
	seen := make(map[string]bool, len(fanartPaths))
	distinct := 0
	for _, fp := range fanartPaths {
		h, err := img.HashFile(ctx, fp, false)
		if err != nil {
			p.logger.Warn("artwork reconciler: hashing local fanart; counting it as distinct",
				slog.String("path", fp), slog.Any("error", err))
			distinct++
			continue
		}
		if !seen[h.Content] {
			seen[h.Content] = true
			distinct++
		}
	}
	return distinct
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
// the connected mirror.
//
// IDEMPOTENT AT THE PASS LEVEL, NOT THE UPLOAD LEVEL (#3144). An individual
// upload is not a no-op on either peer: Jellyfin appends whatever the index
// (#3135), and Emby appends at any index past its current count. What makes a
// repeated pass harmless is the trigger plus the full-set push it runs:
//
//   - fanart fires only while the platform holds FEWER backdrops than the
//     local set has DISTINCT images (distinctLocalFanart), so a platform the
//     backdrop prune reduced to one copy per image is left alone;
//   - when it fires, Emby replaces in place below its count and appends the
//     rest, and Jellyfin clears and rebuilds the whole list (#3145), so a push
//     that lands in full ends at or above the distinct count and the next
//     pass writes nothing. A push that does NOT land in full (an unreadable
//     local file, a failed upload) leaves the deficit open, and every pass
//     retries it, as before #3144;
//   - thumb/logo/banner fire only when the platform lacks the image outright.
//
// Two limits, stated so nobody reads more into it. The deficit check is
// count-based and does not detect a substituted image: it reads only the
// platform's BackdropCount, never its bytes, so a platform holding as many
// backdrops as the local set has distinct images, but a different image in
// place of a local one (local A,B,A against platform A,C), is not repaired.
// That was equally true of the file-count check before #3144 (local A,B
// against platform A,C). And
// every Emby push leaves a copy in Emby's own metadata store (#3151), so a
// pass that does fire is not free on Emby even though it converges.
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
	if interval <= 0 {
		p.logger.Warn("artwork reconciler: non-positive interval; reconciler not started",
			slog.String("interval", interval.String()))
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
