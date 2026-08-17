package publish

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
)

// ReconcileRelinks periodically re-attempts the post-move relink for every
// artist with a FOLDER-BACKED peer platform mapping -- today that means
// Jellyfin only (#2426). Emby and Lidarr are skipped; see the peer-type
// section below for why each is excluded.
//
// The rename path (relink.go, reconcilePeerLink) resolves or keeps within a
// short poll budget and never drops a link -- see errRelinkUnverified for why
// guessing at the difference between "not yet rescanned" and "abandoned
// ghost" from inside that budget deleted good links twice. This reconciler is
// the other half of the fix: it runs on an interval measured in MINUTES, not
// seconds (default 15, operator-configurable via
// relink_reconcile.interval_minutes -- see cmd/stillwater/main.go), so a peer
// whose library scan genuinely just needed more time gets caught up
// automatically instead of staying wrong until an operator notices and
// manually triggers a library scan.
//
// It NEVER drops a link either. It only ever calls commitRelink -- the same
// monotone upgrade the rename path uses -- when the peer's own listing now
// shows an item AT the artist's current path. (resolvePeerArtist's separate
// pathless-name-match clause exists for the rename path's benefit; it is
// UNREACHABLE from here, because reconcileMapping skips every pathless-by-design
// peer -- currently just Emby -- before resolvePeerArtist is ever called. See
// the skip list below.) An artist whose link still cannot be resolved is left
// exactly as it is and tried again on the next pass. Ghost collection
// (deleting a link with no confirmable target) stays out of scope: two
// earlier attempts to infer that from a snapshot listing (once from a
// timeout, once from library roots) both deleted good links, and nothing here
// changes the evidence available to make that call safely. See relink.go's
// package-level comment for the full history.
//
// TWO OF THE THREE PEER TYPES ARE SKIPPED, leaving this a JELLYFIN-ONLY
// repair. Both skips are in reconcileMapping, each with its full reasoning:
//
//   - Lidarr honors a path write (honorsPathWrites), so its links never enter
//     the unverified state at all.
//   - Emby is pathless BY DESIGN (peerIsPathless), so its links also never
//     enter that state -- and acting on the only key it does expose, the name,
//     would let an unattended sweep repoint a good link onto a metadata-only
//     ghost. That is #2442's deferred follow-up, and it is deliberately left
//     UNSOLVED rather than guessed at.
//
// So the repaired population is exactly the one that can be repaired on
// evidence: folder-backed peers, matched on the path we moved the directory to.
//
// It writes through commitRelink, which uses the AUTHORITATIVE SetPlatformID.
// That deserves justifying, because commitRelink's own doc comment reserves the
// authoritative setter for a caller that JUST MOVED THE DIRECTORY and warns off
// callers "guessing from a library listing" -- which describes a periodic sweep.
// The divergence-aware SetPlatformIDStable is nonetheless the WRONG tool here,
// for a mechanical reason rather than a stylistic one: it resolves a conflict by
// keeping the LEXICOGRAPHICALLY LOWER of the stored and incoming ids
// (sqlite_platform.go, "Insert-or-tiebreak"), so it would discard a correct
// re-resolution whenever the correct item's id happens to sort above the stale
// one. Repair would then succeed or fail on string ordering of two opaque peer
// ids -- a coin flip, and the same defect that makes an operator's manual
// library scan an unreliable fix today.
//
// What makes the authoritative write legitimate is that this caller is NOT
// guessing. It only ever writes an id that resolvePeerArtist matched AT the
// artist's current path on a folder-backed peer -- the same positive,
// path-backed evidence the rename path acts on, just observed later. The
// distinction commitRelink's comment is drawing is evidence vs. inference, not
// synchronous vs. background, and the skips above are what keep this caller on
// the evidence side of it.
func (p *Publisher) ReconcileRelinks(ctx context.Context) {
	if p == nil {
		return
	}

	artistIDs, err := p.artistService.ListArtistsWithPlatformMappings(ctx)
	if err != nil {
		p.logger.Error("relink reconciler: listing artists with platform mappings",
			slog.Any("error", err))
		return
	}
	if len(artistIDs) == 0 {
		return
	}
	if p.artistGetter == nil {
		p.logger.Warn("relink reconciler: no artist getter wired; cannot load artist paths, reconciler is a no-op")
		return
	}

	p.logger.Info("relink reconciler: starting run", slog.Int("artist_count", len(artistIDs)))

	// Per-run cache: one peer listing per connection, not per artist, so an
	// artist-dense connection is not re-listed once per artist -- exactly the
	// large-library scenario this reconciler exists to help costs the peer
	// only ONE listing call per connection per pass.
	rr := &relinkReconcileRun{p: p, cache: map[string][]connection.PeerArtist{}}

	for _, artistID := range artistIDs {
		if ctx.Err() != nil {
			break
		}
		rr.reconcileArtist(ctx, artistID)
	}

	// Test-only observation hook: the run's counters (upgraded in particular)
	// are otherwise unreachable from outside this package, and a test that
	// wants to discriminate "no-op because already correct" from "no-op
	// because a broken guard fired anyway" needs a real observable rather than
	// asserting on directSets alone (see TestReconcileRelinks_AlreadyCorrectIsANoOp).
	// nil in production; tests set and restore it.
	if relinkReconcileTestHook != nil {
		relinkReconcileTestHook(rr)
	}

	p.logger.Info("relink reconciler: run complete",
		slog.Int("checked", rr.checked),
		slog.Int("upgraded", rr.upgraded),
		slog.Int("skipped_no_connection", rr.skippedNoConn),
		slog.Int("skipped_disabled", rr.skippedDisabled),
		slog.Int("skipped_list_error", rr.skippedListErr),
		slog.Int("skipped_load_error", rr.skippedLoadErr),
		slog.Int("skipped_peer_type", rr.skippedPeerType),
		slog.Int("skipped_no_resolver", rr.skippedNoResolver),
		slog.Int("skipped_resolve_error", rr.skippedResolveErr))
}

// relinkReconcileRun carries the per-run peer-listing cache and stat counters
// across the artist/connection fan-out, keeping ReconcileRelinks itself under
// the cognitive-complexity lint cap.
type relinkReconcileRun struct {
	p     *Publisher
	cache map[string][]connection.PeerArtist

	checked, upgraded, skippedNoConn, skippedDisabled, skippedListErr     int
	skippedLoadErr, skippedPeerType, skippedNoResolver, skippedResolveErr int
}

// relinkReconcileTestHook, when non-nil, is invoked with the completed run
// right before the summary log. Test-only observability for counters
// (upgraded, skippedPeerType, ...) that have no other externally visible
// effect. nil in production.
var relinkReconcileTestHook func(*relinkReconcileRun)

// reconcileArtist re-attempts the relink for every one of one artist's
// platform mappings.
func (rr *relinkReconcileRun) reconcileArtist(ctx context.Context, artistID string) {
	p := rr.p
	// Minimal hydration (#2426): this reconciler reads exactly two fields off
	// the artist, a.Name and a.Path, both populated by the base row load
	// itself (artist.Service.GetByID's underlying store query), never by the
	// opt-in side-table hydrations (provider IDs, images, primary library).
	// The default (no opts) pulls in all three of those extra queries per
	// call; on a large library that is tens of thousands of needless queries
	// every reconciler interval.
	a, artistErr := p.artistGetter.GetByID(ctx, artistID, artist.HydrateOpts{})
	if artistErr != nil || a == nil {
		// Both branches LOG. An earlier version returned silently, so an artist
		// that could never be loaded was indistinguishable from one that needed no
		// work: the run counters showed a healthy pass and the operator's link
		// stayed broken forever with nothing anywhere saying why. A skip is a
		// decision, and an unattended job's decisions have to be readable after
		// the fact.
		//
		// A nil artist with a nil error gets its own message rather than being
		// folded into the error case: it means the store answered successfully
		// that the artist does not exist, which points at a stale
		// artist_platform_ids row (an orphan the cascade missed), not at an
		// infrastructure failure. Same skip, different diagnosis.
		if artistErr != nil {
			p.logger.Warn("relink reconciler: loading artist; skipping it this pass",
				slog.String("artist_id", artistID),
				slog.Any("error", artistErr))
		} else {
			p.logger.Warn("relink reconciler: artist has platform mappings but no artist row; skipping it this pass",
				slog.String("artist_id", artistID))
		}
		rr.skippedLoadErr++
		return
	}
	platformIDs, pidErr := p.artistService.GetPlatformIDs(ctx, artistID)
	if pidErr != nil {
		p.logger.Warn("relink reconciler: loading platform mappings; skipping this artist this pass",
			slog.String("artist_id", artistID),
			slog.Any("error", pidErr))
		rr.skippedLoadErr++
		return
	}
	for _, pid := range platformIDs {
		rr.checked++
		rr.reconcileMapping(ctx, artistID, a.Name, a.Path, pid)
	}
}

// reconcileMapping re-attempts the relink for one artist<->connection
// mapping: it never drops, and only ever upgrades the link when the peer's
// own listing shows a strictly better match than the one already held.
func (rr *relinkReconcileRun) reconcileMapping(ctx context.Context, artistID, artistName, artistPath string, pid artist.PlatformID) {
	p := rr.p
	conn, connErr := p.connectionService.GetByID(ctx, pid.ConnectionID)
	if connErr != nil {
		// A DB error loading the connection row. Distinct from "no such
		// connection": this one is an infrastructure hiccup, not necessarily an
		// orphaned mapping, so it earns its own log line rather than being folded
		// into the missing-connection case below.
		p.logger.Warn("relink reconciler: loading connection; skipping this mapping this pass",
			slog.String("artist_id", artistID),
			slog.String("connection_id", pid.ConnectionID),
			slog.Any("error", connErr))
		rr.skippedNoConn++
		return
	}
	if conn == nil {
		// The connection row does not exist. Points at an orphaned
		// artist_platform_ids row (a connection deleted out from under a
		// mapping) rather than a transient failure -- worth flagging so an
		// operator can see the dangling reference.
		p.logger.Warn("relink reconciler: platform mapping references a connection that no longer exists; skipping",
			slog.String("artist_id", artistID),
			slog.String("connection_id", pid.ConnectionID))
		rr.skippedNoConn++
		return
	}
	if !conn.Enabled {
		// A deliberate operator choice, not a fault -- Debug is enough here.
		//
		// Its own counter, NOT skippedNoConn: "no connection" means the row is
		// missing or unreadable, which is a problem to investigate, whereas a
		// disabled connection is the operator getting exactly what they asked
		// for. Folding the two together made the run summary report a healthy
		// deliberate configuration as if links were failing to resolve.
		p.logger.Debug("relink reconciler: skipping disabled connection",
			slog.String("artist_id", artistID),
			slog.String("connection", conn.Name),
			slog.String("connection_id", conn.ID))
		rr.skippedDisabled++
		return
	}
	// THE ALLOW-LIST IS WHAT ACTUALLY DECIDES which peer types this unattended
	// sweep may act on. It is a single positive gate: neither honorsPathWrites
	// nor peerIsPathless (documented below, for WHY Lidarr and Emby land
	// outside the allow-list specifically) is what stops a THIRD,
	// not-yet-considered peer type from joining the sweep -- that is
	// peerIsFolderBacked alone, and a new peer type must be deliberately added
	// to it. Without this gate, the only thing standing between a brand-new
	// connection.Type and an authoritative background write would be
	// relinkResolverFactory's switch two files away, whose job is building an
	// HTTP client, not authorizing writes. See peerIsFolderBacked's doc
	// comment.
	//
	// Lidarr: it stores the path we send it, so it never lands in the
	// unverified state this reconciler exists to repair.
	//
	// EMBY IS SKIPPED, AND THIS IS THE #2426 GHOST DECISION #2442 DEFERRED HERE.
	//
	// This reconciler repairs ONE failure: the peer had not finished
	// rescanning the MOVED DIRECTORY within the rename's poll budget, so the
	// link stayed unverified. That failure is path-shaped, and Emby has no
	// paths. Its MusicArtist entities are virtual and NAME-KEYED (every one
	// reports Path: null), so an Emby item SURVIVES a directory rename
	// unchanged -- the rename path's keepCurrentIfStillValid already ratifies
	// it on the spot, and the link never enters the state repaired here.
	// There is nothing for this reconciler to fix on Emby.
	//
	// What it COULD do on Emby is strictly harmful. With no path to compare,
	// resolvePeerArtist falls through to clause (b), where a unique pathless
	// NAME match resolves -- and on Emby an abandoned metadata-only ghost is
	// pathless with the right name, i.e. INDISTINGUISHABLE from the real item
	// on the only fields a listing carries (ID, Name, Path). So an unattended
	// sweep that acted on a name match would repoint a good link onto a ghost
	// whenever the real item happened to be missing from one listing. That is
	// #2442's flagged follow-up ("a link can be repointed to a name-matching
	// metadata ghost"), and it is precisely the #2380 corruption -- reported
	// green -- arriving through a new door.
	//
	// The rename path can afford clause (b) because it is repairing a link it
	// has POSITIVE reason to doubt (the directory just moved) and it acts once,
	// synchronously, with an operator watching the result. A periodic
	// background sweep has neither the triggering event nor the observer, so
	// the same generosity becomes an unattended, silent repoint.
	//
	// Withholding the pass is the same move publisher.go makes by withholding
	// DeletePlatformID: the safe behavior is structural, not a policy the next
	// patch can quietly undo. Ghost discrimination on Emby needs evidence a
	// listing does not carry, so it stays UNSOLVED rather than guessed at.
	// TestReconcileRelinks_Emby_GhostWouldRepointWithoutTheSkip pins the
	// behavior this skip is buying protection from.
	if !peerIsFolderBacked(conn.Type) {
		// M4: count every peer-type skip here, Lidarr and Emby included -- this
		// is the one number an operator needs to confirm, e.g., the Emby skip is
		// actually engaged in production, distinct from rr.checked (which counts
		// a skipped mapping as "checked" and would otherwise overstate work done).
		rr.skippedPeerType++
		return
	}
	resolver, ok := relinkResolverFactory(conn, p.logger)
	if !ok {
		// Unreachable today: peerIsFolderBacked above admits only Jellyfin, and
		// the factory builds a resolver for it. It becomes reachable the moment
		// a type is added to the allow-list without a matching factory case, and
		// a silent return there would present as "the reconciler simply does
		// nothing for this peer" with nothing anywhere saying why. Log loudly:
		// this is a wiring mistake, not a runtime condition.
		p.logger.Error("relink reconciler: no resolver for an allow-listed peer type; skipping (this is a wiring bug -- peerIsFolderBacked and relinkResolverFactory disagree)",
			slog.String("connection", conn.Name),
			slog.String("connection_id", conn.ID),
			slog.String("type", conn.Type))
		rr.skippedNoResolver++
		return
	}

	items := rr.peerArtists(ctx, resolver, conn)
	if items == nil {
		return
	}

	wantPath := conn.MapArtistPath(artistPath)
	resolvedID, resolveErr := resolvePeerArtist(items, wantPath, artistName, pid.PlatformArtistID, peerIsPathless(conn.Type))
	if resolveErr != nil {
		// SPLIT OUT of the combined condition below. An AMBIGUOUS listing (two
		// candidates at the path, say) is a diagnosable state an operator can
		// act on, and it was previously indistinguishable from the perfectly
		// ordinary "nothing matched yet, try next pass". Debug rather than Warn:
		// unlike an unreachable peer, this recurs every pass for a genuinely
		// ambiguous library and would drown the log at a visible level.
		p.logger.Debug("relink reconciler: resolving peer artist; leaving the link for the next pass",
			slog.String("artist_id", artistID),
			slog.String("connection", conn.Name),
			slog.String("connection_id", conn.ID),
			slog.Any("error", resolveErr))
		rr.skippedResolveErr++
		return
	}
	if resolvedID == "" || resolvedID == pid.PlatformArtistID {
		// Still unresolved, or already correct -- either way there is
		// nothing to do, and neither is noteworthy. Leave it for the next pass.
		return
	}

	// RE-READ IMMEDIATELY BEFORE THE WRITE (#2426 lost-update fix). The id we
	// are about to overwrite was snapshotted back in reconcileArtist, and
	// between then and now sits a connection lookup and, for the first artist
	// on this connection each pass, a full peer listing round trip -- a window
	// a concurrent foreground rename can land in. commitRelink itself compares
	// old==new against whatever the CALLER hands it, not against the database,
	// and it is shared with the synchronous rename path, so it must not grow a
	// conditional write here that would change ITS semantics. Instead: confirm
	// the stored id for this connection is still the one we snapshotted, right
	// before writing, and abort this mapping if it is not.
	//
	// This narrows the race window; it does not close it -- reading and then
	// writing is not compare-and-swap, and a rename could still land in the
	// gap between this re-read and the SetPlatformID call below. That residual
	// window is accepted rather than eliminated, because closing it needs a
	// real CAS primitive this package does not have. It is bounded by
	// self-healing: the NEXT pass re-resolves against a fresh listing and will
	// repair whatever this one missed, so the cost of the remaining window is
	// one extra reconciler interval, not a lost write.
	//
	// The foreground write, when one landed, is treated as more authoritative
	// than this background one: it acted on a listing fetched seconds ago in
	// response to a real rename event, where this reconciler's listing can be
	// minutes old by the time a large connection's later artists reach it.
	current, reReadErr := p.artistService.GetPlatformIDs(ctx, artistID)
	if reReadErr != nil {
		p.logger.Warn("relink reconciler: re-reading platform mappings before write; skipping this mapping this pass",
			slog.String("artist_id", artistID),
			slog.String("connection", conn.Name),
			slog.String("connection_id", conn.ID),
			slog.Any("error", reReadErr))
		return
	}
	stillMatches := false
	for _, cpid := range current {
		if cpid.ConnectionID == pid.ConnectionID {
			stillMatches = cpid.PlatformArtistID == pid.PlatformArtistID
			break
		}
	}
	if !stillMatches {
		p.logger.Info("relink reconciler: stored mapping changed since it was read for this pass (a concurrent write, or the mapping was deleted, e.g. via the platform-IDs DELETE API); yielding to the current stored state",
			slog.String("artist_id", artistID),
			slog.String("connection", conn.Name),
			slog.String("connection_id", conn.ID))
		return
	}

	if commitErr := p.commitRelink(ctx, conn, artistID, pid.PlatformArtistID, resolvedID); commitErr != nil {
		p.logger.Warn("relink reconciler: upgrading link",
			slog.String("artist_id", artistID),
			slog.String("connection", conn.Name),
			slog.String("connection_id", conn.ID),
			slog.Any("error", commitErr))
		return
	}
	rr.upgraded++
}

// peerArtists returns conn's cached listing for this run, fetching and
// caching it on first use.
//
// On a FETCH ERROR it caches and returns nil, so a down peer is listed once per
// run rather than once per artist. An EMPTY listing is returned as-is -- which
// may be a non-nil empty slice, not nil. The caller's `items == nil` check
// therefore short-circuits only the error case; an empty-but-non-nil listing
// falls through to resolvePeerArtist, which finds no match and leaves the link
// alone. Same outcome, different path, and the distinction matters to anyone
// reading that nil check: it is an ERROR guard, not an emptiness guard.
func (rr *relinkReconcileRun) peerArtists(ctx context.Context, resolver peerArtistResolver, conn *connection.Connection) []connection.PeerArtist {
	items, isCached := rr.cache[conn.ID]
	if isCached {
		return items
	}
	listed, listErr := resolver.ListLibraryArtists(ctx)
	if listErr != nil {
		rr.skippedListErr++
		// Warn, not Debug: this is the MOST LIKELY real-world skip (peer down,
		// API key rotated, wrong URL), and it was previously invisible at the
		// default log level. Already deduped per connection by the cache above,
		// so raising it cannot spam.
		rr.p.logger.Warn("relink reconciler: listing peer artists",
			slog.String("connection", conn.Name),
			slog.String("connection_id", conn.ID),
			slog.String("type", conn.Type),
			slog.Any("error", listErr))
		rr.cache[conn.ID] = nil
		return nil
	}
	rr.cache[conn.ID] = listed
	return listed
}

// StartRelinkReconciler runs ReconcileRelinks once at startup (after
// startupDelay) and then on a fixed interval until ctx is canceled. Mirrors
// StartArtworkReconciler (reconcile.go).
//
// startupDelay is a parameter so tests can drive it in milliseconds rather
// than waiting the full production delay.
//
// DISABLING IS THE CALLER'S JOB, NOT THIS FUNCTION'S. The operator-facing
// "off" switch is relink_reconcile.interval_minutes = 0, and main.go resolves
// that to "do not call this at all" (with a log line saying so) rather than
// calling it with a zero interval. So by the time control reaches here the
// interval is meant to be positive, and a non-positive one is a PROGRAMMING
// error in a new caller, not a configuration state -- hence Error, not Warn.
// Returning rather than starting a ticker is still the right response
// (time.NewTicker PANICS on a non-positive duration), but it must not look
// like a supported way to switch the feature off: a caller that "disables" the
// reconciler this way gets no operator-visible disable log, which is exactly
// the silent behavior #2426's review flagged.
func (p *Publisher) StartRelinkReconciler(ctx context.Context, interval, startupDelay time.Duration) {
	if p == nil {
		return
	}
	if interval <= 0 {
		p.logger.Error("relink reconciler: non-positive interval; reconciler not started (set relink_reconcile.interval_minutes to 0 to disable it deliberately)",
			slog.String("interval", interval.String()))
		return
	}
	p.logger.Info("relink reconciler started",
		slog.String("interval", interval.String()),
		slog.String("startup_delay", startupDelay.String()))

	// time.NewTimer + Stop rather than time.After: time.After's timer cannot be
	// stopped, so on a shutdown during the startup delay it stays alive in the
	// runtime's heap until it fires anyway -- up to the full delay after the
	// process was told to stop. Harmless in a one-shot, but this runs per
	// Publisher and the delay is measured in tens of seconds, so it is a real
	// (if small) shutdown-latency leak with a one-line fix.
	startupTimer := time.NewTimer(startupDelay)
	defer startupTimer.Stop()

	select {
	case <-ctx.Done():
		p.logger.Info("relink reconciler stopped before first run")
		return
	case <-startupTimer.C:
	}

	p.runReconcileRelinksWithRecover(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("relink reconciler stopped")
			return
		case <-ticker.C:
			p.runReconcileRelinksWithRecover(ctx)
		}
	}
}

// runReconcileRelinksWithRecover wraps ReconcileRelinks in a panic guard so a
// bug in the reconciler does not crash the whole process.
func (p *Publisher) runReconcileRelinksWithRecover(ctx context.Context) {
	defer func() {
		if v := recover(); v != nil {
			p.logger.Error("relink reconciler: panic recovered",
				slog.Any("panic", v),
				slog.String("stack", string(debug.Stack())))
		}
	}()
	p.ReconcileRelinks(ctx)
}
