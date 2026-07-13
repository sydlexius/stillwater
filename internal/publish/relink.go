package publish

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/connection/lidarr"
)

// THE #2380 CORE INVARIANT
//
// A peer's 2xx on a path update proves NOTHING. Two of the three peers accept a
// Path field, answer 204, and silently discard it:
//
//	Jellyfin 10.11.10  POST /Items/{id} with Path -> 204, path UNCHANGED   (replayed live)
//	Emby      4.9.5.0  POST /Items/{id} with Path -> 204, path UNCHANGED   (replayed live)
//	Lidarr             PUT  /api/v1/artist/{id}   -> path honored + echoed
//
// Neither media server has a repath endpoint AT ALL. An item's path there is
// DERIVED by the library scanner from the filesystem: the server does not move an
// item, the old item is abandoned and a new item appears at the new path. So the
// thing that must be updated after a directory move is not the peer's item - it is
// STILLWATER'S LINK to it (artist_platform_ids).
//
// Leaving the link alone is the actual reported corruption: the old Jellyfin item
// survives at "/config/metadata/artists/<name>" with no library folder behind it,
// Stillwater keeps pointing at it, and every future write for that artist targets a
// metadata-only ghost - while the UI reports a green "ok".
//
// So: after the move, read the path back; if the peer did not honor it (the normal
// case on Emby/Jellyfin), re-resolve the artist on the peer and REWRITE THE LINK.
//
// The one thing this must never do is "fix" the mismatch by adopting the peer's
// returned path into artists.path. That path is a CONTAINER path in the peer's
// namespace; writing it into Stillwater's own artists.path would break every
// filesystem operation and cannot be right for three peers at once.

// relinkPollBudget bounds how long a single connection's post-move relink waits
// for the peer's library scan to surface the moved directory. A library refresh
// is ASYNCHRONOUS - the peer accepts the job and returns immediately - so the new
// item is usually not visible on the first look.
//
// Deliberately modest, because the rename's HTTP response is waiting on this.
//
// THE BUDGET IS AN OPPORTUNITY WINDOW, NEVER AN ORACLE. Its expiry licenses exactly
// one conclusion: "the move has not surfaced YET". It can never license a drop,
// because on a peer's asynchronously-updating index "the item is gone" and "the
// item is mid-scan" ARE THE SAME OBSERVATION - and a real library scan takes
// MINUTES against this budget's seconds, so "mid-scan" is the NORMAL outcome here,
// not an edge case. Two previous versions of this file tried to tell those states
// apart from within the budget (first from the timeout alone, then from the peer's
// library roots) and both DELETED GOOD LINKS.
//
// So the rename path RESOLVES-OR-KEEPS and never drops. Within the budget a
// positive match (the peer's own item found AT the new directory) upgrades the
// link; anything else keeps the link we hold and reports the failure loudly. Ghosts
// are still collected, but where the evidence actually exists: the merge path holds
// the loser's link directly, and the background reconciler (#2426) has MINUTES to
// let the peer settle before it re-resolves and drops.
//
// Package-level vars, not consts, so tests can collapse them and exercise the
// timeout branch without burning real time.
var (
	relinkPollBudget   = 20 * time.Second
	relinkPollInterval = 2 * time.Second
)

// peerArtistResolver is the capability the post-move relink needs from a
// connection: read one item's path back, enumerate the peer's library artists,
// and ask it to re-scan. Implemented by all three adapters; declared locally so
// tests can inject a fake without standing up HTTP.
//
// There is deliberately NO ListRoots here. An earlier draft filtered candidates
// against the peer's root folders to exclude metadata-only ghosts; reverting that
// filter broke not one test, because resolvePeerArtist's "a name match must carry
// NO path" clause already subsumes it and is strictly stronger. An unprovable
// guard is worse than no guard, so it was removed rather than kept as decoration.
type peerArtistResolver interface {
	// GetArtistPath returns the peer's CURRENT path for the item, "" if the peer
	// exposes none (the normal Emby answer for every artist).
	GetArtistPath(ctx context.Context, platformArtistID string) (string, error)
	// ListLibraryArtists enumerates artists in the peer's music libraries.
	ListLibraryArtists(ctx context.Context) ([]connection.PeerArtist, error)
	// TriggerLibraryScan asks the peer to re-scan. Asynchronous on every peer.
	TriggerLibraryScan(ctx context.Context) error
}

// honorsPathWrites reports whether a connection type actually persists a path we
// send it. Only Lidarr does. Emby and Jellyfin are both PROVEN not to (see the
// header comment); for them a post-update read-back that does not match is the
// EXPECTED result, not an error, and must route to a relink rather than to a
// failure the operator can do nothing about.
func honorsPathWrites(connType string) bool {
	return connType == connection.TypeLidarr
}

// peerIsPathless reports whether a peer exposes NO path for its artists as a matter
// of design. Emby's MusicArtist entities are virtual and NAME-KEYED -- every one of
// them reports Path: null (37/37 on a live server) -- so on Emby "no path" is normal
// and carries no information.
//
// Jellyfin is FOLDER-BACKED: a healthy artist there always has a path. An item that
// reports none is anomalous, and an abandoned metadata-only ghost is precisely that.
// Accepting a pathless name match on a path-exposing peer is therefore how a ghost
// gets linked -- the #2380 corruption, reported green. So pathlessness is a
// legitimate identity key ONLY where the peer has no paths to give.
func peerIsPathless(connType string) bool {
	return connType == connection.TypeEmby
}

type jellyfinResolver struct{ c *jellyfin.Client }

func (r jellyfinResolver) GetArtistPath(ctx context.Context, id string) (string, error) {
	return r.c.GetArtistPath(ctx, id)
}
func (r jellyfinResolver) ListLibraryArtists(ctx context.Context) ([]connection.PeerArtist, error) {
	return r.c.ListLibraryArtists(ctx)
}
func (r jellyfinResolver) TriggerLibraryScan(ctx context.Context) error {
	return r.c.TriggerLibraryScan(ctx)
}

type embyResolver struct{ c *emby.Client }

func (r embyResolver) GetArtistPath(ctx context.Context, id string) (string, error) {
	return r.c.GetArtistPath(ctx, id)
}
func (r embyResolver) ListLibraryArtists(ctx context.Context) ([]connection.PeerArtist, error) {
	return r.c.ListLibraryArtists(ctx)
}
func (r embyResolver) TriggerLibraryScan(ctx context.Context) error {
	return r.c.TriggerLibraryScan(ctx)
}

type lidarrResolver struct{ c *lidarr.Client }

func (r lidarrResolver) GetArtistPath(ctx context.Context, id string) (string, error) {
	return r.c.GetArtistPath(ctx, id)
}
func (r lidarrResolver) ListLibraryArtists(ctx context.Context) ([]connection.PeerArtist, error) {
	return r.c.ListLibraryArtists(ctx)
}
func (r lidarrResolver) TriggerLibraryScan(ctx context.Context) error {
	// Lidarr has no server-wide library-scan primitive, and it does not need one:
	// it is the one peer that honors a path write, so its read-back matches and the
	// relink never gets this far. A no-op is the honest answer -- NOT a silent one,
	// hence the explicit nil rather than an unreachable panic.
	return nil
}

// relinkResolverFactory builds a peerArtistResolver for a connection. Overridable
// by tests (t.Cleanup-restored), mirroring renamePathUpdaterFactory /
// mergeRefresherFactory. Returns (nil, false) for a type with no resolve surface.
var relinkResolverFactory = func(conn *connection.Connection, logger *slog.Logger) (peerArtistResolver, bool) {
	switch conn.Type {
	case connection.TypeEmby:
		return embyResolver{emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)}, true
	case connection.TypeJellyfin:
		return jellyfinResolver{jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)}, true
	case connection.TypeLidarr:
		return lidarrResolver{lidarr.New(conn.URL, conn.APIKey, logger)}, true
	default:
		return nil, false
	}
}

// errRelinkUnverified is the ONLY failure the rename path can produce. It means the
// move did not re-resolve to a peer item: the peer was unreachable, the scan could
// not be triggered, the context expired, the poll budget ran out before the move
// surfaced, the candidates were ambiguous, or the database refused the write.
//
// It does NOT mean the link is bad, and NOTHING observable from inside the rename's
// budget could show that it is. A peer's artist listing is a snapshot of an index
// the peer rebuilds ASYNCHRONOUSLY, so "the item is gone" and "the item is mid-scan"
// are the same observation. Two previous versions of this code tried to separate
// them anyway - one on the timeout, one on the peer's library roots - and both
// deleted good links. There is no scheduled connection sync to put a dropped link
// back (the only re-stamp path is an operator-triggered library scan), so a wrong
// drop silently stops every future push for that artist until a human notices.
//
// Hence the rule this file now enforces without exception: on the rename path we
// resolve or we KEEP, and we report loudly either way. Dropping happens only where
// the evidence really exists - the merge path, which holds the loser's link, and the
// background reconciler (#2426), which can wait minutes for the peer to settle.
var errRelinkUnverified = fmt.Errorf("peer link could not be verified after the move")

// verifyPeerPath is the read-back verifier, GENERALIZED across all three adapters
// and ALWAYS ON.
//
// It returns honored=true only when the peer's own read-back agrees with the path
// we sent. It NEVER trusts UpdateArtistPath's return value, because that value is
// a lie on two of the three peers.
//
// Before #2380 this mechanism existed on Lidarr ALONE, was opt-in, and defaulted
// to OFF -- switched off on exactly the peers where it would have caught the bug,
// and left on the one peer where it is weakest (Lidarr echoes its input, so its
// read-back cannot distinguish "stored" from "echoed"). It is now unconditional:
// a correctness guard, not a preference. That is why nothing here consults
// Connection.GetVerifyPathAfterUpdate -- that toggle still drives Lidarr's own
// in-client check and is left alone, but it can no longer switch OFF the guard
// that matters.
func (p *Publisher) verifyPeerPath(ctx context.Context, r peerArtistResolver, platformArtistID, sentPath string) (honored bool, got string, err error) {
	got, err = r.GetArtistPath(ctx, platformArtistID)
	if err != nil {
		return false, "", err
	}
	return connection.SamePeerPath(got, sentPath), got, nil
}

// resolvePeerArtist finds the peer item that now backs the moved directory.
//
// THE INVARIANT: link only to an item that either
//
//	(a) SITS AT the directory we just moved to, or
//	(b) has NO PATH AT ALL (the peer does not track paths for artists) and is the
//	    unique item carrying the artist's name.
//
// Everything else is refused. That single rule is what makes the metadata-only
// ghost unlinkable, and it is worth being precise about why, because a weaker rule
// looks equally correct and is not.
//
// When Jellyfin abandons an artist during a move it strands the old item at
// "/config/metadata/artists/<name>" -- observed live, sitting outside the library
// roots ("/music", "/classical") while a fresh item appears at the new directory.
// That ghost has the RIGHT NAME. A resolver that name-matched anything would link
// straight to it and reproduce the corruption. Clause (b)'s requirement that the
// candidate have NO path is what disqualifies it: a ghost always has one.
//
// Clause (b) is also strictly stronger than asking "is this path inside the peer's
// roots?" -- it additionally refuses an item that IS inside the roots but is still
// sitting at its OLD path because the peer's scan has not run yet. Linking to that
// is the same stale-link bug wearing a different hat, and a roots-based ghost
// filter would wave it straight through.
//
// Clause (b) is not optional generosity either: Emby reports a null path for every
// artist (proven live), so there the name is the ONLY key that exists. Requiring
// uniqueness is what keeps it safe -- an ambiguous name refuses rather than guesses.
//
// Returns "" (no error) when nothing resolves yet: the caller polls, because the
// peer's library scan is asynchronous.
// currentID is the link we already hold. It is passed in because a link that is
// STILL VALID must never be re-derived: re-deriving it can only lose. On Emby the
// artist item is name-keyed and pathless, so it SURVIVES a directory rename
// unchanged -- the link we hold is already correct, and throwing it away to
// rebuild it from name-uniqueness turns every quirk of the peer's naming into a
// dropped link. Keep what we have whenever the peer still reports it as a
// legitimate target.
func resolvePeerArtist(items []connection.PeerArtist, wantPath, artistName, currentID string, peerPathless bool) (string, error) {
	// Dedupe by ID FIRST. A peer returns the same global artist entity once per
	// music library it appears in (Emby's /Artists/AlbumArtists is queried
	// per-ParentId, and production commonly has two music roots -- e.g. /music
	// and /classical). The same Id coming back twice is ONE artist, not an
	// ambiguity, and counting it as two used to drop the link of any artist that
	// happened to have tracks in both roots.
	seen := make(map[string]bool, len(items))
	deduped := make([]connection.PeerArtist, 0, len(items))
	for _, it := range items {
		if it.ID == "" || seen[it.ID] {
			continue
		}
		seen[it.ID] = true
		deduped = append(deduped, it)
	}

	var nameHits []connection.PeerArtist
	var pathHits []connection.PeerArtist
	for _, it := range deduped {
		// (a) The item AT the new directory we just moved to.
		if connection.SamePeerPath(it.Path, wantPath) {
			pathHits = append(pathHits, it)
			continue
		}
		// (b) A name match is a candidate ONLY if the peer tracks no path for it AND
		// the peer is one that has no paths to give (Emby). An item WITH a path that
		// is not wantPath is either a ghost or a not-yet-rescanned item, and neither
		// may ever be linked. An item WITHOUT a path on a FOLDER-BACKED peer
		// (Jellyfin) is anomalous -- a healthy artist there always has one -- and an
		// abandoned ghost is exactly that shape, so it must not be linked either.
		if peerPathless && artistName != "" && it.Name == artistName && it.Path == "" {
			nameHits = append(nameHits, it)
		}
	}

	// The link we already hold still satisfies the invariant -- keep it.
	//
	// It must satisfy the SAME invariant a fresh link would, NAME INCLUDED. An
	// earlier version checked only that the ID was present and pathless, which meant
	// a link already pointing at the WRONG artist's item (a mislink from an earlier
	// scan, or an item a merge left behind) got RATIFIED and reported ok -- the
	// relink could no longer repair a bad link, only rubber-stamp it. Confirming a
	// link is not the same as accepting whatever we happen to hold.
	//
	// This runs BEFORE the ambiguity check on purpose: if the peer reports two
	// artists sharing a name and one of them is the item we are already correctly
	// linked to, that is not an ambiguity we need to resolve.
	if id := keepCurrentIfStillValid(deduped, wantPath, artistName, currentID, peerPathless); id != "" {
		return id, nil
	}

	// (a) resolved. A single item at the target directory is the artist,
	// unambiguously. More than one (Jellyfin transiently carries both the old and
	// the re-derived item) is NOT a free choice: prefer the link we already hold,
	// then a name match, and refuse rather than pick an arbitrary listing order --
	// an earlier version returned whichever happened to come first, which could hand
	// the link to an item with a completely different name.
	if len(pathHits) > 0 {
		return resolvePathHits(pathHits, wantPath, artistName, currentID)
	}

	switch len(nameHits) {
	case 0:
		return "", nil
	case 1:
		return nameHits[0].ID, nil
	default:
		// M1: an ambiguous NAME is a failure to CHOOSE, not proof the link is dead.
		// It must not license a delete.
		return "", fmt.Errorf("%w: %d items on the peer share the name %q, so the correct one is ambiguous",
			errRelinkUnverified, len(nameHits), artistName)
	}
}

// relinkArtist re-resolves the artist on the peer after the directory moved and,
// on a positive match, rewrites artist_platform_ids to point at the item that now
// backs the new path.
//
// RESOLVE OR KEEP. It returns the platform ID it linked to, or an error wrapping
// errRelinkUnverified - and it NEVER drops the link it was given. Every path out of
// here that is not a positive match leaves artist_platform_ids exactly as it found
// it, because nothing observable within the poll budget can distinguish a peer that
// has abandoned our item from a peer that has merely not rescanned yet. See
// errRelinkUnverified for why guessing between those two cost this code two
// rounds of deleted links, and relinkPollBudget for why the budget cannot settle it.
//
// The upgrade is strictly monotone: we only ever move the link to an item the peer
// itself reports AT the directory we just moved to (or, on a pathless-by-design peer,
// to the unique item carrying the artist's name). A failure to find that is a failure
// to IMPROVE the link, never evidence against the link we already hold.
func (p *Publisher) relinkArtist(
	ctx context.Context,
	r peerArtistResolver,
	conn *connection.Connection,
	artistID, artistName, newPlatformPath, currentPlatformID string,
) (string, error) {
	// One look BEFORE asking for a scan: a peer whose own filesystem watcher already
	// picked the move up needs no scan at all, and this makes the common case a
	// single round-trip instead of a scan plus a poll.
	id, resolveErr := p.tryResolve(ctx, r, newPlatformPath, artistName, currentPlatformID, peerIsPathless(conn.Type))
	if resolveErr != nil {
		return "", resolveErr
	}
	if id != "" {
		return id, p.commitRelink(ctx, conn, artistID, currentPlatformID, id)
	}

	if err := r.TriggerLibraryScan(ctx); err != nil {
		// We could not even ask the peer to rescan. That says nothing about the
		// link. Keep it.
		return "", fmt.Errorf("%w: could not trigger a %s library scan to pick up the moved directory: %s",
			errRelinkUnverified, conn.Type, truncErr(err))
	}

	// The scan is asynchronous, so poll until the moved directory surfaces or the
	// budget runs out. ctx already carries the per-connection deadline, so a slow
	// peer cannot stretch past it either way.
	//
	// Both exits below are UNVERIFIED, unconditionally. A peer that has not surfaced
	// the move within seconds is overwhelmingly just a peer whose library scan takes
	// minutes, and no amount of squinting at the listing can prove otherwise from
	// here.
	ticker := time.NewTicker(relinkPollInterval)
	defer ticker.Stop()
	deadline := time.After(relinkPollBudget)
	for {
		select {
		case <-ctx.Done():
			// Both errors are wrapped: callers match errRelinkUnverified for the
			// keep-the-link policy, and the context error stays reachable for anyone
			// distinguishing a cancellation from a deadline.
			return "", fmt.Errorf("%w: %s did not surface %q before the deadline (%w)",
				errRelinkUnverified, conn.Type, newPlatformPath, ctx.Err())
		case <-deadline:
			return "", fmt.Errorf("%w: the %s library scan did not surface %q within %s",
				errRelinkUnverified, conn.Type, newPlatformPath, relinkPollBudget)
		case <-ticker.C:
			pollID, pollErr := p.tryResolve(ctx, r, newPlatformPath, artistName, currentPlatformID, peerIsPathless(conn.Type))
			if pollErr != nil {
				return "", pollErr
			}
			if pollID != "" {
				return pollID, p.commitRelink(ctx, conn, artistID, currentPlatformID, pollID)
			}
		}
	}
}

// keepCurrentIfStillValid returns the link we already hold when the peer still
// reports it as a legitimate target, or "" when it does not.
//
// It applies the SAME invariant a fresh link must satisfy, NAME INCLUDED. An earlier
// version checked only that the ID was present and pathless, so a link already
// pointing at the WRONG artist's item got ratified and reported ok -- the relink
// could no longer repair a bad link, only rubber-stamp it.
func keepCurrentIfStillValid(items []connection.PeerArtist, wantPath, artistName, currentID string, peerPathless bool) string {
	if currentID == "" {
		return ""
	}
	for _, it := range items {
		if it.ID != currentID {
			continue
		}
		if connection.SamePeerPath(it.Path, wantPath) {
			return currentID
		}
		// Pathless is acceptable ONLY on a peer that is pathless by design (Emby), and
		// only with a name match -- exactly what clause (b) demands of any other
		// candidate. On a folder-backed peer a pathless item is a ghost, and keeping a
		// link to it is the corruption this file exists to remove.
		if peerPathless && it.Path == "" && artistName != "" && it.Name == artistName {
			return currentID
		}
	}
	return ""
}

// resolvePathHits picks among items sitting AT the directory we moved to.
//
// One is unambiguous. More than one is NOT a free choice: Jellyfin transiently
// carries both the old and the re-derived item, and an earlier version returned
// whichever came first in listing order -- with no name check -- so the link could be
// handed to an item with a completely different name. Prefer the link we already
// hold, then a unique name match, and otherwise REFUSE rather than guess. Refusing
// is errRelinkUnverified -- the only failure this file has: failing to CHOOSE
// between candidates is not evidence that what we hold is dead, so the link stays.
func resolvePathHits(pathHits []connection.PeerArtist, wantPath, artistName, currentID string) (string, error) {
	if len(pathHits) == 1 {
		return pathHits[0].ID, nil
	}
	for _, it := range pathHits {
		if currentID != "" && it.ID == currentID {
			return it.ID, nil
		}
	}
	var named []connection.PeerArtist
	for _, it := range pathHits {
		if artistName != "" && it.Name == artistName {
			named = append(named, it)
		}
	}
	if len(named) == 1 {
		return named[0].ID, nil
	}
	return "", fmt.Errorf("%w: %d items on the peer sit at %q, so the correct one cannot be chosen",
		errRelinkUnverified, len(pathHits), wantPath)
}

// tryResolve is one enumerate-and-match attempt: "" (no error) when nothing matches
// yet, so the caller polls.
//
// A transport error stops the poll loop rather than silently burning its budget
// against a peer that is down. Like every other non-match here it is UNVERIFIED: a
// peer we cannot reach tells us nothing about whether the link we hold is good.
func (p *Publisher) tryResolve(
	ctx context.Context, r peerArtistResolver, wantPath, artistName, currentID string, peerPathless bool,
) (string, error) {
	items, err := r.ListLibraryArtists(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: could not enumerate the peer's library artists: %s",
			errRelinkUnverified, truncErr(err))
	}
	return resolvePeerArtist(items, wantPath, artistName, currentID, peerPathless)
}

// commitRelink writes the re-resolved platform ID into artist_platform_ids.
//
// Uses the AUTHORITATIVE SetPlatformID, not the divergence-aware
// SetPlatformIDStable the scan-time resolvers use. That distinction is the whole
// point: those callers are guessing from a library listing and must not clobber a
// better-known mapping, whereas this caller JUST MOVED THE DIRECTORY and read the
// peer's own item back at the new path. It knows the truth, so it overwrites.
// Routing this through the stable set would preserve the very stale row we are
// here to replace.
func (p *Publisher) commitRelink(ctx context.Context, conn *connection.Connection, artistID, oldPlatformID, newPlatformID string) error {
	if oldPlatformID == newPlatformID {
		// The peer kept the same item (a rename it absorbed in place). Nothing to
		// rewrite; the link is already correct.
		p.logger.Debug("relink: peer kept the same item id",
			slog.String("artist_id", artistID),
			slog.String("connection", conn.Name),
			slog.String("platform_artist_id", newPlatformID))
		return nil
	}
	if err := p.artistService.SetPlatformID(ctx, artistID, conn.ID, newPlatformID); err != nil {
		// We resolved the CORRECT item and the database refused the write. Dropping
		// here would delete a link because our own storage hiccuped -- destroying
		// good data to punish an unrelated failure. UNVERIFIED: keep what we have.
		return fmt.Errorf("%w: re-resolved the artist to %s item %s but could not store the link: %s",
			errRelinkUnverified, conn.Type, newPlatformID, truncErr(err))
	}
	p.logger.Info("relink: rewrote peer link after directory move",
		slog.String("artist_id", artistID),
		slog.String("connection", conn.Name),
		slog.String("type", conn.Type),
		slog.String("previous_platform_artist_id", oldPlatformID),
		slog.String("platform_artist_id", newPlatformID))
	return nil
}
