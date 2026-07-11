package publish

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/lidarr"
)

// lidarrArtistLister is the narrow read surface self-heal needs from a Lidarr
// connection: enumerate its artists so we can match one to a Stillwater artist
// by MusicBrainz ID. Implemented by *lidarr.Client; declared locally so the
// factory can be swapped in tests without standing up a real HTTP fixture.
type lidarrArtistLister interface {
	GetArtists(ctx context.Context) ([]lidarr.Artist, error)
}

// lidarrArtistListerFactory builds a lidarrArtistLister for a Lidarr
// connection. The production factory returns a real *lidarr.Client; tests
// override it (t.Cleanup-restored) to inject a fake, mirroring the
// renamePathUpdaterFactory / mergeRefresherFactory seams already in this
// package. Declared as a package-level var (not const) purely so tests can
// swap it.
var lidarrArtistListerFactory = func(conn *connection.Connection, logger *slog.Logger) lidarrArtistLister {
	return lidarr.New(conn.URL, conn.APIKey, logger)
}

// mbidFor loads the artist's MusicBrainz ID, the key self-heal matches on.
// Returns "" (no heal) when the getter is unwired, the load fails, or the
// artist carries no MBID. A load failure is logged, not propagated: self-heal
// is best-effort and must never fail the merge/rename it augments.
func (p *Publisher) mbidFor(ctx context.Context, artistID string) string {
	if p == nil || p.artistGetter == nil {
		return ""
	}
	a, err := p.artistGetter.GetByID(ctx, artistID)
	if err != nil {
		p.logger.Warn("self-heal: loading artist for MBID; skipping heal",
			slog.String("artist_id", artistID),
			slog.String("error", err.Error()))
		return ""
	}
	if a == nil {
		return ""
	}
	// Trim so a stored MBID with stray whitespace still matches a clean Lidarr
	// ForeignArtistID (the compare side is trimmed too).
	return strings.TrimSpace(a.MusicBrainzID)
}

// selfHealLidarrLinks resolves-by-MBID any enabled Lidarr connections that are
// not yet linked to the given artist, stamping the discovered numeric platform
// ID via SetPlatformID. It returns a map of connection ID -> numeric platform
// ID for the connections it newly linked (empty when it linked none).
//
// This closes the propagation gap in MergeAndReconcile: both chokepoints
// (SyncRename via GetPlatformIDs; SyncMergeRefresh via survivorByConn) are
// keyed on EXISTING artist_platform_ids rows, and the normal operator journey
// has zero Lidarr links, so path/refresh propagation silently no-ops for
// Lidarr. Resolving the link by MBID at merge/rename time lets the new path
// reach Lidarr through the existing UpdateArtistPath / RefreshAfterMerge chain.
//
// BEST-EFFORT is the hard invariant: EVERY failure (connection list error,
// per-connection GetArtists error, SetPlatformID error, or simply no MBID
// match) is logged and skipped. This method never returns an error and must
// never fail the merge/rename it augments; it returns only what it managed to
// link.
//
// Matching is MBID-equality ONLY (case-insensitive on ForeignArtistID). It
// deliberately does NOT reuse dedupeForImport, whose name-fallback could
// link a different artist onto the wrong Lidarr record.
//
// Gating is on conn.Enabled ONLY, matching the existing Lidarr path-sync /
// merge-refresh gate: stamping a link plus its path is not a "manage server
// files" write, so feature_manage_server_files is intentionally not consulted.
func (p *Publisher) selfHealLidarrLinks(ctx context.Context, artistID, mbid string, alreadyLinked map[string]bool) map[string]string {
	linked := map[string]string{}
	// Trim defensively so a caller passing a raw (untrimmed) MBID still matches;
	// mbidFor already trims, but selfHealLidarrLinks is also called directly.
	mbid = strings.TrimSpace(mbid)
	if p == nil || mbid == "" {
		return linked
	}

	conns, err := p.connectionService.ListByType(ctx, connection.TypeLidarr)
	if err != nil {
		p.logger.Warn("self-heal: listing Lidarr connections; skipping heal",
			slog.String("artist_id", artistID),
			slog.String("error", err.Error()))
		return linked
	}

	for i := range conns {
		conn := &conns[i]
		if !conn.Enabled {
			continue
		}
		// Idempotency: an already-linked connection needs no heal, and skipping
		// it here avoids a wasted GetArtists round-trip.
		if alreadyLinked[conn.ID] {
			continue
		}

		lister := lidarrArtistListerFactory(conn, p.logger)
		if lister == nil {
			continue
		}
		artists, listErr := lister.GetArtists(ctx)
		if listErr != nil {
			p.logger.Warn("self-heal: fetching Lidarr artists; skipping connection",
				slog.String("artist_id", artistID),
				slog.String("connection", conn.Name),
				slog.String("error", listErr.Error()))
			continue
		}

		matchedID := ""
		for _, la := range artists {
			// First match wins. A single Lidarr instance normally enforces
			// ForeignArtistID uniqueness, so a duplicate is a data anomaly; the
			// stamped ID is then order-dependent but the blast radius is one
			// wrongly-picked (still same-artist) Lidarr record.
			if strings.EqualFold(strings.TrimSpace(la.ForeignArtistID), mbid) {
				matchedID = strconv.Itoa(la.ID)
				break
			}
		}
		if matchedID == "" {
			p.logger.Debug("self-heal: no MBID match on Lidarr connection",
				slog.String("artist_id", artistID),
				slog.String("connection", conn.Name))
			continue
		}

		// Self-heal is a non-authoritative writer, so route through the stable
		// set: keep the deterministic (lowest-id) mapping rather than clobber an
		// existing divergent id (#2344). Best-effort posture is unchanged -- a
		// DB error skips this connection and is never propagated.
		outcome, setErr := p.artistService.SetPlatformIDStable(ctx, artistID, conn.ID, matchedID)
		if setErr != nil {
			p.logger.Warn("self-heal: stamping Lidarr platform ID; skipping connection",
				slog.String("artist_id", artistID),
				slog.String("connection", conn.Name),
				slog.String("platform_artist_id", matchedID),
				slog.String("error", setErr.Error()))
			continue
		}
		if outcome.Diverged {
			p.logger.Info("self-heal: resolved a divergent Lidarr platform ID; kept deterministic pick",
				slog.String("artist_id", artistID),
				slog.String("connection", conn.Name),
				slog.String("kept_platform_artist_id", outcome.StoredID),
				slog.String("previous_platform_artist_id", outcome.PreviousID),
				slog.String("incoming_platform_artist_id", matchedID))
		}

		linked[conn.ID] = matchedID
		p.logger.Info("self-heal: linked Lidarr artist by MBID",
			slog.String("artist_id", artistID),
			slog.String("connection", conn.Name),
			slog.String("platform_artist_id", matchedID))
	}

	return linked
}
