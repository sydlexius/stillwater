package publish

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/connection/lidarr"
)

// mergeRefreshTimeout caps each per-connection refresh call. A library scan
// returns as soon as the server accepts the job, so this only guards a slow or
// unresponsive peer. Matches renameSyncTimeout for consistency.
var mergeRefreshTimeout = 30 * time.Second

// mergeRefresher is the per-connection capability SyncMergeRefresh needs.
// survivorPlatformID is the survivor's platform artist ID on THIS connection,
// or "" when the survivor is not mapped here (e.g. a connection that only
// mapped a loser). loserPlatformIDs are the platform IDs the LOSERS held on
// this connection (captured before their DB rows -- and platform_ids -- were
// deleted). Implementations that need the survivor ID (Lidarr) no-op on "".
type mergeRefresher interface {
	RefreshAfterMerge(ctx context.Context, survivorPlatformID string, loserPlatformIDs []string) error
}

// itemRescanner is the scoped-refresh primitive Emby and Jellyfin share:
// re-validate a single item (and its children, if a folder) without forcing
// a metadata/image replace. embyRefresher and jellyfinRefresher both use it
// to scope post-merge reconciliation instead of a full library scan.
type itemRescanner interface {
	TriggerItemRescan(ctx context.Context, itemID string) error
	TriggerLibraryScan(ctx context.Context) error
}

// scopedMergeRefresh implements the Emby/Jellyfin RefreshAfterMerge contract
// against any itemRescanner: rescan the survivor's item (recursive, so newly
// absorbed album subfolders are picked up) and each known loser item (so the
// peer notices the now-missing directory and evicts it). Falls back to a full
// library scan when the survivor is unmapped on this connection (nothing to
// scope the index-side refresh to) or when the survivor rescan itself fails
// (the peer or network is unhealthy enough that scoping is not trustworthy).
// A loser rescan failure is logged by the caller via the returned error only
// when it is the sole failure; today it is folded into the overall error so
// a partial eviction still surfaces as a per-connection failure rather than a
// silent miss.
func scopedMergeRefresh(ctx context.Context, c itemRescanner, survivorPlatformID string, loserPlatformIDs []string) error {
	if survivorPlatformID == "" {
		return c.TriggerLibraryScan(ctx)
	}
	if err := c.TriggerItemRescan(ctx, survivorPlatformID); err != nil {
		return c.TriggerLibraryScan(ctx)
	}
	var loserErrs []error
	for _, id := range loserPlatformIDs {
		if err := c.TriggerItemRescan(ctx, id); err != nil {
			loserErrs = append(loserErrs, fmt.Errorf("loser item %s: %w", id, err))
		}
	}
	if len(loserErrs) > 0 {
		return fmt.Errorf("scoped rescan of %d loser item(s) failed: %w", len(loserErrs), errors.Join(loserErrs...))
	}
	return nil
}

type embyRefresher struct{ c *emby.Client }

func (r embyRefresher) RefreshAfterMerge(ctx context.Context, survivorPlatformID string, loserPlatformIDs []string) error {
	return scopedMergeRefresh(ctx, r.c, survivorPlatformID, loserPlatformIDs)
}

type jellyfinRefresher struct{ c *jellyfin.Client }

func (r jellyfinRefresher) RefreshAfterMerge(ctx context.Context, survivorPlatformID string, loserPlatformIDs []string) error {
	return scopedMergeRefresh(ctx, r.c, survivorPlatformID, loserPlatformIDs)
}

type lidarrRefresher struct{ c *lidarr.Client }

func (r lidarrRefresher) RefreshAfterMerge(ctx context.Context, survivorPlatformID string, _ []string) error {
	// Lidarr has no server-wide library-scan primitive; refresh the survivor
	// so it re-reads its (now larger) folder. If the survivor is not mapped on
	// this Lidarr connection there is nothing to refresh -> no-op OK.
	if survivorPlatformID == "" {
		return nil
	}
	id, err := strconv.Atoi(survivorPlatformID)
	if err != nil {
		return fmt.Errorf("lidarr survivor id %q not numeric: %w", survivorPlatformID, err)
	}
	_, err = r.c.TriggerArtistRefresh(ctx, id)
	return err
}

// mergeRefresherFactory builds a mergeRefresher for a connection. Overridable
// by tests. Returns (nil, false) for types without a refresh primitive.
var mergeRefresherFactory = func(conn *connection.Connection, logger *slog.Logger) (mergeRefresher, bool) {
	switch conn.Type {
	case connection.TypeEmby:
		return embyRefresher{emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)}, true
	case connection.TypeJellyfin:
		return jellyfinRefresher{jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)}, true
	case connection.TypeLidarr:
		return lidarrRefresher{lidarr.New(conn.URL, conn.APIKey, logger)}, true
	default:
		return nil, false
	}
}

// SyncMergeRefresh reconciles connected platforms after a merge. Implements
// artist.PlatformMergeRefresher. Synchronous, best-effort: per-connection
// failures land in the returned slice with Result == PlatformRemapFailed; the
// outer error is always nil today.
//
// The refresh is not symmetric across platforms: Emby/Jellyfin scope a
// rescan to the survivor item (re-indexing absorbed albums) and each known
// loser item on that connection (evicting the stale item whose directory was
// removed), falling back to a full library scan only when the survivor is
// unmapped there or the scoped call fails; Lidarr only re-reads the survivor
// via TriggerArtistRefresh -- it has no server-wide scan primitive, so the
// loser's Lidarr artist lingers pointing at the deleted folder and must be
// cleaned up on the Lidarr side manually (broader eviction deferred to #2318).
func (p *Publisher) SyncMergeRefresh(ctx context.Context, survivorID string, connectionIDs []string, loserPlatformIDs map[string][]string) ([]artist.PlatformRefreshResult, error) {
	// Only the nil-publisher guard remains: a fully-unlinked merge (empty
	// connectionIDs) must still reach self-heal, since the whole point is to
	// discover a Lidarr link that no pre-delete AffectedConnectionIDs entry
	// captured. The refresh loop below is a no-op when connectionIDs stays
	// empty after self-heal.
	if p == nil {
		return nil, nil
	}
	// Resolve the survivor's platform IDs once, keyed by connection, so Lidarr
	// (and any future per-artist primitive) can find the survivor's ID. A
	// lookup failure is non-fatal: connections where the survivor is unmapped
	// (Emby/Jellyfin library scan) still reconcile; only Lidarr's per-artist
	// refresh needs the ID and no-ops without it.
	survivorByConn := map[string]string{}
	if pids, err := p.artistService.GetPlatformIDs(ctx, survivorID); err != nil {
		p.logger.Warn("merge-refresh: listing survivor platform IDs",
			slog.String("artist_id", survivorID), slog.String("error", err.Error()))
	} else {
		for _, pid := range pids {
			survivorByConn[pid.ConnectionID] = pid.PlatformArtistID
		}
	}

	// Self-heal: a Lidarr connection linked only after the merge began (or never
	// linked to the survivor) is absent from BOTH survivorByConn AND the
	// pre-delete AffectedConnectionIDs. Resolve-by-MBID and union each freshly
	// linked connection into both, so refreshOne can find the survivor's numeric
	// ID and the loop actually visits the connection. Best-effort: never errors.
	alreadyLinked := make(map[string]bool, len(survivorByConn))
	for connID := range survivorByConn {
		alreadyLinked[connID] = true
	}
	// Detach from the originating HTTP request (WithoutCancel), mirroring
	// refreshOne's post-commit best-effort context handling: the merge has
	// already committed, so a client disconnect must not cancel this self-heal.
	healCtx := context.WithoutCancel(ctx)
	if mbid := p.mbidFor(healCtx, survivorID); mbid != "" {
		inConnIDs := make(map[string]bool, len(connectionIDs))
		for _, cid := range connectionIDs {
			inConnIDs[cid] = true
		}
		for connID, platformArtistID := range p.selfHealLidarrLinks(healCtx, survivorID, mbid, alreadyLinked) {
			survivorByConn[connID] = platformArtistID
			if !inConnIDs[connID] {
				connectionIDs = append(connectionIDs, connID)
				inConnIDs[connID] = true
			}
		}
	}

	// Nothing to reconcile: neither an affected connection nor a self-healed
	// Lidarr link. Return nil (not an empty slice) to preserve the prior
	// "no connections" contract for callers that distinguish the two.
	if len(connectionIDs) == 0 {
		return nil, nil
	}
	results := make([]artist.PlatformRefreshResult, 0, len(connectionIDs))
	for _, cid := range connectionIDs {
		results = append(results, p.refreshOne(ctx, survivorID, cid, survivorByConn[cid], loserPlatformIDs[cid]))
	}
	return results, nil
}

func (p *Publisher) refreshOne(ctx context.Context, survivorID, connID, survivorPlatformID string, loserPlatformIDs []string) artist.PlatformRefreshResult {
	res := artist.PlatformRefreshResult{ConnectionID: connID, Result: artist.PlatformRemapFailed}
	// Per-connection deadline on a context detached from the originating HTTP
	// request (WithoutCancel): the merge has already committed on disk, so
	// reaching every platform matters more than honoring request cancellation.
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mergeRefreshTimeout)
	defer cancel()

	conn, err := p.connectionService.GetByID(callCtx, connID)
	if err != nil {
		res.Error = "fetching connection: " + truncErr(err)
		p.logger.Error("merge-refresh: fetching connection",
			slog.String("connection_id", connID), slog.String("error", err.Error()))
		return res
	}
	if !conn.Enabled {
		// Opted out: nothing to do. Counts as ok so the operator's Enabled=false
		// choice does not surface a noisy failure. Error stays empty per the
		// OpenAPI contract that error is present only when result is failed.
		res.Result = artist.PlatformRemapOK
		p.logger.Debug("merge-refresh: skipping disabled connection", slog.String("connection", conn.Name))
		return res
	}
	refresher, ok := mergeRefresherFactory(conn, p.logger)
	if !ok {
		res.Error = "connection type does not support refresh: " + conn.Type
		p.logger.Warn("merge-refresh: unsupported connection type",
			slog.String("connection", conn.Name), slog.String("type", conn.Type))
		return res
	}
	if err := refresher.RefreshAfterMerge(callCtx, survivorPlatformID, loserPlatformIDs); err != nil {
		res.Error = truncErr(err)
		p.logger.Error("merge-refresh: refresh failed",
			slog.String("connection", conn.Name), slog.String("type", conn.Type),
			slog.String("error", truncErr(err)))
		return res
	}
	res.Result = artist.PlatformRemapOK
	p.logger.Info("merge-refresh: platform refreshed",
		slog.String("survivor_id", survivorID), slog.String("connection", conn.Name), slog.String("type", conn.Type))
	return res
}
