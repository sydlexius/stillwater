package publish

import (
	"context"
	"fmt"
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

// rootLister is the per-connection-type capability the pre-flight root guard
// needs: enumerate the peer's OWN root folders / library locations, in the
// peer's filesystem namespace. Lidarr answers from /api/v1/rootfolder;
// Emby/Jellyfin from their music VirtualFolders' Locations.
type rootLister interface {
	ListRoots(ctx context.Context) ([]string, error)
}

type lidarrRootLister struct{ c *lidarr.Client }

func (l lidarrRootLister) ListRoots(ctx context.Context) ([]string, error) {
	folders, err := l.c.GetRootFolders(ctx)
	if err != nil {
		return nil, err
	}
	roots := make([]string, 0, len(folders))
	for _, f := range folders {
		if f.Path != "" {
			roots = append(roots, f.Path)
		}
	}
	return roots, nil
}

type embyRootLister struct{ c *emby.Client }

func (l embyRootLister) ListRoots(ctx context.Context) ([]string, error) {
	libs, err := l.c.GetMusicLibraries(ctx)
	if err != nil {
		return nil, err
	}
	var roots []string
	// Index-based: VirtualFolder is large enough to trip gocritic's rangeValCopy.
	for i := range libs {
		roots = append(roots, libs[i].Locations...)
	}
	return roots, nil
}

type jellyfinRootLister struct{ c *jellyfin.Client }

func (l jellyfinRootLister) ListRoots(ctx context.Context) ([]string, error) {
	libs, err := l.c.GetMusicLibraries(ctx)
	if err != nil {
		return nil, err
	}
	var roots []string
	// Index-based: VirtualFolder is large enough to trip gocritic's rangeValCopy.
	for i := range libs {
		roots = append(roots, libs[i].Locations...)
	}
	return roots, nil
}

// renameRootListerFactory builds a rootLister for a connection. Overridable by
// tests (t.Cleanup-restored). Returns (nil, false) for a type with no root
// surface -- which the guard treats as UNVERIFIABLE and therefore refuses, since
// a type we cannot check is exactly the case where a bad path would slip through
// reporting ok.
var renameRootListerFactory = func(conn *connection.Connection, logger *slog.Logger) (rootLister, bool) {
	switch conn.Type {
	case connection.TypeEmby:
		return embyRootLister{emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)}, true
	case connection.TypeJellyfin:
		return jellyfinRootLister{jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)}, true
	case connection.TypeLidarr:
		return lidarrRootLister{lidarr.New(conn.URL, conn.APIKey, logger)}, true
	default:
		return nil, false
	}
}

// guardPlatformPath is the pre-flight root guard: THE chokepoint that makes a
// wrongly mapped path impossible to push. It runs after MapArtistPath and before
// UpdateArtistPath, and returns a non-nil error (wrapping
// connection.ErrPathOutsideRoots) when platformPath does not resolve inside any
// of the peer's own roots.
//
// FAIL-CLOSED, deliberately. An unreachable root endpoint, an auth failure, an
// unsupported connection type, or a peer that reports zero roots ALL refuse the
// push rather than attempt it blind. The bug this guards (#2380) was precisely a
// wrong path that peers accepted while reporting success, so "could not verify"
// must never degrade into "send it anyway": the operator sees a loud, actionable
// failure with a remedy instead of silent corruption they discover weeks later
// as duplicate artists. The refusal is per-connection and non-fatal to the
// rename (which has already committed on disk) - it surfaces as one
// PlatformRemapFailed entry, exactly like an HTTP failure.
func (p *Publisher) guardPlatformPath(ctx context.Context, conn *connection.Connection, hostPath, platformPath string) error {
	lister, ok := renameRootListerFactory(conn, p.logger)
	if !ok {
		return fmt.Errorf("%w: connection type %s exposes no root-folder list, so %q cannot be verified",
			connection.ErrPathOutsideRoots, conn.Type, platformPath)
	}
	roots, err := lister.ListRoots(ctx)
	if err != nil {
		return fmt.Errorf("%w: could not read %s root folders to verify %q: %s",
			connection.ErrPathOutsideRoots, conn.Type, platformPath, truncErr(err))
	}
	if connection.PathWithinRoots(platformPath, roots) {
		return nil
	}
	return fmt.Errorf("%w: %s", connection.ErrPathOutsideRoots,
		connection.RemedyForOutsideRoots(conn.Name, conn.Type, hostPath, platformPath, roots))
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
	// Self-heal: resolve-by-MBID any enabled Lidarr connections not yet linked
	// to this artist and append them so the rename's new path reaches Lidarr via
	// the same MapArtistPath -> UpdateArtistPath chain below. The normal
	// operator journey has zero Lidarr platform_ids rows, so without this the
	// loop would never touch Lidarr. Best-effort: selfHealLidarrLinks never
	// errors, so a heal failure cannot fail the rename. Covers standalone rename
	// as well as the merge path that routes through here.
	linked := make(map[string]bool, len(platformIDs))
	for _, pid := range platformIDs {
		linked[pid.ConnectionID] = true
	}
	// Detach from the originating HTTP request (WithoutCancel), mirroring
	// refreshOne's post-commit best-effort context handling: the rename has
	// already committed on disk, so a client disconnect must not cancel this
	// self-heal.
	healCtx := context.WithoutCancel(ctx)
	if mbid := p.mbidFor(healCtx, artistID); mbid != "" {
		for connID, platformArtistID := range p.selfHealLidarrLinks(healCtx, artistID, mbid, linked) {
			platformIDs = append(platformIDs, artist.PlatformID{
				ArtistID:         artistID,
				ConnectionID:     connID,
				PlatformArtistID: platformArtistID,
			})
		}
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
	// PUT. For a shared-mount deployment MapArtistPath returns newPath unchanged,
	// so this is a no-op; for a split-mount peer with configured PathMappings it
	// rewrites the prefix so the peer receives a path it can resolve instead of
	// one it rejects or silently stores as nonsense. Since #2380 this applies to
	// Emby and Jellyfin too, not just Lidarr. Both rename and merge propagation
	// route through this single MapArtistPath -> guard -> UpdateArtistPath
	// chokepoint.
	platformPath := conn.MapArtistPath(newPath)

	// Pre-flight root guard. A path outside the peer's own roots is refused here
	// and NEVER pushed: every peer accepts (or silently ignores) such a path
	// while reporting success, so this is the only place the mistake can be
	// caught. Fail-closed - see guardPlatformPath.
	if err := p.guardPlatformPath(callCtx, conn, newPath, platformPath); err != nil {
		res.Error = truncErr(err)
		p.logger.Error("rename-sync: refusing out-of-root path",
			slog.String("artist_id", artistID),
			slog.String("connection", conn.Name),
			slog.String("type", conn.Type),
			slog.String("host_path", newPath),
			slog.String("platform_path", platformPath),
			slog.String("error", truncErr(err)))
		return res
	}

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

	// UpdateArtistPath returned success. That means NOTHING on Emby or Jellyfin:
	// both accept the Path field, answer 204, and silently discard it (each proven
	// by replay against a live server). Verify against the peer's own state -- never
	// against its status code. This read-back is THE mechanism that would have
	// caught #2380, and it is unconditional.
	return p.reconcilePeerLink(callCtx, conn, res, artistID, newPath, platformPath, pid.PlatformArtistID)
}

// reconcilePeerLink is the post-update truth check: read the path back from the
// peer and, when the peer did not honor it, re-resolve the artist and rewrite
// Stillwater's link to it.
//
// The split by honorsPathWrites is not cosmetic. On Lidarr a mismatch is a genuine
// FAULT (Lidarr does store paths, so a divergence means it coerced ours against
// its Root Folder list) and the operator must see it. On Emby/Jellyfin a mismatch
// is the EXPECTED, unavoidable outcome of a peer with no repath endpoint, so
// failing there would just spam an operator who can do nothing about it -- the
// correct response is to relink, not to complain.
func (p *Publisher) reconcilePeerLink(
	ctx context.Context,
	conn *connection.Connection,
	res artist.PlatformRemapResult,
	artistID, newPath, platformPath, platformArtistID string,
) artist.PlatformRemapResult {
	resolver, ok := relinkResolverFactory(conn, p.logger)
	if !ok {
		res.Error = "connection type cannot verify a path update: " + conn.Type
		p.logger.Warn("rename-sync: no verify surface for connection type",
			slog.String("artist_id", artistID),
			slog.String("connection", conn.Name),
			slog.String("type", conn.Type))
		return res
	}

	honored, got, err := p.verifyPeerPath(ctx, resolver, platformArtistID, platformPath)
	if err != nil {
		// Could not read the peer's state back. Fail rather than assume: an
		// unverifiable push is exactly the situation #2380 shipped broken from.
		res.Error = "could not verify the path update against the peer: " + truncErr(err)
		p.logger.Error("rename-sync: path read-back failed",
			slog.String("artist_id", artistID),
			slog.String("connection", conn.Name),
			slog.String("type", conn.Type),
			slog.String("error", truncErr(err)))
		return res
	}

	if honored {
		res.Result = artist.PlatformRemapOK
		p.logger.Info("rename-sync: path updated and verified against the peer",
			slog.String("artist_id", artistID),
			slog.String("connection", conn.Name),
			slog.String("type", conn.Type),
			slog.String("new_path", newPath),
			slog.String("platform_path", platformPath))
		return res
	}

	if honorsPathWrites(conn.Type) {
		// Lidarr DOES persist paths, so a mismatch here is a real fault on the
		// peer (typically a Root Folder coercion), not the ignore-the-field
		// behavior the media servers exhibit. Surface it.
		res.Error = fmt.Sprintf("%s did not store the path we sent: sent %q, peer reports %q", conn.Type, platformPath, got)
		p.logger.Error("rename-sync: peer coerced the path",
			slog.String("artist_id", artistID),
			slog.String("connection", conn.Name),
			slog.String("type", conn.Type),
			slog.String("sent", platformPath),
			slog.String("got", got))
		return res
	}

	// Emby / Jellyfin: the peer ignored the path, as it always does. The item we
	// are linked to is now stale -- it either still points at the old directory or,
	// after a merge, has been abandoned as a metadata-only ghost. Re-resolve the
	// artist against the peer's library and rewrite the link.
	p.logger.Info("rename-sync: peer ignored the path write (expected); re-resolving the link",
		slog.String("artist_id", artistID),
		slog.String("connection", conn.Name),
		slog.String("type", conn.Type),
		slog.String("sent", platformPath),
		slog.String("peer_reports", got))

	newID, relinkErr := p.relinkArtist(ctx, resolver, conn, artistID, p.artistNameFor(ctx, artistID), platformPath, platformArtistID)
	if relinkErr != nil {
		// THE RENAME PATH NEVER DROPS THE LINK. Not on a timeout, not on an empty
		// listing, not on an item we cannot find, not on a ghost we think we can
		// recognize. Every one of those is the SAME OBSERVATION as "the peer has not
		// rescanned yet", because the peer's index updates asynchronously and its
		// scan takes minutes against a budget measured in seconds. Two earlier
		// versions of this code tried to infer staleness here anyway and both DELETED
		// GOOD LINKS -- and nothing re-establishes a dropped link automatically, so
		// every wrong drop silently stops all future pushes for that artist.
		//
		// So: keep the link, and be LOUD. The operator gets a failed per-platform
		// result naming the remedy, not a silent no-op. Ghosts are collected where the
		// evidence actually exists -- the merge path (which holds the loser's link) and
		// the background reconciler (#2426), which can wait minutes for the peer to
		// settle before it re-resolves and drops.
		res.Error = truncErr(relinkErr) + "; the existing link was KEPT (it could not be verified, " +
			"and an unverified link must not be destroyed); retry the rename or run a library scan for this connection"
		p.logger.Error("rename-sync: could not VERIFY the peer link after the move; keeping the existing link",
			slog.String("artist_id", artistID),
			slog.String("connection", conn.Name),
			slog.String("type", conn.Type),
			slog.String("platform_path", platformPath),
			slog.String("kept_platform_artist_id", platformArtistID),
			slog.String("error", truncErr(relinkErr)))
		return res
	}

	res.Result = artist.PlatformRemapOK
	p.logger.Info("rename-sync: peer link re-resolved after the move",
		slog.String("artist_id", artistID),
		slog.String("connection", conn.Name),
		slog.String("type", conn.Type),
		slog.String("platform_path", platformPath),
		slog.String("platform_artist_id", newID))
	return res
}

// artistNameFor loads the artist's name, the fallback identity key the relink uses
// on a peer that exposes no paths (Emby). Returns "" when the artist cannot be
// loaded; resolvePeerArtist then simply has no name to match on and the relink
// falls through to its safe "could not resolve" branch rather than mislinking.
// An empty return is NOT benign: on a peer that exposes no paths (Emby) the name
// is the ONLY identity key the relink has, so "" means the artist cannot be
// resolved at all. Both failure branches therefore shout rather than shrug -- a
// silently missing name would present as an unexplained relink failure.
func (p *Publisher) artistNameFor(ctx context.Context, artistID string) string {
	if p.artistGetter == nil {
		p.logger.Error("rename-sync: no artist getter wired; the relink has no name key "+
			"and cannot resolve an artist on a pathless peer (Emby). This is a wiring bug.",
			slog.String("artist_id", artistID))
		return ""
	}
	a, err := p.artistGetter.GetByID(ctx, artistID)
	if err != nil || a == nil {
		// truncErr dereferences the error, and this branch is reachable with a NIL
		// error (a not-found repository returns (nil, nil) -- see artist.GetByMBID),
		// so guard it. Reporting a missing name must not itself panic mid-rename.
		detail := "artist not found"
		if err != nil {
			detail = truncErr(err)
		}
		p.logger.Error("rename-sync: could not load the artist name for the relink; "+
			"a pathless peer (Emby) cannot be resolved without it",
			slog.String("artist_id", artistID),
			slog.String("error", detail))
		return ""
	}
	return a.Name
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
