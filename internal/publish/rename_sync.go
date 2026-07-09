package publish

import (
	"context"
	"log/slog"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/connection/lidarr"
)

// renameSyncTimeout caps each per-platform path update so a single slow or
// unresponsive peer cannot stretch the HTTP response unbounded. Chosen to
// match pushTimeout for consistency; rename is rare and operator-driven so
// the absolute wall time matters less than predictable failure semantics.
//
// Declared as a package-level var (not const) so tests can shorten it via
// a t.Cleanup-restored swap and exercise the deadline branch without
// blocking real-time for the full production value.
var renameSyncTimeout = 30 * time.Second

// pathUpdater is the per-connection-type capability surface that
// SyncRename needs. Implemented by emby.Client, jellyfin.Client, and
// lidarr.Client; declaring it locally keeps the artist package free of any
// HTTP dependency while still letting tests substitute fakes via
// renamePathUpdaterFactory.
type pathUpdater interface {
	UpdateArtistPath(ctx context.Context, platformArtistID, newPath string) error
}

// renamePathUpdaterFactory builds a pathUpdater for a given connection. The
// production factory dispatches by conn.Type and is overridden by tests that
// want to inject fakes. Returns (nil, false) for connection types that do
// not support path updates (none currently; reserved for future types like
// Kodi where the API surface is different).
var renamePathUpdaterFactory = func(conn *connection.Connection, logger *slog.Logger) (pathUpdater, bool) {
	switch conn.Type {
	case connection.TypeEmby:
		return emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger), true
	case connection.TypeJellyfin:
		return jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger), true
	case connection.TypeLidarr:
		// VerifyPathAfterUpdate is the per-connection opt-in: when true,
		// UpdateArtistPath issues a follow-up GET and confirms the
		// returned path round-trips. Setting it here is the load-bearing
		// wiring -- without this line the Connection field exists in the
		// DB but never reaches the client, so the verify capability is
		// dead code. Default false on the model means existing rows keep
		// today's single-PUT behavior.
		client := lidarr.New(conn.URL, conn.APIKey, logger)
		client.SetVerifyPathAfterUpdate(conn.GetVerifyPathAfterUpdate())
		return client, true
	default:
		return nil, false
	}
}

// SyncRename re-issues the artist's path on every connected platform after a
// successful Service.RenameDirectory. Implements artist.PlatformRenameSyncer
// so the artist service can call it without importing internal/connection.
//
// Synchronous and best-effort. BOTH enumeration failure (looking up the
// artist's platform_ids rows from the DB) AND per-platform HTTP failures
// are represented as entries in the returned slice with Result ==
// PlatformRemapFailed and Error filled in: the enumeration-failure case
// emits a single synthesized entry with an empty ConnectionID, and each
// per-platform failure emits one entry keyed by its real connection_id.
// A non-nil outer error is reserved for catastrophic failures outside
// per-platform handling (none today; the contract leaves the slot open
// for a future implementation that could fail before producing any entry).
// In short: today this method always returns (results, nil).
//
// oldPath is accepted for symmetry with the rename call and to support
// future audit-log enrichment; it is currently not used by the per-platform
// HTTP calls because every supported platform identifies the artist by its
// platform ID rather than by the prior path.
func (p *Publisher) SyncRename(ctx context.Context, artistID, _ /* oldPath */, newPath string) ([]artist.PlatformRemapResult, error) {
	if p == nil {
		return nil, nil
	}
	platformIDs, err := p.artistService.GetPlatformIDs(ctx, artistID)
	if err != nil {
		p.logger.Error("rename-sync: listing platform IDs",
			slog.String("artist_id", artistID),
			slog.String("error", err.Error()))
		// Surface the enumeration failure in-band so callers (and the HTTP
		// response) carry a concrete signal instead of an empty platforms
		// slice that is indistinguishable from "no mappings". Empty
		// ConnectionID flags the synthesized entry; the Error string names
		// the failed step so operators can triage. Returning nil for the
		// outer error keeps the rename itself reported as successful (the
		// on-disk move already happened) while still telling the user that
		// the post-rename platform reconciliation could not even start.
		return []artist.PlatformRemapResult{{
			ConnectionID: "",
			Result:       artist.PlatformRemapFailed,
			Error:        "platform mappings unavailable: " + truncErr(err),
		}}, nil
	}
	if len(platformIDs) == 0 {
		return nil, nil
	}

	results := make([]artist.PlatformRemapResult, 0, len(platformIDs))
	for _, pid := range platformIDs {
		results = append(results, p.syncOne(ctx, artistID, newPath, pid))
	}
	return results, nil
}

// syncOne handles one connection. Split out so the per-platform try/log
// boilerplate is not nested four indents deep in the loop. The slice append
// in the caller drives the partial-failure semantics: every call site here
// returns a populated PlatformRemapResult, never a panic or short-circuit.
func (p *Publisher) syncOne(ctx context.Context, artistID, newPath string, pid artist.PlatformID) artist.PlatformRemapResult {
	res := artist.PlatformRemapResult{
		ConnectionID: pid.ConnectionID,
		Result:       artist.PlatformRemapFailed,
	}

	// Per-platform deadline. The caller (Service.RenameDirectory) passes a
	// context detached from the originating HTTP request via
	// context.WithoutCancel, so this WithTimeout is a fixed 30s bound that
	// does NOT shorten if the client disconnects mid-rename. That matches
	// the intent: the on-disk rename has already committed, so reaching
	// every platform with the new path is more important than honoring
	// request cancellation. PushLocks uses the same WithoutCancel pattern
	// at the publisher.go call site for the same reason.
	callCtx, cancel := context.WithTimeout(ctx, renameSyncTimeout)
	defer cancel()

	conn, connErr := p.connectionService.GetByID(callCtx, pid.ConnectionID)
	if connErr != nil {
		res.Error = "fetching connection: " + connErr.Error()
		p.logger.Error("rename-sync: fetching connection",
			slog.String("artist_id", artistID),
			slog.String("connection_id", pid.ConnectionID),
			slog.String("error", connErr.Error()))
		return res
	}
	if !conn.Enabled {
		// A disabled connection counts as "ok" because there is nothing to
		// remap: the operator has opted the platform out of sync, and the
		// rename should not surface a noisy failure for that legitimate
		// state. Same reasoning PushLocks applies to disabled connections.
		// Leave Error empty so the JSON response honors the OpenAPI contract
		// that `error` is "present only when result is failed"; the skip
		// reason is captured at Debug for operator log tailing.
		res.Result = artist.PlatformRemapOK
		p.logger.Debug("rename-sync: skipping disabled connection",
			slog.String("artist_id", artistID),
			slog.String("connection", conn.Name))
		return res
	}

	updater, ok := renamePathUpdaterFactory(conn, p.logger)
	if !ok {
		// Unknown connection type. Record as failed so the gap is visible
		// to the caller; this is a code-level miss, not a runtime peer
		// issue, and silently OK'ing it would hide a future-type regression.
		res.Error = "connection type does not support path update: " + conn.Type
		p.logger.Warn("rename-sync: unsupported connection type",
			slog.String("artist_id", artistID),
			slog.String("connection", conn.Name),
			slog.String("type", conn.Type))
		return res
	}

	// Translate the host artist path into the platform's namespace before the
	// PUT. For a shared-mount deployment (or any non-Lidarr peer) MapArtistPath
	// returns newPath unchanged, so this is a no-op; for a split-mount Lidarr
	// with configured PathMappings it rewrites the prefix so Lidarr receives a
	// path it can resolve instead of one it rejects or silently coerces against
	// its Root Folder list. This covers both rename and the merge propagation
	// #2303 routes through the same UpdateArtistPath chokepoint.
	platformPath := conn.MapArtistPath(newPath)
	if err := updater.UpdateArtistPath(callCtx, pid.PlatformArtistID, platformPath); err != nil {
		// Bound the surfaced error: Jellyfin's postFullItem can wrap up to
		// 1 MB of peer response body into the returned error, and the full
		// string flows into the JSON response and into operator log lines.
		// 256 bytes keeps the diagnostic useful without flooding either.
		res.Error = truncErr(err)
		// Truncate the log error too: Jellyfin's postFullItem can wrap up
		// to 1 MB of peer response body into err, and the raw string would
		// otherwise still flood operator logs even though res.Error is
		// bounded. truncErr keeps both surfaces in lockstep.
		p.logger.Error("rename-sync: path update failed",
			slog.String("artist_id", artistID),
			slog.String("connection", conn.Name),
			slog.String("type", conn.Type),
			slog.String("error", truncErr(err)))
		return res
	}

	res.Result = artist.PlatformRemapOK
	p.logger.Info("rename-sync: path updated",
		slog.String("artist_id", artistID),
		slog.String("connection", conn.Name),
		slog.String("type", conn.Type),
		slog.String("new_path", newPath))
	return res
}

// truncErr renders an error's message and caps it at 256 bytes. The cap
// guards against per-client wrappers (notably jellyfin.postFullItem) that
// include the full peer response body in the returned error: that string
// flows into res.Error which becomes both the JSON response payload and
// the slog attribute, so an unbounded body can blow up either consumer.
func truncErr(err error) string {
	const maxLen = 256
	s := err.Error()
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
