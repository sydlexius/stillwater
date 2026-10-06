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

		// The jellyfin/emby clients both read backdrop bytes; a getter that
		// cannot leaves fanartDeficit on its count-only check.
		reader, _ := stateGetter.(connection.BackdropReader)
		p.accumulateNeeds(ctx, artistID, dir, state, &needs, reader, pid.PlatformArtistID)
	}
	return needs
}

// accumulateNeeds merges per-connection platform state into the shared needs
// struct, checking local file presence for each image type not yet flagged.
// reader (nil allowed) and platformArtistID let the fanart check read the
// platform's backdrop bytes; see fanartDeficit.
func (p *Publisher) accumulateNeeds(
	ctx context.Context,
	artistID, dir string,
	state *connection.ArtistPlatformState,
	needs *artworkNeeds,
	reader connection.BackdropReader,
	platformArtistID string,
) {
	if !needs.fanart {
		needs.fanart = p.fanartDeficit(ctx, artistID, dir, state, reader, platformArtistID)
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

// fanartDeficit reports whether the platform is missing local fanart.
//
// THREE TIERS, cheapest first, each run only when the one before cannot decide:
//
//  1. FILE COUNT. A platform holding at least as many backdrops as there are
//     local files is not short. No read at all, the common case.
//  2. DISTINCT COUNT (#3144). A platform holding fewer backdrops than the local
//     set has distinct PUSHABLE images is short. Reads the local files only.
//  3. IDENTITY (#3147). Left over is a platform whose count sits between the
//     distinct count and the file count, which only a local set with
//     byte-identical duplicates can produce. A count cannot tell its two causes
//     apart: the backdrop prune leaves one copy of EVERY distinct image (local
//     A,B,A, platform A,B: converged), while a Jellyfin resync interrupted after
//     its deletes and some uploads leaves a PREFIX of the local set that can
//     repeat one image and lack another (local A,A,B, platform A,A: short a
//     B). So this tier reads the platform's backdrop bytes and flags a deficit
//     when any readable distinct local image is absent, and the repair
//     (syncAllFanartToPlatforms, which on Jellyfin clears and rebuilds)
//     restores the full ordered set.
//
// WHAT THIS GUARANTEES FOR AN INTERRUPTED RESYNC: it is recovered whenever the
// surviving prefix is missing at least one distinct local image. When the
// prefix already holds every distinct image (local A,B,A, crash after two
// uploads, platform A,B), only a duplicate copy is missing; that is
// byte-for-byte what the prune leaves, so it is treated as a prune and not
// restored, by design. Without a durable in-flight record the two states are
// identical (TestResyncCrash_DistinctCompletePrefixIsTreatedAsPrune).
//
// Tier 3 is a MEMBERSHIP check, not an order or substitution check: a platform
// holding enough backdrops but a different image in place of a local one is
// still not detected (see ReconcileArtworkToPlatforms).
//
// FAIL DIRECTION. A local file the push cannot read (unreadable, or degraded by
// a snapshot cap) does NOT count toward the deficit (#3200): the push could not
// carry it, so re-pushing every pass only churns the peer (on Emby each pass
// replaces the earlier slots in place and leaves a metadata copy; on Jellyfin
// it is refused). The pushable files are classified by snapshotFanart, the
// push's own predicate. A snapshot error (cancel, stalled mount) is no deficit
// this pass, like a platform read failure. A failure to
// read the PLATFORM's bytes in tier 3 goes the other way: no deficit this pass,
// logged, retried next pass. Repairing on that error would rebuild a platform
// the prune had reduced and restore the copies it removed (#3144) every time a
// peer read failed, while declining costs one reconciler interval.
func (p *Publisher) fanartDeficit(
	ctx context.Context,
	artistID, dir string,
	state *connection.ArtistPlatformState,
	reader connection.BackdropReader,
	platformArtistID string,
) bool {
	fanartPaths, discoverErr := img.DiscoverFanart(ctx, dir, p.getActiveFanartPrimary(ctx))
	if discoverErr != nil {
		p.logger.Warn("artwork reconciler: discovering fanart",
			slog.String("artist_id", artistID),
			slog.String("dir", dir),
			slog.Any("error", discoverErr))
		return false
	}
	if len(fanartPaths) == 0 || state.BackdropCount >= len(fanartPaths) {
		return false
	}
	// Classify with the PUSH's own predicate (#3200), so "what the push can
	// carry" and "what the reconciler thinks is missing" cannot drift apart.
	// A nil-data slot is a file the push cannot send (unreadable, or over a
	// snapshot cap); pushing again could never fill it.
	snapshot, _, snapErr := p.snapshotFanart(ctx, fanartPaths)
	if snapErr != nil {
		// A cancel or stalled mount: a push would abort on the same error.
		p.logger.Warn("artwork reconciler: reading local fanart to check for missing fanart; retrying next pass",
			slog.String("artist_id", artistID), slog.Any("error", snapErr))
		return false
	}
	local := make(map[string]bool, len(snapshot))
	unpushable := 0
	for _, sf := range snapshot {
		if sf.data == nil {
			unpushable++
			continue
		}
		local[img.ContentHash(sf.data)] = true
	}
	if state.BackdropCount < len(local) {
		return true
	}
	// reader == nil means the check is count-only. Every platform client
	// implements BackdropReader today, so the stale-replacement edge this leaves
	// (right count, wrong bytes) is unreachable in production.
	if reader == nil {
		return false
	}
	platform, readErr := backdropContentHashes(ctx, reader, platformArtistID, state.BackdropCount)
	if readErr != nil {
		p.logger.Warn("artwork reconciler: reading platform backdrops to check for missing fanart; retrying next pass",
			slog.String("artist_id", artistID),
			slog.Any("error", readErr))
		return false
	}
	held := make(map[string]bool, len(platform))
	for _, h := range platform {
		held[h] = true
	}
	for h := range local {
		if !held[h] {
			return true
		}
	}
	if unpushable > 0 {
		p.logger.Info("artwork reconciler: fanart deficit is explained by local file(s) the push cannot read; not re-pushing",
			slog.String("artist_id", artistID), slog.Int("unpushable", unpushable))
	}
	return false
}

// localFanartHashes returns the set of DISTINCT content hashes (sha256) in the
// local fanart set, plus how many files could not be hashed (read error, over
// the size bound, canceled ctx). len(distinct)+unreadable is the most
// backdrops a platform needs to carry every local image (#3144): an unhashable
// file cannot be proven a duplicate, so it counts as distinct.
func (p *Publisher) localFanartHashes(ctx context.Context, fanartPaths []string) (distinct map[string]bool, unreadable int) {
	distinct = make(map[string]bool, len(fanartPaths))
	for _, fp := range fanartPaths {
		h, err := img.HashFile(ctx, fp, false)
		if err != nil {
			p.logger.Warn("artwork reconciler: hashing local fanart; counting it as distinct",
				slog.String("path", fp), slog.Any("error", err))
			unreadable++
			continue
		}
		distinct[h.Content] = true
	}
	return distinct, unreadable
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
//   - fanart fires only while the platform is missing a DISTINCT local image
//     (fanartDeficit: fewer backdrops than distinct images, or, when the
//     local set holds duplicates, a distinct image absent from the platform's
//     bytes), so a platform the backdrop prune reduced to one copy per image
//     is left alone. The same rule means a resync interrupted after it had
//     already uploaded every distinct image is left one duplicate short, by
//     design: without an in-flight record it is identical to a prune (#3147);
//   - when it fires, Emby replaces in place below its count and appends the
//     rest, and Jellyfin clears and rebuilds the whole list (#3145), so a push
//     that lands in full ends at or above the distinct count and the next
//     pass writes nothing. A failed upload leaves the deficit open and every
//     pass retries it, as before #3144. A local file the push cannot read
//     (unreadable, or over a snapshot cap) is NOT a deficit (#3200): the push
//     could never carry it, so retrying would only churn the peer;
//   - thumb/logo/banner fire only when the platform lacks the image outright.
//
// Two limits, stated so nobody reads more into it. The deficit check does not
// detect a substituted image on a platform holding at least as many backdrops
// as there are local FILES (local A,B against platform A,C): it reads the
// platform's bytes only when the count alone cannot decide (fanartDeficit).
// And
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
