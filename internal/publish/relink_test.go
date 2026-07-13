package publish

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
)

// These are the tests the #2380 fix is actually judged by, so they are written
// against the question the shipped-broken tests never asked:
//
//	"what broken behavior still passes this?"
//
// The old jellyfin TestUpdateArtistPath_RoundTrip asserted that the client SENT a
// Path field to an httptest fake that answered 204 and stored nothing. THE ENTIRE
// BUG still passes that test. So every peer double here STORES STATE and is free
// to LIE about the write exactly the way the real servers were proven to:
//
//	Jellyfin 10.11.10  POST /Items/{id} + Path -> 204, path unchanged
//	Emby      4.9.5.0  POST /Items/{id} + Path -> 204, path unchanged
//
// and each test asserts on the OUTCOME (which item is Stillwater linked to now?)
// rather than on the exit code.

// shortenRelinkPolling collapses the relink's poll budget so the timeout branch
// is exercised in milliseconds instead of the production 20s.
func shortenRelinkPolling(t *testing.T) {
	t.Helper()
	origBudget, origInterval := relinkPollBudget, relinkPollInterval
	relinkPollBudget = 150 * time.Millisecond
	relinkPollInterval = 10 * time.Millisecond
	t.Cleanup(func() {
		relinkPollBudget, relinkPollInterval = origBudget, origInterval
	})
}

// relinkFixture wires a publisher with one connection whose peer is `peer`, an
// artist named "Artist A" linked to peer item `linkedID`, and returns the lister
// so a test can assert which link was ultimately written or dropped.
func relinkFixture(t *testing.T, connType string, peer *fakePeer, linkedID string) (*Publisher, *fakePlatformLister) {
	t.Helper()
	shortenRelinkPolling(t)

	updater := &fakePathUpdater{}
	origU := renamePathUpdaterFactory
	renamePathUpdaterFactory = func(*connection.Connection, *slog.Logger) (pathUpdater, bool) {
		return updater, true
	}
	t.Cleanup(func() { renamePathUpdaterFactory = origU })

	// The pre-flight root guard must PASS so the test reaches the read-back; the
	// guard's own refusal branches are covered in rename_guard_test.go.
	withFakeRootLister(t, fakeRootLister{roots: peer.roots})
	withFakePeer(t, peer)

	lister := &fakePlatformLister{ids: []artist.PlatformID{
		{ArtistID: "a1", ConnectionID: "c1", PlatformArtistID: linkedID},
	}}
	p := New(Deps{
		ArtistService: lister,
		ArtistGetter: &fakeArtistGetter{artists: map[string]*artist.Artist{
			"a1": {ID: "a1", Name: "Artist A"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c1": {
				ID: "c1", Name: "peer", Type: connType, URL: "http://peer", Enabled: true,
				Emby:     &connection.EmbyConfig{PlatformUserID: "u1"},
				Jellyfin: &connection.JellyfinConfig{PlatformUserID: "u1"},
			},
		}},
		Logger: silentLogger(),
	})
	return p, lister
}

// TestRelink_JellyfinIgnoresPath_RewritesLinkToNewItem is THE regression test for
// #2380.
//
// The peer behaves exactly as the live Jellyfin was proven to: it accepts the path
// write, reports success, and does not move the item. Its library scanner then
// re-derives the artist at the new directory as a NEW item with a NEW id, and
// abandons the old one as a metadata-only ghost.
//
// The bug was that Stillwater reported "platform_refresh: ok" and stayed linked to
// the ghost. The fix must leave the link pointing at the new, real item.
func TestRelink_JellyfinIgnoresPath_RewritesLinkToNewItem(t *testing.T) {
	peer := &fakePeer{
		honorsPath: false, // the proven Jellyfin behavior
		storedPath: "/music/Old Name",
		roots:      []string{"/music"},
		items: []connection.PeerArtist{
			{ID: "jf-old", Name: "Artist A", Path: "/music/Old Name"},
		},
		// The library scan is what re-derives the item at the new path. Until it
		// runs, the new directory is invisible -- the scan is asynchronous.
		onScan: func(p *fakePeer) {
			p.items = []connection.PeerArtist{
				// The abandoned original, now a metadata-only ghost outside every
				// library root. Linking to THIS is the corruption.
				{ID: "jf-old", Name: "Artist A", Path: "/config/metadata/artists/Artist A"},
				// The real item the scanner derived at the new directory.
				{ID: "jf-new", Name: "Artist A", Path: "/music/New Name"},
			}
		},
	}
	p, lister := relinkFixture(t, connection.TypeJellyfin, peer, "jf-old")

	results, err := p.SyncRename(context.Background(), "a1", "/music/Old Name", "/music/New Name")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if len(results) != 1 || results[0].Result != artist.PlatformRemapOK {
		t.Fatalf("results = %+v, want one ok entry", results)
	}

	// The assertion the old test could not make: WHICH item are we linked to now?
	if len(lister.directSets) != 1 {
		t.Fatalf("SetPlatformID calls = %d (%+v), want 1 -- the stale link was never rewritten",
			len(lister.directSets), lister.directSets)
	}
	got := lister.directSets[0]
	if got.platformArtistID != "jf-new" {
		t.Errorf("relinked to platform item %q, want %q", got.platformArtistID, "jf-new")
	}
	if got.platformArtistID == "jf-old" {
		t.Error("relinked to the METADATA-ONLY GHOST -- this is the #2380 corruption")
	}
	if got.artistID != "a1" || got.connectionID != "c1" {
		t.Errorf("relink wrote (%s,%s), want (a1,c1)", got.artistID, got.connectionID)
	}
}

// TestRelink_NeverLinksToMetadataGhost isolates the rule that makes the ghost
// unlinkable. The scan surfaces ONLY the abandoned item: it has the artist's
// EXACT NAME, and it is stranded at the metadata directory outside every library
// root -- the shape observed live on Jellyfin.
//
// A relink that name-matched anything would link straight to it and reproduce the
// corruption. What stops it is resolvePeerArtist's requirement that a name match
// carry NO path; a ghost always has one.
//
// Correct behavior: REFUSE TO LINK, keep the link we hold, and report loudly. Note
// what this test deliberately does NOT assert -- that the stale row is dropped.
// This listing is indistinguishable from a peer that simply has not rescanned yet,
// so "it looks like a ghost" cannot license a delete from inside the rename's
// budget. Ghost collection is the background reconciler's job (#2426), where the
// peer has had minutes to settle.
func TestRelink_NeverLinksToMetadataGhost(t *testing.T) {
	peer := &fakePeer{
		honorsPath: false,
		storedPath: "/music/Old Name",
		roots:      []string{"/music"},
		items: []connection.PeerArtist{
			{ID: "jf-old", Name: "Artist A", Path: "/config/metadata/artists/Artist A"},
		},
		onScan: func(*fakePeer) {},
	}
	p, lister := relinkFixture(t, connection.TypeJellyfin, peer, "jf-old")

	results, err := p.SyncRename(context.Background(), "a1", "/music/Old Name", "/music/New Name")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if results[0].Result != artist.PlatformRemapFailed {
		t.Fatalf("Result = %q, want failed: a ghost must never satisfy the relink", results[0].Result)
	}
	if len(lister.directSets) != 0 {
		t.Fatalf("relink wrote a link to %+v; it must never link to a ghost", lister.directSets)
	}
	if len(lister.deletedConnIDs) != 0 {
		t.Errorf("the link was DROPPED (%v). From inside the rename budget this listing is "+
			"indistinguishable from a peer mid-rescan, so it cannot prove the link is dead. "+
			"Ghost collection belongs to the background reconciler (#2426)", lister.deletedConnIDs)
	}
	if results[0].Error == "" {
		t.Error("kept the link but said nothing; an unresolved relink must report loudly")
	}
}

// TestRelink_NeverLinksToNotYetRescannedItem is the case a roots-based ghost
// filter would have WAVED THROUGH, which is why no such filter is in the code.
//
// The peer still shows the artist's item sitting at its OLD directory: the scan
// has not caught up. That item has the right name AND its path is comfortably
// INSIDE the library roots, so "is it a ghost?" answers no. But it is still the
// wrong item to be linked to -- it is the pre-move item, and treating it as the
// resolution is the same stale-link bug in a different hat.
//
// The rule that saves us is the stricter one: a name match must carry NO path at
// all. This item has one, and it is not the directory we moved to, so it is
// refused.
func TestRelink_NeverLinksToNotYetRescannedItem(t *testing.T) {
	peer := &fakePeer{
		honorsPath: false,
		storedPath: "/music/Old Name",
		roots:      []string{"/music"},
		items: []connection.PeerArtist{
			// Right name, INSIDE the roots -- and still the wrong (pre-move) item.
			{ID: "jf-old", Name: "Artist A", Path: "/music/Old Name"},
		},
		onScan: func(*fakePeer) {}, // the scan never catches up within the budget
	}
	p, lister := relinkFixture(t, connection.TypeJellyfin, peer, "jf-old")

	results, err := p.SyncRename(context.Background(), "a1", "/music/Old Name", "/music/New Name")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if results[0].Result != artist.PlatformRemapFailed {
		t.Fatalf("Result = %q, want failed: the item still at the OLD path is not a resolution",
			results[0].Result)
	}
	if len(lister.directSets) != 0 {
		t.Errorf("relinked to %+v, but that item is still sitting at the pre-move path",
			lister.directSets)
	}
}

// TestRelink_ItemMissingFromTheListingKeepsTheLink is the test whose PREVIOUS
// VERSION certified the bug.
//
// It used to be called ...DropsTheLink and asserted the opposite of what it asserts
// now, on this reasoning: "the peer answered with a NON-EMPTY listing that does not
// contain our item. It knows other artists and does not know this ID. That is the
// one unambiguous proof the link is dead."
//
// IT IS NOT PROOF OF ANYTHING. A peer rebuilding its index serves a partial listing:
// it answers 200, reports the artists it has re-derived so far, and has simply not
// reached ours yet. That is byte-for-byte the same observation as "the item is
// gone". The peer is mid-scan far more often than it is amnesiac, because the scan
// takes minutes and this budget is seconds -- so the "unambiguous proof" fired
// routinely on healthy libraries and deleted their links, with nothing to put them
// back.
//
// A non-empty listing missing our item now means exactly what it can support: NOT
// RESOLVED YET. Keep the link, report loudly, and leave the drop to the reconciler
// that can afford to wait (#2426).
func TestRelink_ItemMissingFromTheListingKeepsTheLink(t *testing.T) {
	peer := &fakePeer{
		honorsPath: false,
		storedPath: "/music/Old Name",
		roots:      []string{"/music"},
		items: []connection.PeerArtist{
			// The peer reports OTHER artists but not jf-old. Gone, or just not
			// re-derived yet? From here those are the same observation.
			{ID: "jf-someone", Name: "Somebody Else", Path: "/music/Somebody Else"},
		},
		onScan: func(*fakePeer) {},
	}
	p, lister := relinkFixture(t, connection.TypeJellyfin, peer, "jf-old")

	results, err := p.SyncRename(context.Background(), "a1", "/music/Old Name", "/music/New Name")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if results[0].Result != artist.PlatformRemapFailed {
		t.Fatalf("Result = %q, want failed", results[0].Result)
	}
	if peer.scans == 0 {
		t.Error("no library scan was triggered; the relink must ask the peer to re-scan")
	}
	if len(lister.deletedConnIDs) != 0 {
		t.Errorf("the link was DROPPED (%v) because our item was absent from ONE listing. "+
			"A peer mid-rebuild serves a partial listing that looks identical, and it is the "+
			"likelier case -- this is the exact inference that deleted good links twice",
			lister.deletedConnIDs)
	}
	if results[0].Error == "" {
		t.Error("failed result carries no error string; the operator gets no remedy")
	}
}

// TestRelink_NotYetRescannedItemKeepsTheLink is the #2426 case, and the one that
// made the previous two versions of this code delete good links.
//
// The peer still reports our item at its OLD path, INSIDE its library roots. That is
// what a peer that has not finished rescanning looks like -- and it is exactly what
// an abandoned ghost looks like too, from the listing alone. The roots are what tell
// them apart: a real library-backed item is inside them; a ghost is parked outside
// them (/config/metadata/artists/...). The poll budget is 20s and a real library scan
// takes minutes, so THIS IS THE NORMAL JELLYFIN RENAME PATH, not an edge case.
// Dropping here deletes the link of essentially every renamed artist on a large
// library.
func TestRelink_NotYetRescannedItemKeepsTheLink(t *testing.T) {
	peer := &fakePeer{
		honorsPath: false,
		storedPath: "/music/Old Name",
		roots:      []string{"/music"},
		items: []connection.PeerArtist{
			// Still at the old path, still INSIDE the roots: the peer is mid-scan.
			{ID: "jf-old", Name: "Artist A", Path: "/music/Old Name"},
		},
		onScan: func(*fakePeer) {}, // the scan never completes within the budget
	}
	p, lister := relinkFixture(t, connection.TypeJellyfin, peer, "jf-old")

	results, err := p.SyncRename(context.Background(), "a1", "/music/Old Name", "/music/New Name")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if len(lister.deletedConnIDs) != 0 {
		t.Errorf("the link was DROPPED (%v) for an item still INSIDE the peer's library roots. "+
			"That is a peer that has not finished rescanning, not an abandoned ghost -- and nothing "+
			"re-establishes a dropped link automatically", lister.deletedConnIDs)
	}
	if results[0].Result != artist.PlatformRemapFailed {
		t.Errorf("Result = %q, want failed -- an unverified relink is not a success", results[0].Result)
	}
}

// TestRelink_EmptyListingKeepsTheLink: a peer mid-rebuild answers 200 with ZERO
// artists, and an Emby /Artists query is user-scoped, so a bad platform user id does
// the same. Reading that as "our item is gone" would delete the link of EVERY artist
// renamed in that window. guardPlatformPath fails closed on zero roots; this must
// fail closed on zero artists.
func TestRelink_EmptyListingKeepsTheLink(t *testing.T) {
	peer := &fakePeer{
		honorsPath: false,
		storedPath: "/music/Old Name",
		roots:      []string{"/music"},
		items:      nil, // 200 OK, zero artists
		onScan:     func(*fakePeer) {},
	}
	p, lister := relinkFixture(t, connection.TypeJellyfin, peer, "jf-old")

	if _, err := p.SyncRename(context.Background(), "a1", "/music/Old Name", "/music/New Name"); err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if len(lister.deletedConnIDs) != 0 {
		t.Errorf("the link was DROPPED (%v) on an EMPTY peer listing. An empty listing proves nothing; "+
			"treating it as proof of absence deletes every link renamed while the peer is rebuilding",
			lister.deletedConnIDs)
	}
}

// TestRelink_KeepCurrentStillRequiresANameMatch: an already-WRONG link must not be
// ratified. An earlier version kept the current link on ID presence alone, so a
// mislink to another artist's item self-confirmed forever and the relink could no
// longer repair it -- only rubber-stamp it.
func TestRelink_KeepCurrentStillRequiresANameMatch(t *testing.T) {
	got, err := resolvePeerArtist([]connection.PeerArtist{
		{ID: "cur", Name: "Some Other Artist", Path: ""}, // what we are (wrongly) linked to
		{ID: "right", Name: "Artist A", Path: ""},        // the artist we actually are
	}, "/music/Artist A", "Artist A", "cur", true)
	if err != nil {
		t.Fatalf("resolvePeerArtist: %v", err)
	}
	if got == "cur" {
		t.Error("a link pointing at ANOTHER artist's item was ratified because the ID was present. " +
			"Keep-current must satisfy the same name invariant as a fresh link, or a bad link is permanent")
	}
	if got != "right" {
		t.Errorf("resolved to %q, want the correctly-named item %q", got, "right")
	}
}

// TestRelink_PathTieDoesNotHandTheLinkToAnImpostor: two items can sit at the target
// path (Jellyfin transiently carries both the old and the re-derived item). An
// earlier version returned whichever came FIRST in listing order, with no name check
// -- handing the link to an item with a completely different name.
func TestRelink_PathTieDoesNotHandTheLinkToAnImpostor(t *testing.T) {
	got, err := resolvePeerArtist([]connection.PeerArtist{
		{ID: "impostor", Name: "Totally Different", Path: "/music/New"},
		{ID: "cur", Name: "Artist A", Path: "/music/New"},
	}, "/music/New", "Artist A", "cur", true)
	if err != nil {
		t.Fatalf("resolvePeerArtist: %v", err)
	}
	if got == "impostor" {
		t.Error("the link was handed to an arbitrary first-at-path item with a different name")
	}
	if got != "cur" {
		t.Errorf("resolved to %q, want the link we already correctly held (%q)", got, "cur")
	}
}

// TestRelink_EmbyResolvesByName is the Emby-shaped case. Emby exposes NO path for
// any artist (proven live: every MusicArtist reports Path: null), so a path-keyed
// re-resolve cannot work there and the name is the only identity key available.
//
// An empty path must therefore NOT read as "ghost" -- if it did, no Emby item
// would ever be linkable.
func TestRelink_EmbyResolvesByName(t *testing.T) {
	peer := &fakePeer{
		honorsPath: false, // proven Emby behavior
		storedPath: "",    // Emby reports no path at all
		roots:      []string{"/share/Music"},
		items: []connection.PeerArtist{
			{ID: "emby-1", Name: "Artist A", Path: ""},
			{ID: "emby-2", Name: "Someone Else", Path: ""},
		},
	}
	p, lister := relinkFixture(t, connection.TypeEmby, peer, "emby-stale")

	results, err := p.SyncRename(context.Background(), "a1", "/music/Old Name", "/share/Music/New Name")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if results[0].Result != artist.PlatformRemapOK {
		t.Fatalf("Result = %q (err=%q), want ok", results[0].Result, results[0].Error)
	}
	if len(lister.directSets) != 1 || lister.directSets[0].platformArtistID != "emby-1" {
		t.Errorf("relinked to %+v, want emby-1 (matched by name, since Emby exposes no paths)",
			lister.directSets)
	}
}

// TestRelink_AmbiguousNameRefuses: two live items share the artist's name, so the
// correct one cannot be known. Guessing here would corrupt the link half the time,
// so the relink must refuse and drop rather than pick.
func TestRelink_AmbiguousNameRefuses(t *testing.T) {
	peer := &fakePeer{
		honorsPath: false,
		storedPath: "",
		roots:      []string{"/share/Music"},
		items: []connection.PeerArtist{
			{ID: "emby-1", Name: "Artist A", Path: ""},
			{ID: "emby-2", Name: "Artist A", Path: ""},
		},
	}
	p, lister := relinkFixture(t, connection.TypeEmby, peer, "emby-stale")

	results, err := p.SyncRename(context.Background(), "a1", "/music/Old Name", "/share/Music/New Name")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if results[0].Result != artist.PlatformRemapFailed {
		t.Fatalf("Result = %q, want failed on an ambiguous name match", results[0].Result)
	}
	if len(lister.directSets) != 0 {
		t.Errorf("relink guessed a link (%+v) despite an ambiguous name match", lister.directSets)
	}
}

// TestRelink_LidarrPathMismatchIsAFailure guards the per-platform split. Lidarr DOES
// store paths, so a read-back mismatch there is a genuine peer fault (a Root Folder
// coercion) that the operator must see -- it must NOT be quietly treated as the
// ignore-the-field behavior the media servers exhibit and swept into a relink.
func TestRelink_LidarrPathMismatchIsAFailure(t *testing.T) {
	peer := &fakePeer{
		honorsPath: false, // Lidarr coerced our path to something else
		storedPath: "/coerced/elsewhere",
		roots:      []string{"/music"},
	}
	p, lister := relinkFixture(t, connection.TypeLidarr, peer, "42")

	results, err := p.SyncRename(context.Background(), "a1", "/music/Old Name", "/music/New Name")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if results[0].Result != artist.PlatformRemapFailed {
		t.Fatalf("Result = %q, want failed: Lidarr storing a different path is a real fault", results[0].Result)
	}
	if !strings.Contains(results[0].Error, "/coerced/elsewhere") {
		t.Errorf("Error = %q, want it to name the path the peer actually stored", results[0].Error)
	}
	if peer.scans != 0 {
		t.Error("a Lidarr mismatch must not trigger a library-scan relink; it is a fault to surface")
	}
	if len(lister.deletedConnIDs) != 0 {
		t.Error("a Lidarr coercion must not drop the link; the item is still the right item")
	}
}

// TestRelink_HonoredPathNeedsNoRelink is the negative control: when the peer really
// does store what we sent (Lidarr's normal behavior), nothing else should happen --
// no scan, no link rewrite. Without this, a fix that relinked unconditionally would
// pass every other test in this file.
func TestRelink_HonoredPathNeedsNoRelink(t *testing.T) {
	updater := &fakePathUpdater{}
	peer := &fakePeer{honorsPath: true, updater: updater, roots: []string{"/music"}}

	p, lister := relinkFixture(t, connection.TypeLidarr, peer, "42")
	// relinkFixture installed its own updater; re-point the factory at ours so the
	// peer echoes the path this test's updater recorded.
	origU := renamePathUpdaterFactory
	renamePathUpdaterFactory = func(*connection.Connection, *slog.Logger) (pathUpdater, bool) {
		return updater, true
	}
	t.Cleanup(func() { renamePathUpdaterFactory = origU })

	results, err := p.SyncRename(context.Background(), "a1", "/music/Old Name", "/music/New Name")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if results[0].Result != artist.PlatformRemapOK {
		t.Fatalf("Result = %q (err=%q), want ok", results[0].Result, results[0].Error)
	}
	if peer.scans != 0 {
		t.Errorf("triggered %d library scans on a peer that honored the write; want 0", peer.scans)
	}
	if len(lister.directSets) != 0 {
		t.Errorf("rewrote the link (%+v) though the peer honored the path; want no rewrite",
			lister.directSets)
	}
}

// TestRelink_ReadBackFailureDoesNotReportOK: the peer is unreachable for the
// read-back. "Could not verify" must never decay into "assume it worked" -- that
// decay is the entire #2380 failure mode.
func TestRelink_ReadBackFailureDoesNotReportOK(t *testing.T) {
	peer := &fakePeer{
		honorsPath: false,
		roots:      []string{"/music"},
		readErr:    errors.New("connection refused"),
	}
	p, _ := relinkFixture(t, connection.TypeJellyfin, peer, "jf-old")

	results, err := p.SyncRename(context.Background(), "a1", "/music/Old Name", "/music/New Name")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if results[0].Result != artist.PlatformRemapOK {
		return // correct: unverifiable must not report ok
	}
	t.Fatal("reported ok despite being unable to read the path back from the peer")
}

// --- unit-level checks on the matcher itself ---

func TestResolvePeerArtist(t *testing.T) {
	tests := []struct {
		name     string
		items    []connection.PeerArtist
		wantPath string
		wantName string
		wantID   string
		wantErr  bool
	}{
		{
			name: "path match wins over a same-named ghost",
			items: []connection.PeerArtist{
				{ID: "ghost", Name: "A", Path: "/config/metadata/artists/A"},
				{ID: "real", Name: "A", Path: "/music/New"},
			},
			wantPath: "/music/New", wantName: "A", wantID: "real",
		},
		{
			name: "ghost alone never matches",
			items: []connection.PeerArtist{
				{ID: "ghost", Name: "A", Path: "/config/metadata/artists/A"},
			},
			wantPath: "/music/New", wantName: "A", wantID: "",
		},
		{
			name: "pathless item matches by name (the Emby case)",
			items: []connection.PeerArtist{
				{ID: "emby-1", Name: "A", Path: ""},
			},
			wantPath: "/music/New", wantName: "A", wantID: "emby-1",
		},
		{
			name: "an item with a DIFFERENT real path is not a name match",
			items: []connection.PeerArtist{
				{ID: "other", Name: "A", Path: "/music/Somewhere Else"},
			},
			wantPath: "/music/New", wantName: "A", wantID: "",
		},
		{
			name: "trailing-slash difference still matches",
			items: []connection.PeerArtist{
				{ID: "real", Name: "A", Path: "/music/New/"},
			},
			wantPath: "/music/New", wantName: "A", wantID: "real",
		},
		{
			name: "ambiguous name match errors rather than guessing",
			items: []connection.PeerArtist{
				{ID: "one", Name: "A", Path: ""},
				{ID: "two", Name: "A", Path: ""},
			},
			wantPath: "/music/New", wantName: "A", wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolvePeerArtist(tc.items, tc.wantPath, tc.wantName, "", true)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolvePeerArtist = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolvePeerArtist: %v", err)
			}
			if got != tc.wantID {
				t.Errorf("resolvePeerArtist = %q, want %q", got, tc.wantID)
			}
		})
	}
}

// TestSamePeerPath_EmptyIsNeverEqual pins the property the read-back verifier
// leans on: Emby returns NO path for artists, and "no path" must read as "did not
// honor the write", never as a match. An == comparison on two empty strings would
// silently report every Emby rename as verified.
func TestSamePeerPath_EmptyIsNeverEqual(t *testing.T) {
	if connection.SamePeerPath("", "") {
		t.Error(`SamePeerPath("","") = true; an absent path must never verify as a match`)
	}
	if connection.SamePeerPath("", "/music/X") {
		t.Error("an empty read-back must not verify against a real sent path")
	}
	if !connection.SamePeerPath("/music/X/", "/music/X") {
		t.Error("trailing-slash-only difference should compare equal")
	}
	if connection.SamePeerPath("/music/x", "/music/X") {
		t.Error("comparison must stay case-sensitive (Linux peers)")
	}
}

// TestRelink_RenamePathNeverDropsTheLink is THE fix-guard for #2380's final policy,
// and it is written to fail for any patch that reintroduces a drop through ANY
// branch -- which is how this bug came back twice.
//
// Both previous fixes removed the unsound drop from one branch and left it alive in
// another, so a test that pinned a single scenario kept passing while the deletion
// simply moved. This one sweeps EVERY failure shape the rename path can reach and
// asserts the same two things about all of them:
//
//	the link SURVIVES, and the operator is TOLD.
//
// The peer states below are deliberately the ones that most "look like" a dead link.
// Every one of them is also exactly what a peer mid-rescan looks like, which is the
// whole point: from inside the rename's budget the two are the same observation, and
// the mid-rescan case is the COMMON one (minutes of scan against seconds of budget).
func TestRelink_RenamePathNeverDropsTheLink(t *testing.T) {
	tests := []struct {
		name string
		peer *fakePeer
		// why this state is NOT proof the link is dead
		because string
	}{
		{
			name: "item absent from a non-empty listing",
			peer: &fakePeer{
				items:  []connection.PeerArtist{{ID: "jf-other", Name: "Somebody Else", Path: "/music/Somebody Else"}},
				onScan: func(*fakePeer) {},
			},
			because: "a peer rebuilding its index serves a PARTIAL listing that looks identical",
		},
		{
			name: "ghost-shaped item outside the library roots",
			peer: &fakePeer{
				items:  []connection.PeerArtist{{ID: "jf-old", Name: "Artist A", Path: "/config/metadata/artists/Artist A"}},
				onScan: func(*fakePeer) {},
			},
			because: "the peer may still re-derive this item into the library; a 20s budget cannot outlast a multi-minute scan",
		},
		{
			name: "pathless item on a folder-backed peer",
			peer: &fakePeer{
				items:  []connection.PeerArtist{{ID: "jf-old", Name: "Artist A", Path: ""}},
				onScan: func(*fakePeer) {},
			},
			because: "an item mid-rescan can report no path transiently",
		},
		{
			name: "item still at its pre-move path",
			peer: &fakePeer{
				items:  []connection.PeerArtist{{ID: "jf-old", Name: "Artist A", Path: "/music/Old Name"}},
				onScan: func(*fakePeer) {},
			},
			because: "this is the textbook not-yet-rescanned peer",
		},
		{
			name: "empty listing",
			peer: &fakePeer{
				items:  nil,
				onScan: func(*fakePeer) {},
			},
			because: "a peer mid-rebuild answers 200 with zero artists, as does a user-scoped Emby query with a bad user id",
		},
		{
			name: "peer unreachable",
			peer: &fakePeer{
				listErr: errors.New("connection refused"),
				onScan:  func(*fakePeer) {},
			},
			because: "a network blip tells us nothing about the link",
		},
		{
			name: "scan trigger refused",
			peer: &fakePeer{
				items:   []connection.PeerArtist{{ID: "jf-old", Name: "Artist A", Path: "/music/Old Name"}},
				scanErr: errors.New("peer returned 503"),
				onScan:  func(*fakePeer) {},
			},
			because: "we never even enumerated the peer",
		},
		{
			// Both carry the artist's name, so the unique-name tiebreak in
			// resolvePathHits cannot separate them and the relink refuses. (Two items
			// at the path with DIFFERENT names is not this case -- the name tiebreak
			// resolves that one cleanly.)
			name: "ambiguous candidates",
			peer: &fakePeer{
				items: []connection.PeerArtist{
					{ID: "jf-a", Name: "Artist A", Path: "/music/New Name"},
					{ID: "jf-b", Name: "Artist A", Path: "/music/New Name"},
				},
				onScan: func(*fakePeer) {},
			},
			because: "failing to CHOOSE between candidates is not evidence against what we hold",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.peer.honorsPath = false
			tc.peer.storedPath = "/music/Old Name"
			tc.peer.roots = []string{"/music"}

			p, lister := relinkFixture(t, connection.TypeJellyfin, tc.peer, "jf-old")

			results, err := p.SyncRename(context.Background(), "a1", "/music/Old Name", "/music/New Name")
			if err != nil {
				t.Fatalf("SyncRename: %v", err)
			}

			// 1. THE LINK SURVIVES.
			if len(lister.deletedConnIDs) != 0 {
				t.Errorf("the rename path DROPPED the link (%v). It must never drop: %s. "+
					"Nothing re-establishes a dropped link automatically, so a wrong drop silently "+
					"stops every future push for this artist",
					lister.deletedConnIDs, tc.because)
			}

			// 2. AND THE OPERATOR IS TOLD. A silent no-op is the other way to fail
			// this: keeping the link while reporting ok would hide the unresolved
			// state instead of surfacing it.
			if results[0].Result != artist.PlatformRemapFailed {
				t.Errorf("Result = %q, want failed -- an unresolved relink must not report ok",
					results[0].Result)
			}
			if results[0].Error == "" {
				t.Error("kept the link but reported no error; the operator has no idea anything is wrong")
			}
		})
	}
}

// --- #2380 adversarial-review fixes: the rename path KEEPS, it never drops ---
//
// The original policy dropped the link on ANY relink failure. That made a network
// blip, a slow peer, or a database hiccup MORE destructive than the bug this file
// exists to fix: there is no scheduled connection sync to re-establish a dropped
// link, so it silently stops every future push for that artist until a human runs
// a library scan by hand. These tests pin the corrected policy.

// TestRelink_SameArtistInTwoLibrariesIsNotAmbiguous is the production topology:
// a peer with TWO music roots (e.g. /music and /classical). The peer's artist
// endpoint is queried per-library and returns the SAME global artist entity once
// per library -- same ID, and on Emby both are pathless. Counting that as two
// candidates made it "ambiguous" and DELETED the link of any artist with tracks in
// both roots, for an item the peer never even touched.
func TestRelink_SameArtistInTwoLibrariesIsNotAmbiguous(t *testing.T) {
	got, err := resolvePeerArtist([]connection.PeerArtist{
		{ID: "emby-7", Name: "Bjork", Path: ""}, // as returned for /music
		{ID: "emby-7", Name: "Bjork", Path: ""}, // the SAME artist, as returned for /classical
	}, "/music/Bjork", "Bjork", "", true)
	if err != nil {
		t.Fatalf("one artist reported once per library must not be ambiguous, got err = %v", err)
	}
	if got != "emby-7" {
		t.Errorf("resolved to %q, want %q", got, "emby-7")
	}
}

// TestRelink_KeepsAValidExistingLink covers the Emby shape: the artist item is
// name-keyed and pathless, so it SURVIVES a directory rename unchanged and the link
// we already hold is still correct. Re-deriving it from name-uniqueness can only
// lose -- so a still-valid link must be kept, even when the peer reports another
// artist sharing the name (which would otherwise read as "ambiguous").
func TestRelink_KeepsAValidExistingLink(t *testing.T) {
	got, err := resolvePeerArtist([]connection.PeerArtist{
		{ID: "emby-1", Name: "Ambiguous Name", Path: ""}, // the one we are linked to
		{ID: "emby-2", Name: "Ambiguous Name", Path: ""}, // a genuine name collision
	}, "/music/Whatever", "Ambiguous Name", "emby-1", true)
	if err != nil {
		t.Fatalf("a link we already hold and that the peer still reports must not be re-litigated: %v", err)
	}
	if got != "emby-1" {
		t.Errorf("resolved to %q, want the link we already held (%q)", got, "emby-1")
	}
}

// TestRelink_UnreachablePeerKeepsTheLink: the peer is down. We learn NOTHING about
// whether our link is good, so it must survive. Dropping here would mean a blip
// during a rename costs the user their link, with no automatic way back.
func TestRelink_UnreachablePeerKeepsTheLink(t *testing.T) {
	peer := &fakePeer{
		honorsPath: false,
		storedPath: "/music/Old Name",
		roots:      []string{"/music"},
		listErr:    errors.New("connection refused"),
		onScan:     func(*fakePeer) {},
	}
	p, lister := relinkFixture(t, connection.TypeJellyfin, peer, "jf-old")

	results, err := p.SyncRename(context.Background(), "a1", "/music/Old Name", "/music/New Name")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if results[0].Result != artist.PlatformRemapFailed {
		t.Errorf("Result = %q, want failed -- an unverifiable relink is not a success", results[0].Result)
	}
	if len(lister.deletedConnIDs) != 0 {
		t.Errorf("the link was DROPPED (%v) because the peer was unreachable. We never proved it was stale; "+
			"a network blip must not destroy a link that has no automatic path back", lister.deletedConnIDs)
	}
	if results[0].Error == "" {
		t.Error("an unverified relink must report loudly, not fail silently")
	}
}

// TestRelink_ScanTriggerFailureKeepsTheLink: we could not even ask the peer to
// rescan. That says nothing about the link.
func TestRelink_ScanTriggerFailureKeepsTheLink(t *testing.T) {
	peer := &fakePeer{
		honorsPath: false,
		storedPath: "/music/Old Name",
		roots:      []string{"/music"},
		items:      []connection.PeerArtist{{ID: "jf-old", Name: "Nobody", Path: "/music/Old Name"}},
		scanErr:    errors.New("peer returned 503"),
		onScan:     func(*fakePeer) {},
	}
	p, lister := relinkFixture(t, connection.TypeJellyfin, peer, "jf-old")

	if _, err := p.SyncRename(context.Background(), "a1", "/music/Old Name", "/music/New Name"); err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if len(lister.deletedConnIDs) != 0 {
		t.Errorf("the link was DROPPED (%v) because the SCAN TRIGGER failed. We never enumerated the peer, "+
			"so we have no evidence the link is stale", lister.deletedConnIDs)
	}
}

// TestRelink_DBWriteFailureKeepsTheLink is the worst of the old policy: we resolved
// the CORRECT item, our own database refused the write, and the code then DELETED
// the link. Destroying good data to punish an unrelated failure.
func TestRelink_DBWriteFailureKeepsTheLink(t *testing.T) {
	peer := &fakePeer{
		honorsPath: false,
		storedPath: "/music/Old Name",
		roots:      []string{"/music"},
		items:      []connection.PeerArtist{{ID: "jf-new", Name: "Nobody", Path: "/music/New Name"}},
		onScan:     func(*fakePeer) {},
	}
	p, lister := relinkFixture(t, connection.TypeJellyfin, peer, "jf-old")
	lister.directSetErr = errors.New("database is locked")

	results, err := p.SyncRename(context.Background(), "a1", "/music/Old Name", "/music/New Name")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if results[0].Result != artist.PlatformRemapFailed {
		t.Errorf("Result = %q, want failed", results[0].Result)
	}
	if len(lister.deletedConnIDs) != 0 {
		t.Errorf("the link was DROPPED (%v) because the DB write failed -- after we had resolved the RIGHT "+
			"item. A storage hiccup must never delete a link", lister.deletedConnIDs)
	}
}

// TestRelink_PathlessGhostOnAFolderBackedPeerIsRefused closes the hole the second
// adversarial review found: the entire ghost defense rested on "a ghost always has a
// path", and TestRelink_NeverLinksToMetadataGhost passed only because its fixture
// happened to give the ghost one. Flip the ghost to PATHLESS and the old rule linked
// straight to it and reported green -- the #2380 corruption, certified by a passing
// test.
//
// Jellyfin is FOLDER-BACKED: a healthy artist always has a path, so a pathless item
// is an abandoned metadata-only entity. Only Emby is pathless by design.
func TestRelink_PathlessGhostOnAFolderBackedPeerIsRefused(t *testing.T) {
	ghost := []connection.PeerArtist{
		{ID: "jf-ghost", Name: "Artist A", Path: ""}, // no path, no library folder
	}

	// Folder-backed peer (Jellyfin): pathless is anomalous -> must NOT be linked.
	got, err := resolvePeerArtist(ghost, "/music/Artist A", "Artist A", "", false)
	if err != nil {
		t.Fatalf("resolvePeerArtist: %v", err)
	}
	if got != "" {
		t.Errorf("linked to %q: a PATHLESS item on a folder-backed peer is a ghost with no library folder "+
			"behind it, and linking to it is the #2380 corruption", got)
	}

	// And it must not survive as the CURRENT link either -- a ghost we already point
	// at is precisely what the relink exists to clear.
	got, err = resolvePeerArtist(ghost, "/music/Artist A", "Artist A", "jf-ghost", false)
	if err != nil {
		t.Fatalf("resolvePeerArtist: %v", err)
	}
	if got == "jf-ghost" {
		t.Error("the existing link to a PATHLESS ghost was ratified on a folder-backed peer")
	}

	// Emby, by contrast, reports EVERY artist pathless. There the same shape is the
	// normal case and must resolve.
	got, err = resolvePeerArtist(ghost, "/music/Artist A", "Artist A", "", true)
	if err != nil {
		t.Fatalf("resolvePeerArtist (emby): %v", err)
	}
	if got != "jf-ghost" {
		t.Errorf("on a pathless-by-design peer (Emby) a pathless name match MUST resolve; got %q", got)
	}
}

// TestRelinkResolverFactory_ProductionDispatch exercises the production
// relinkResolverFactory (not the fake withFakePeer swaps for the rest of
// this file) for all three supported connection types plus an unsupported
// one, and drives each real adapter's methods against a closed httptest
// server. Mirrors TestSyncRename_FactoryProductionDispatch's rationale: the
// GetArtistPath/ListLibraryArtists/TriggerLibraryScan adapter methods and the
// factory's switch are production code with no other route to coverage,
// since every relink behavior test injects a fake peerArtistResolver.
func TestRelinkResolverFactory_ProductionDispatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Close()

	logger := silentLogger()

	cases := []struct {
		name     string
		connType string
		wantOK   bool
	}{
		{"emby", connection.TypeEmby, true},
		{"jellyfin", connection.TypeJellyfin, true},
		{"lidarr", connection.TypeLidarr, true},
		{"unsupported", "sonarr", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := &connection.Connection{
				ID: "c1", Name: "peer", Type: tc.connType, URL: srv.URL,
				Emby:     &connection.EmbyConfig{PlatformUserID: "u1"},
				Jellyfin: &connection.JellyfinConfig{PlatformUserID: "u1"},
			}
			resolver, ok := relinkResolverFactory(conn, logger)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				if resolver != nil {
					t.Errorf("resolver = %+v, want nil for an unsupported type", resolver)
				}
				return
			}

			ctx := context.Background()
			if _, err := resolver.GetArtistPath(ctx, "p1"); err == nil {
				t.Error("GetArtistPath against a closed server: want error, got nil")
			}
			if _, err := resolver.ListLibraryArtists(ctx); err == nil {
				t.Error("ListLibraryArtists against a closed server: want error, got nil")
			}
			// Lidarr has no server-wide scan primitive and its adapter is a
			// documented no-op (see lidarrResolver.TriggerLibraryScan), so it
			// must succeed even against a dead server; Emby/Jellyfin must not.
			err := resolver.TriggerLibraryScan(ctx)
			if tc.connType == connection.TypeLidarr {
				if err != nil {
					t.Errorf("lidarr TriggerLibraryScan: want nil no-op, got %v", err)
				}
			} else if err == nil {
				t.Errorf("%s TriggerLibraryScan against a closed server: want error, got nil", tc.connType)
			}
		})
	}
}

// TestRelink_CtxCanceledDuringPollKeepsTheLink covers the ctx.Done() exit of
// relinkArtist's poll loop (as opposed to the poll-BUDGET exit, which the
// ...NotYetRescannedItemKeepsTheLink tests already cover): the outer
// per-connection deadline (renameSyncTimeout) expires while the peer still
// has not surfaced the moved item, and the link must be kept, not dropped.
func TestRelink_CtxCanceledDuringPollKeepsTheLink(t *testing.T) {
	origTimeout := renameSyncTimeout
	renameSyncTimeout = 30 * time.Millisecond
	t.Cleanup(func() { renameSyncTimeout = origTimeout })

	peer := &fakePeer{
		honorsPath: false,
		storedPath: "/music/Old Name",
		roots:      []string{"/music"},
		items:      nil, // never resolves
		onScan:     func(*fakePeer) {},
	}
	p, lister := relinkFixture(t, connection.TypeJellyfin, peer, "jf-old")
	// relinkFixture's shortenRelinkPolling sets a 150ms budget; widen it so the
	// outer ctx (30ms) cancels first, isolating the ctx.Done() branch from the
	// deadline-ticker branch. Registered after relinkFixture's own cleanup, so
	// it restores to the fixture's 150ms/10ms before that cleanup restores the
	// package defaults.
	origBudget, origInterval := relinkPollBudget, relinkPollInterval
	relinkPollBudget = 5 * time.Second
	relinkPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { relinkPollBudget, relinkPollInterval = origBudget, origInterval })

	results, err := p.SyncRename(context.Background(), "a1", "/music/Old Name", "/music/New Name")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if results[0].Result != artist.PlatformRemapFailed {
		t.Fatalf("Result = %q, want failed", results[0].Result)
	}
	if !strings.Contains(results[0].Error, "did not surface") {
		t.Errorf("Error = %q, want the ctx-canceled poll message", results[0].Error)
	}
	if len(lister.directSets) != 0 || len(lister.deletedConnIDs) != 0 {
		t.Errorf("link was mutated (sets=%v deletes=%v) on ctx cancellation; it must be kept",
			lister.directSets, lister.deletedConnIDs)
	}
}

// TestRelink_PollErrorKeepsTheLink covers the in-loop pollErr branch of
// relinkArtist: the FIRST resolve attempt (before a scan is even triggered)
// succeeds with no match, but a SUBSEQUENT enumerate (inside the ticker loop)
// fails. That is distinct from TestRelink_UnreachablePeerKeepsTheLink, which
// fails on the pre-scan resolve and never reaches the loop at all.
func TestRelink_PollErrorKeepsTheLink(t *testing.T) {
	peer := &fakePeer{
		honorsPath: false,
		storedPath: "/music/Old Name",
		roots:      []string{"/music"},
		items:      nil, // pre-scan resolve: no match, no error
		onScan: func(p *fakePeer) {
			// The scan "succeeds" but the peer starts erroring on the next
			// enumerate -- e.g. it fell over mid-rebuild.
			p.listErr = errors.New("peer index rebuild failed")
		},
	}
	p, lister := relinkFixture(t, connection.TypeJellyfin, peer, "jf-old")

	results, err := p.SyncRename(context.Background(), "a1", "/music/Old Name", "/music/New Name")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if results[0].Result != artist.PlatformRemapFailed {
		t.Fatalf("Result = %q, want failed", results[0].Result)
	}
	if !strings.Contains(results[0].Error, "enumerate") {
		t.Errorf("Error = %q, want the enumerate-failure message", results[0].Error)
	}
	if len(lister.directSets) != 0 || len(lister.deletedConnIDs) != 0 {
		t.Errorf("link was mutated (sets=%v deletes=%v) on a poll enumerate error; it must be kept",
			lister.directSets, lister.deletedConnIDs)
	}
}

// TestResolvePathHits covers the two branches TestRelink_PathTieDoesNotHandTheLinkToAnImpostor
// and the ambiguous-name tests do not reach directly: preferring the
// already-held link among multiple path hits, and falling back to a unique
// name match when the held link is not among them.
func TestResolvePathHits(t *testing.T) {
	hits := []connection.PeerArtist{
		{ID: "jf-a", Name: "Artist A", Path: "/music/Artist A"},
		{ID: "jf-b", Name: "Artist B", Path: "/music/Artist A"},
	}

	t.Run("prefers the currently held link", func(t *testing.T) {
		got, err := resolvePathHits(hits, "/music/Artist A", "", "jf-b")
		if err != nil {
			t.Fatalf("resolvePathHits: %v", err)
		}
		if got != "jf-b" {
			t.Errorf("got %q, want the currently held link jf-b", got)
		}
	})

	t.Run("falls back to a unique name match", func(t *testing.T) {
		got, err := resolvePathHits(hits, "/music/Artist A", "Artist A", "jf-c")
		if err != nil {
			t.Fatalf("resolvePathHits: %v", err)
		}
		if got != "jf-a" {
			t.Errorf("got %q, want the unique name match jf-a", got)
		}
	})
}

// TestCommitRelink_SamePlatformIDIsANoOp covers commitRelink's short-circuit:
// when the peer's re-resolved item ID equals the one already stored, nothing
// is written -- asserted here via a Publisher whose ArtistService would fail
// the test if SetPlatformID were called at all.
func TestCommitRelink_SamePlatformIDIsANoOp(t *testing.T) {
	lister := &fakePlatformLister{ids: []artist.PlatformID{
		{ArtistID: "a1", ConnectionID: "c1", PlatformArtistID: "p1"},
	}}
	p := New(Deps{ArtistService: lister, Logger: silentLogger()})

	err := p.commitRelink(context.Background(), &connection.Connection{ID: "c1", Type: connection.TypeJellyfin}, "a1", "p1", "p1")
	if err != nil {
		t.Fatalf("commitRelink: %v", err)
	}
	if len(lister.directSets) != 0 {
		t.Errorf("SetPlatformID called (%v) for an unchanged platform ID; commitRelink must no-op", lister.directSets)
	}
}
