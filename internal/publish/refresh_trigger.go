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
)

// artistRefreshTimeout caps each per-connection full-reimport refresh call. The
// server returns as soon as it accepts the refresh job, so this only guards a
// slow or unresponsive peer. Matches pushTimeout for consistency.
var artistRefreshTimeout = 30 * time.Second

// artistRefresher is the per-connection capability RefreshArtistOnPlatforms
// needs: force the platform to re-import the artist's on-disk NFO. Kept as a
// package-private structural interface (mirroring merge_refresh.go's
// mergeRefresher) so tests can swap the factory without standing up real peers.
type artistRefresher interface {
	RefreshArtist(ctx context.Context, platformArtistID string) error
}

type embyArtistRefresher struct{ c *emby.Client }

func (r embyArtistRefresher) RefreshArtist(ctx context.Context, platformArtistID string) error {
	return r.c.TriggerArtistRefresh(ctx, platformArtistID)
}

type jellyfinArtistRefresher struct{ c *jellyfin.Client }

func (r jellyfinArtistRefresher) RefreshArtist(ctx context.Context, platformArtistID string) error {
	return r.c.TriggerArtistRefresh(ctx, platformArtistID)
}

// artistRefresherFactory builds an artistRefresher for a connection. Overridable
// by tests (mirrors mergeRefresherFactory). Returns (nil, false) for types
// without a per-artist NFO re-import primitive: Lidarr does not consume NFO the
// same way and is out of scope for #2336.
var artistRefresherFactory = func(conn *connection.Connection, logger *slog.Logger) (artistRefresher, bool) {
	switch conn.Type {
	case connection.TypeEmby:
		return embyArtistRefresher{emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)}, true
	case connection.TypeJellyfin:
		return jellyfinArtistRefresher{jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)}, true
	default:
		return nil, false
	}
}

// RefreshArtistOnPlatforms tells opted-in Emby/Jellyfin connections to re-import
// the artist's on-disk NFO after Stillwater has rewritten it (#2336). This is the
// channel through which NFO-only fields -- Disambiguation and YearsActive, which
// have no Emby/Jellyfin BaseItemDto field and are therefore dropped by the
// metadata API push -- actually reach the platform: the server re-reads the NFO,
// which already carries both fields correctly.
//
// Gated on conn.GetFeatureTriggerRefresh(): the underlying refresh is a
// destructive full re-import (ReplaceAllMetadata=true) that replaces platform-side
// metadata from the NFO, so it must only fire when the operator has opted in.
//
// Fire-and-forget per connection, mirroring PushMetadataAsync: each goroutine
// detaches from the originating request context (context.WithoutCancel) with its
// own timeout, and logs failures only (best-effort, matching the package
// contract that the primary DB write has already succeeded).
//
// Duplicate-refresh resolution (#2336, plan Phase 2 Task 2): the emby push path's
// fire-and-forget refreshItem is intentionally left NON-destructive
// (ReplaceAllMetadata=false, no MetadataRefreshMode) -- it persists Emby's own
// metadata back to the NFO after an API push and always runs, so it must not
// become a destructive re-import. The destructive NFO->platform re-import lives
// solely here, behind the opt-in gate, so an opted-in connection is refreshed
// destructively exactly once per publish.
func (p *Publisher) RefreshArtistOnPlatforms(ctx context.Context, a *artist.Artist) {
	if p == nil {
		return
	}
	platformIDs, err := p.artistService.GetPlatformIDs(ctx, a.ID)
	if err != nil {
		p.logger.Error("refresh-trigger: listing platform IDs",
			slog.String("artist_id", a.ID),
			slog.String("error", err.Error()))
		return
	}
	if len(platformIDs) == 0 {
		return
	}

	for _, pid := range platformIDs {
		go func() {
			gCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), artistRefreshTimeout)
			defer cancel()
			defer func() {
				if v := recover(); v != nil {
					p.logger.Error("refresh-trigger: panic in goroutine",
						slog.String("artist_id", a.ID),
						slog.String("connection_id", pid.ConnectionID),
						slog.Any("panic", v),
						slog.String("stack", string(debug.Stack())))
				}
			}()

			conn, connErr := p.connectionService.GetByID(gCtx, pid.ConnectionID)
			if connErr != nil {
				p.logger.Error("refresh-trigger: fetching connection",
					slog.String("artist_id", a.ID),
					slog.String("connection_id", pid.ConnectionID),
					slog.String("error", connErr.Error()))
				return
			}
			// Skip connections the operator disabled or that are not healthy, and
			// connections that did not opt into the destructive re-import.
			if !conn.Enabled || conn.Status != "ok" {
				return
			}
			if !conn.GetFeatureTriggerRefresh() {
				p.logger.Debug("refresh-trigger: skipping connection without trigger-refresh opt-in",
					slog.String("artist_id", a.ID),
					slog.String("connection", conn.Name))
				return
			}

			refresher, ok := artistRefresherFactory(conn, p.logger)
			if !ok {
				// Unsupported type (e.g. Lidarr): nothing to re-import.
				return
			}
			if refreshErr := refresher.RefreshArtist(gCtx, pid.PlatformArtistID); refreshErr != nil {
				p.logger.Error("refresh-trigger: NFO re-import failed",
					slog.String("artist_id", a.ID),
					slog.String("artist_name", a.Name),
					slog.String("connection", conn.Name),
					slog.String("error", refreshErr.Error()))
			} else {
				p.logger.Info("refresh-trigger: NFO re-import triggered",
					slog.String("artist_id", a.ID),
					slog.String("artist_name", a.Name),
					slog.String("connection", conn.Name))
			}
		}()
	}
}
