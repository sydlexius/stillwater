package publish

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
)

// relinkReconcileFixture wires a publisher with one Jellyfin connection whose
// peer is `peer`, an artist at "/music/New Name" linked to peer item
// `linkedID`, and returns the platform lister so a test can assert which
// link (if any) the reconciler wrote.
func relinkReconcileFixture(t *testing.T, peer *fakePeer, linkedID string) (*Publisher, *fakePlatformLister) {
	t.Helper()
	return relinkReconcileFixtureForType(t, peer, linkedID, connection.TypeJellyfin)
}

// relinkReconcileFixtureForType is relinkReconcileFixture with the peer's TYPE
// as a parameter, because type is not a cosmetic detail here: it selects
// peerIsPathless, and that flag decides whether a pathless name match may
// resolve at all. A ghost/ambiguity test pinned only against Jellyfin measures
// the EASY case -- there a pathless item is already rejected outright -- and
// says nothing about Emby, where every item is pathless by design and the name
// is the only key that exists. See TestReconcileRelinks_Emby_* below.
func relinkReconcileFixtureForType(
	t *testing.T, peer *fakePeer, linkedID, connType string,
) (*Publisher, *fakePlatformLister) {
	t.Helper()
	orig := relinkResolverFactory
	relinkResolverFactory = func(_ *connection.Connection, _ *slog.Logger) (peerArtistResolver, bool) {
		return peer, true
	}
	t.Cleanup(func() { relinkResolverFactory = orig })

	lister := &fakePlatformLister{ids: []artist.PlatformID{
		{ArtistID: "a1", ConnectionID: "c1", PlatformArtistID: linkedID},
	}}
	p := New(Deps{
		ArtistService: lister,
		ArtistGetter: &fakeArtistGetter{artists: map[string]*artist.Artist{
			"a1": {ID: "a1", Name: "Artist A", Path: "/music/New Name"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c1": {ID: "c1", Name: "peer", Type: connType, URL: "http://peer", Enabled: true},
		}},
		Logger: silentLogger(),
	})
	return p, lister
}

// TestReconcileRelinks_UpgradesOnceThePeerSettles is the reconciler's whole
// reason to exist: the rename path left the link UNVERIFIED because the peer
// had not rescanned within its short poll budget. Minutes later, on this
// reconciler's own pass, the peer's listing now shows the real item at the
// artist's current path -- so the link upgrades, exactly like the rename
// path's own commitRelink would have done inside the budget if the scan had
// only been fast enough.
func TestReconcileRelinks_UpgradesOnceThePeerSettles(t *testing.T) {
	peer := &fakePeer{
		items: []connection.PeerArtist{
			// The stale item this reconciler must move OFF of.
			{ID: "jf-old", Name: "Artist A", Path: "/config/metadata/artists/Artist A"},
			// The real item the (by now complete) scan derived at the new path.
			{ID: "jf-new", Name: "Artist A", Path: "/music/New Name"},
		},
	}
	p, lister := relinkReconcileFixture(t, peer, "jf-old")

	p.ReconcileRelinks(context.Background())

	if len(lister.directSets) != 1 {
		t.Fatalf("SetPlatformID calls = %d (%+v), want 1", len(lister.directSets), lister.directSets)
	}
	got := lister.directSets[0]
	if got.platformArtistID != "jf-new" {
		t.Errorf("upgraded to %q, want %q", got.platformArtistID, "jf-new")
	}
	if got.artistID != "a1" || got.connectionID != "c1" {
		t.Errorf("wrote (%s,%s), want (a1,c1)", got.artistID, got.connectionID)
	}
	if len(lister.deletedConnIDs) != 0 {
		t.Errorf("the reconciler DROPPED a link (%v); it must only ever upgrade", lister.deletedConnIDs)
	}
}

// TestReconcileRelinks_StillUnresolvedLeavesLinkAlone is the never-drop guard,
// mutation-proofed against the exact failure mode #2426 reports: a peer whose
// scan STILL has not surfaced the move (a real possibility on a very large
// library even minutes later) must not cost the operator the link on THIS
// pass either. The listing is non-empty (the peer is healthy and reachable)
// but contains nothing that resolves -- the same "not yet, not proof of
// anything" shape the rename path's own tests exercise.
func TestReconcileRelinks_StillUnresolvedLeavesLinkAlone(t *testing.T) {
	peer := &fakePeer{
		items: []connection.PeerArtist{
			{ID: "jf-someone", Name: "Somebody Else", Path: "/music/Somebody Else"},
		},
	}
	p, lister := relinkReconcileFixture(t, peer, "jf-old")

	p.ReconcileRelinks(context.Background())

	if len(lister.directSets) != 0 {
		t.Errorf("wrote a link (%+v) with nothing resolvable on the peer", lister.directSets)
	}
	if len(lister.deletedConnIDs) != 0 {
		t.Errorf("the reconciler DROPPED a link (%v) on an unresolved pass; it must never drop",
			lister.deletedConnIDs)
	}
}

// TestReconcileRelinks_GhostNeverAuthorizesLinking mutates the peer listing to
// contain ONLY the abandoned metadata-only ghost (right name, a path outside
// every library root) with nothing at the artist's current path. The same
// invariant resolvePeerArtist enforces on the rename path (a name match must
// carry NO path) must hold here too: the ghost's presence must not upgrade
// the link to it, and the existing link must survive untouched.
func TestReconcileRelinks_GhostNeverAuthorizesLinking(t *testing.T) {
	peer := &fakePeer{
		items: []connection.PeerArtist{
			{ID: "jf-ghost", Name: "Artist A", Path: "/config/metadata/artists/Artist A"},
		},
	}
	p, lister := relinkReconcileFixture(t, peer, "jf-old")

	p.ReconcileRelinks(context.Background())

	if len(lister.directSets) != 0 {
		t.Errorf("linked to the ghost (%+v); a name match with a path must never resolve", lister.directSets)
	}
	if len(lister.deletedConnIDs) != 0 {
		t.Errorf("dropped the link (%v) off a ghost sighting alone", lister.deletedConnIDs)
	}
}

// TestReconcileRelinks_AlreadyCorrectIsANoOp verifies the reconciler does not
// churn a write when the peer already reports the link we hold as the correct
// target -- resolvedID == pid.PlatformArtistID must short-circuit before
// commitRelink, which itself would also no-op, but the point is asserting we
// do not depend on that: no call happens at all.
//
// Asserting on directSets ALONE cannot tell "the resolvedID==pid guard fired"
// from "commitRelink's own oldID==newID no-op fired instead", because both
// leave directSets empty. Removing the guard in reconcileMapping makes
// rr.upgraded++ fire for every already-correct mapping (the run summary would
// report the whole library as "upgraded" every pass) while directSets stays
// empty, so the test additionally asserts on the upgraded counter (exposed
// via relinkReconcileTestHook) to actually discriminate the two behaviors
// this test's own doc comment claims to.
func TestReconcileRelinks_AlreadyCorrectIsANoOp(t *testing.T) {
	peer := &fakePeer{
		items: []connection.PeerArtist{
			{ID: "jf-new", Name: "Artist A", Path: "/music/New Name"},
		},
	}
	p, lister := relinkReconcileFixture(t, peer, "jf-new")

	origHook := relinkReconcileTestHook
	var upgraded int
	relinkReconcileTestHook = func(rr *relinkReconcileRun) { upgraded = rr.upgraded }
	t.Cleanup(func() { relinkReconcileTestHook = origHook })

	p.ReconcileRelinks(context.Background())

	if len(lister.directSets) != 0 {
		t.Errorf("wrote %+v for a link that was already correct", lister.directSets)
	}
	if upgraded != 0 {
		t.Errorf("upgraded counter = %d, want 0: an already-correct mapping must not count as an upgrade", upgraded)
	}
}

// TestReconcileRelinks_SkipsLidarr proves the reconciler never even calls
// into a Lidarr connection's resolver: Lidarr honors path writes, so it can
// never land in the unverified state this reconciler repairs, and running the
// same peer-listing logic against it would be dead weight at best.
func TestReconcileRelinks_SkipsLidarr(t *testing.T) {
	peer := &fakePeer{
		items: []connection.PeerArtist{{ID: "lid-1", Name: "Artist A", Path: "/music/New Name"}},
	}
	orig := relinkResolverFactory
	relinkResolverFactory = func(_ *connection.Connection, _ *slog.Logger) (peerArtistResolver, bool) {
		return peer, true
	}
	t.Cleanup(func() { relinkResolverFactory = orig })

	lister := &fakePlatformLister{ids: []artist.PlatformID{
		{ArtistID: "a1", ConnectionID: "c1", PlatformArtistID: "lid-1"},
	}}
	p := New(Deps{
		ArtistService: lister,
		ArtistGetter: &fakeArtistGetter{artists: map[string]*artist.Artist{
			"a1": {ID: "a1", Name: "Artist A", Path: "/music/New Name"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c1": {ID: "c1", Name: "lidarr", Type: connection.TypeLidarr, URL: "http://lidarr", Enabled: true},
		}},
		Logger: silentLogger(),
	})

	p.ReconcileRelinks(context.Background())

	if peer.lists != 0 {
		t.Errorf("ListLibraryArtists called %d times against a Lidarr connection, want 0", peer.lists)
	}
}

// TestReconcileRelinks_SkipsEmby is the #2426 ghost decision, pinned.
//
// Emby is pathless BY DESIGN, so it never enters the unverified state this
// reconciler repairs, and the only key its listing carries -- the name -- cannot
// tell a real item from an abandoned metadata-only ghost. The reconciler
// therefore must not even LIST an Emby peer. Asserting on the list count rather
// than on the absence of a write is deliberate: a write-only assertion would
// still pass if the skip moved later in the function, leaving the sweep one
// resolver change away from acting on Emby again.
func TestReconcileRelinks_SkipsEmby(t *testing.T) {
	peer := &fakePeer{
		// A pathless item with the right name: on Emby this is what BOTH the real
		// artist and a metadata-only ghost look like. The point is that we cannot
		// tell, so we do not look.
		items: []connection.PeerArtist{{ID: "emby-ghost", Name: "Artist A", Path: ""}},
	}
	p, lister := relinkReconcileFixtureForType(t, peer, "emby-old", connection.TypeEmby)

	p.ReconcileRelinks(context.Background())

	if peer.lists != 0 {
		t.Errorf("ListLibraryArtists called %d times against an Emby connection, want 0: "+
			"a pathless peer offers no evidence this reconciler can act on", peer.lists)
	}
	if len(lister.directSets) != 0 {
		t.Errorf("repointed an Emby link (%+v) on a name match alone", lister.directSets)
	}
	if len(lister.deletedConnIDs) != 0 {
		t.Errorf("dropped an Emby link (%v); the reconciler must never drop", lister.deletedConnIDs)
	}
}

// TestReconcileRelinks_Emby_GhostWouldRepointWithoutTheSkip is the evidence for
// WHY the skip above exists, and it is the test that makes the skip load-bearing
// rather than decorative.
//
// It drives resolvePeerArtist directly with an Emby-shaped listing -- the real
// item absent from this pass, one pathless ghost carrying the artist's name --
// and asserts the resolver DOES return the ghost. That is not a bug in the
// resolver: on the rename path clause (b) is correct and necessary, because Emby
// exposes no other key and the caller has positive reason to doubt the link it
// holds. It is a bug only for an UNATTENDED sweep with no triggering event and
// no observer.
//
// So this test pins the hazard, not the desired behavior. If someone later
// removes the Emby skip, TestReconcileRelinks_SkipsEmby fails and this test
// explains what they just switched on. If clause (b) is ever tightened so a
// ghost can no longer resolve, THIS test fails -- and that failure is the signal
// that the skip may now be removable, which is exactly when that decision should
// be revisited.
func TestReconcileRelinks_Emby_GhostWouldRepointWithoutTheSkip(t *testing.T) {
	items := []connection.PeerArtist{
		// The abandoned metadata-only ghost. Pathless and correctly named --
		// indistinguishable from the real item on everything a listing carries.
		{ID: "emby-ghost", Name: "Artist A", Path: ""},
	}

	// currentID is the good link we hold; it is NOT in this listing, modeling the
	// pass where the real item did not come back.
	got, err := resolvePeerArtist(items, "/music/New Name", "Artist A", "emby-good", true)
	if err != nil {
		t.Fatalf("resolvePeerArtist returned an error: %v", err)
	}
	if got != "emby-ghost" {
		t.Fatalf("resolvePeerArtist = %q, want %q.\n"+
			"This test pins the HAZARD the Emby skip protects against. If clause (b) "+
			"was tightened so a pathless name match no longer resolves, that is good "+
			"news -- re-evaluate whether reconcileMapping still needs to skip Emby, "+
			"then update or remove this test.", got, "emby-ghost")
	}
}

// captureLogger returns a logger writing into buf, for the tests below that
// assert an unattended skip is AUDIBLE. Asserting "it did not crash" would pass
// against the silent-return version these tests exist to prevent.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestReconcileRelinks_ArtistLoadErrorIsLogged pins the #2426 silent-failure
// fix: an artist whose row cannot be loaded is skipped, and the skip SAYS SO.
//
// The earlier code returned bare on this branch, so a permanently unloadable
// artist looked exactly like one that needed no work -- the run summary showed
// a healthy pass while the operator's link stayed broken with no trace anywhere.
// The assertion is on the emitted log, because that is the only observable this
// branch has: nothing is written, nothing is returned.
func TestReconcileRelinks_ArtistLoadErrorIsLogged(t *testing.T) {
	var buf bytes.Buffer
	lister := &fakePlatformLister{ids: []artist.PlatformID{
		{ArtistID: "a1", ConnectionID: "c1", PlatformArtistID: "jf-old"},
	}}
	p := New(Deps{
		ArtistService: lister,
		ArtistGetter:  &fakeArtistGetter{err: errors.New("db boom")},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c1": {ID: "c1", Name: "peer", Type: connection.TypeJellyfin, URL: "http://peer", Enabled: true},
		}},
		Logger: captureLogger(&buf),
	})

	p.ReconcileRelinks(context.Background())

	out := buf.String()
	if !strings.Contains(out, "loading artist") {
		t.Errorf("artist load failure was skipped SILENTLY; logs:\n%s", out)
	}
	if !strings.Contains(out, "db boom") {
		t.Errorf("log omitted the underlying error, so the skip is unactionable; logs:\n%s", out)
	}
	if !strings.Contains(out, "a1") {
		t.Errorf("log omitted the artist id, so the skip is untraceable; logs:\n%s", out)
	}
	if len(lister.directSets) != 0 || len(lister.deletedConnIDs) != 0 {
		t.Errorf("mutated a link for an artist that could not even be loaded: sets=%+v deletes=%v",
			lister.directSets, lister.deletedConnIDs)
	}
}

// TestReconcileRelinks_PlatformIDLoadErrorIsLogged is the sibling branch: the
// artist loads, but its platform mappings do not. Same contract -- skip, but
// audibly.
func TestReconcileRelinks_PlatformIDLoadErrorIsLogged(t *testing.T) {
	var buf bytes.Buffer
	// ids is populated AND idsErr is set: the fake derives the artist list from
	// ids, so an empty ids would make the reconciler iterate nothing and the
	// assertion would pass vacuously without ever reaching the branch under test.
	lister := &fakePlatformLister{
		ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c1", PlatformArtistID: "jf-old"},
		},
		idsErr: errors.New("mappings boom"),
	}
	p := New(Deps{
		ArtistService: lister,
		ArtistGetter: &fakeArtistGetter{artists: map[string]*artist.Artist{
			"a1": {ID: "a1", Name: "Artist A", Path: "/music/New Name"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c1": {ID: "c1", Name: "peer", Type: connection.TypeJellyfin, URL: "http://peer", Enabled: true},
		}},
		Logger: captureLogger(&buf),
	})

	p.ReconcileRelinks(context.Background())

	out := buf.String()
	if !strings.Contains(out, "loading platform mappings") {
		t.Errorf("platform-mapping load failure was skipped SILENTLY; logs:\n%s", out)
	}
	if !strings.Contains(out, "mappings boom") {
		t.Errorf("log omitted the underlying error; logs:\n%s", out)
	}
}

// TestReconcileRelinks_MissingArtistRowIsLoggedDistinctly covers the nil-artist,
// nil-error case: the store answered SUCCESSFULLY that the artist does not
// exist. That points at a stale artist_platform_ids row rather than at an
// infrastructure failure, so it earns its own message -- folding it into the
// error branch would misdiagnose an orphaned row as a flaky database.
func TestReconcileRelinks_MissingArtistRowIsLoggedDistinctly(t *testing.T) {
	var buf bytes.Buffer
	lister := &fakePlatformLister{ids: []artist.PlatformID{
		{ArtistID: "a1", ConnectionID: "c1", PlatformArtistID: "jf-old"},
	}}
	p := New(Deps{
		ArtistService: lister,
		// nilArtistGetter returns (nil, nil): a successful "not found".
		ArtistGetter: nilArtistGetter{},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c1": {ID: "c1", Name: "peer", Type: connection.TypeJellyfin, URL: "http://peer", Enabled: true},
		}},
		Logger: captureLogger(&buf),
	})

	p.ReconcileRelinks(context.Background())

	out := buf.String()
	if !strings.Contains(out, "no artist row") {
		t.Errorf("a missing artist row was skipped silently or misreported as a load error; logs:\n%s", out)
	}
	if len(lister.directSets) != 0 {
		t.Errorf("wrote a link for an artist that does not exist: %+v", lister.directSets)
	}
}

// nilArtistGetter models a successful lookup that finds nothing, which is a
// different diagnosis from a failed lookup. fakeArtistGetter cannot express it:
// it returns an error for an unknown id.
type nilArtistGetter struct{}

func (nilArtistGetter) GetByID(_ context.Context, _ string, _ ...artist.HydrateOpts) (*artist.Artist, error) {
	return nil, nil
}

// TestReconcileRelinks_ListsEachConnectionOnceAcrossArtists is the
// large-library cost guard: on a library with many artists mapped to the
// SAME connection, the reconciler must issue ONE peer listing per connection
// per pass, not one per artist. Mutating the cache key (or removing it) makes
// this fail by driving the peer's list call count up to the artist count.
func TestReconcileRelinks_ListsEachConnectionOnceAcrossArtists(t *testing.T) {
	peer := &fakePeer{
		items: []connection.PeerArtist{
			{ID: "jf-a", Name: "Artist A", Path: "/music/A"},
			{ID: "jf-b", Name: "Artist B", Path: "/music/B"},
		},
	}
	orig := relinkResolverFactory
	relinkResolverFactory = func(_ *connection.Connection, _ *slog.Logger) (peerArtistResolver, bool) {
		return peer, true
	}
	t.Cleanup(func() { relinkResolverFactory = orig })

	lister := &multiArtistPlatformLister{fakePlatformLister: &fakePlatformLister{
		ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c1", PlatformArtistID: "jf-old-a"},
			{ArtistID: "a2", ConnectionID: "c1", PlatformArtistID: "jf-old-b"},
		},
	}}
	p := New(Deps{
		ArtistService: lister,
		ArtistGetter: &fakeArtistGetter{artists: map[string]*artist.Artist{
			"a1": {ID: "a1", Name: "Artist A", Path: "/music/A"},
			"a2": {ID: "a2", Name: "Artist B", Path: "/music/B"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c1": {ID: "c1", Name: "peer", Type: connection.TypeJellyfin, URL: "http://peer", Enabled: true},
		}},
		Logger: silentLogger(),
	})

	p.ReconcileRelinks(context.Background())

	if peer.lists != 1 {
		t.Errorf("ListLibraryArtists called %d times across 2 artists on 1 connection, want 1", peer.lists)
	}
	if len(lister.directSets) != 2 {
		t.Fatalf("SetPlatformID calls = %d, want 2 (both artists upgraded from the single cached listing)",
			len(lister.directSets))
	}
}

// TestReconcileRelinks_ListErrorNeverDrops mutation-proofs the "cannot reach
// the peer" branch: a transport error must leave every link on that
// connection untouched, not be treated as "the peer answered and knows
// nothing" (which would be a fabricated absence, not an observed one).
func TestReconcileRelinks_ListErrorNeverDrops(t *testing.T) {
	peer := &fakePeer{listErr: errors.New("connection refused")}
	p, lister := relinkReconcileFixture(t, peer, "jf-old")

	p.ReconcileRelinks(context.Background())

	if len(lister.directSets) != 0 || len(lister.deletedConnIDs) != 0 {
		t.Errorf("a list error produced writes: sets=%+v deletes=%v", lister.directSets, lister.deletedConnIDs)
	}
}

// TestReconcileRelinks_NilReceiver verifies the nil guard, matching the
// pattern of TestReconcileArtworkToPlatforms_NilReceiver.
func TestReconcileRelinks_NilReceiver(t *testing.T) {
	var p *Publisher
	p.ReconcileRelinks(context.Background()) // must not panic
}

// TestReconcileRelinks_NoArtistGetterIsANoOp mirrors
// TestReconcileArtworkToPlatforms_NoArtistGetter: without an artist getter the
// reconciler cannot load the current host path at all, so it must refuse to
// run rather than resolve against a stale or empty path.
func TestReconcileRelinks_NoArtistGetterIsANoOp(t *testing.T) {
	lister := &fakePlatformLister{ids: []artist.PlatformID{
		{ArtistID: "a1", ConnectionID: "c1", PlatformArtistID: "jf-old"},
	}}
	p := New(Deps{
		ArtistService: lister,
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c1": {ID: "c1", Name: "peer", Type: connection.TypeJellyfin, URL: "http://peer", Enabled: true},
		}},
		Logger: silentLogger(),
	})

	p.ReconcileRelinks(context.Background()) // must not panic

	if len(lister.directSets) != 0 {
		t.Errorf("wrote %+v with no artist getter wired", lister.directSets)
	}
}

// --- StartRelinkReconciler tests ---

// TestStartRelinkReconciler_StopsOnContextCancel mirrors
// TestStartArtworkReconciler_StopsOnContextCancel.
func TestStartRelinkReconciler_StopsOnContextCancel(t *testing.T) {
	p := New(Deps{
		ArtistService:     &fakePlatformLister{},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{}},
		Logger:            silentLogger(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.StartRelinkReconciler(ctx, 50*time.Millisecond, 5*time.Millisecond)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartRelinkReconciler did not stop within 2s after context cancel")
	}
}

// TestStartRelinkReconciler_CancelBeforeStartup mirrors
// TestStartArtworkReconciler_CancelBeforeStartup.
func TestStartRelinkReconciler_CancelBeforeStartup(t *testing.T) {
	p := New(Deps{
		ArtistService:     &fakePlatformLister{},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{}},
		Logger:            silentLogger(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		p.StartRelinkReconciler(ctx, 100*time.Millisecond, 500*time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartRelinkReconciler did not stop within 2s after pre-cancel")
	}
}

// TestStartRelinkReconciler_NonPositiveInterval mirrors
// TestStartArtworkReconciler_NonPositiveInterval.
func TestStartRelinkReconciler_NonPositiveInterval(t *testing.T) {
	p := New(Deps{
		ArtistService:     &fakePlatformLister{},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{}},
		Logger:            silentLogger(),
	})
	p.StartRelinkReconciler(context.Background(), 0, time.Millisecond)
	p.StartRelinkReconciler(context.Background(), -time.Second, time.Millisecond)
}

// TestRunReconcileRelinksWithRecover_PanicSafety mirrors
// TestRunReconcileWithRecover_PanicSafety: a panic inside ReconcileRelinks
// (driven here by an artist getter that panics) must not escape the recover
// wrapper.
func TestRunReconcileRelinksWithRecover_PanicSafety(t *testing.T) {
	lister := &fakePlatformLister{ids: []artist.PlatformID{
		{ArtistID: "a1", ConnectionID: "c1", PlatformArtistID: "jf-old"},
	}}
	p := New(Deps{
		ArtistService:     lister,
		ArtistGetter:      panicArtistGetter{},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{}},
		Logger:            silentLogger(),
	})
	p.runReconcileRelinksWithRecover(context.Background()) // must not panic
}

// panicArtistGetter always panics, driving the panic-recovery path in
// runReconcileRelinksWithRecover.
type panicArtistGetter struct{}

func (panicArtistGetter) GetByID(context.Context, string, ...artist.HydrateOpts) (*artist.Artist, error) {
	panic("boom")
}

// --- I1 (FIX 1): the peer-type gate is a positive ALLOW-LIST ---

// TestReconcileRelinks_UnenrolledPeerTypeNeverListedOrWritten proves
// peerIsFolderBacked's allow-list is what stops an unrecognized connection
// type from joining the unattended sweep, not relinkResolverFactory's
// default (nil, false). The injected factory deliberately answers (peer,
// true) for the unenrolled type -- exactly the shape of the #2426 review's
// reproduction, which added a "plex" case to relinkResolverFactory and found
// the entire suite still green. If reconcileMapping's `!peerIsFolderBacked`
// gate is removed, this factory override is what lets the sweep proceed, and
// this test fails.
func TestReconcileRelinks_UnenrolledPeerTypeNeverListedOrWritten(t *testing.T) {
	peer := &fakePeer{items: []connection.PeerArtist{{ID: "px-new", Name: "Artist A", Path: "/music/New Name"}}}
	orig := relinkResolverFactory
	relinkResolverFactory = func(_ *connection.Connection, _ *slog.Logger) (peerArtistResolver, bool) {
		return peer, true
	}
	t.Cleanup(func() { relinkResolverFactory = orig })

	lister := &fakePlatformLister{ids: []artist.PlatformID{
		{ArtistID: "a1", ConnectionID: "c1", PlatformArtistID: "px-old"},
	}}
	p := New(Deps{
		ArtistService: lister,
		ArtistGetter: &fakeArtistGetter{artists: map[string]*artist.Artist{
			"a1": {ID: "a1", Name: "Artist A", Path: "/music/New Name"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c1": {ID: "c1", Name: "plex", Type: "plex", URL: "http://plex", Enabled: true},
		}},
		Logger: silentLogger(),
	})

	p.ReconcileRelinks(context.Background())

	if peer.lists != 0 {
		t.Errorf("ListLibraryArtists called %d times against an unenrolled peer type, want 0", peer.lists)
	}
	if len(lister.directSets) != 0 {
		t.Errorf("wrote a link for an unenrolled peer type: %+v", lister.directSets)
	}
}

// --- I2 (FIX 2): re-read guard against a concurrent foreground write ---

// concurrentWriteConnectionGetter wraps a fakeConnectionGetter and, on its
// FIRST GetByID call, mutates the platform lister's stored id for connID --
// modeling a foreground rename landing at exactly the point in
// reconcileMapping's call sequence (connectionService.GetByID, which runs
// BEFORE the re-read guard and the peer listing round trip) the review
// reproduced the lost update at.
type concurrentWriteConnectionGetter struct {
	*fakeConnectionGetter
	lister *fakePlatformLister
	connID string
	newID  string
	fired  bool
}

func (c *concurrentWriteConnectionGetter) GetByID(ctx context.Context, id string) (*connection.Connection, error) {
	if !c.fired {
		c.fired = true
		for i := range c.lister.ids {
			if c.lister.ids[i].ConnectionID == c.connID {
				c.lister.ids[i].PlatformArtistID = c.newID
			}
		}
	}
	return c.fakeConnectionGetter.GetByID(ctx, id)
}

// TestReconcileRelinks_ConcurrentForegroundWriteWins reproduces the lost
// update: a foreground rename writes jf-foreground into artist_platform_ids
// while this reconciler pass is mid-flight (snapshotted jf-old, about to
// resolve and write jf-reconciler-match). The re-read guard added in
// reconcileMapping must see the changed id immediately before the write and
// abort rather than clobber it -- the foreground write is the more
// authoritative one.
func TestReconcileRelinks_ConcurrentForegroundWriteWins(t *testing.T) {
	peer := &fakePeer{
		items: []connection.PeerArtist{
			{ID: "jf-reconciler-match", Name: "Artist A", Path: "/music/New Name"},
		},
	}
	orig := relinkResolverFactory
	relinkResolverFactory = func(_ *connection.Connection, _ *slog.Logger) (peerArtistResolver, bool) {
		return peer, true
	}
	t.Cleanup(func() { relinkResolverFactory = orig })

	lister := &fakePlatformLister{ids: []artist.PlatformID{
		{ArtistID: "a1", ConnectionID: "c1", PlatformArtistID: "jf-old"},
	}}
	connGetter := &concurrentWriteConnectionGetter{
		fakeConnectionGetter: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c1": {ID: "c1", Name: "peer", Type: connection.TypeJellyfin, URL: "http://peer", Enabled: true},
		}},
		lister: lister,
		connID: "c1",
		newID:  "jf-foreground",
	}
	p := New(Deps{
		ArtistService: lister,
		ArtistGetter: &fakeArtistGetter{artists: map[string]*artist.Artist{
			"a1": {ID: "a1", Name: "Artist A", Path: "/music/New Name"},
		}},
		ConnectionService: connGetter,
		Logger:            silentLogger(),
	})

	p.ReconcileRelinks(context.Background())

	if len(lister.directSets) != 0 {
		t.Fatalf("reconciler overwrote a concurrent foreground write: %+v", lister.directSets)
	}
	if len(lister.ids) != 1 || lister.ids[0].PlatformArtistID != "jf-foreground" {
		t.Errorf("final stored id = %+v, want the foreground write (jf-foreground) preserved", lister.ids)
	}
}

// --- FIX 3a: per-run cache is keyed by connection ID, not connection type ---

// TestReconcileRelinks_TwoConnectionsCachedSeparately guards against the
// cache-key mutation the reviewer found silently green: keying peerArtists'
// per-run cache by conn.Type instead of conn.ID collapses two independent
// Jellyfin connections onto one cache slot, so the second connection's
// artists resolve against the FIRST connection's listing -- a silent,
// permanent, cross-server mislink. Two connections, two distinct peers, two
// distinct item ids: each connection's write must carry its OWN peer's id,
// and each peer must be listed exactly once.
func TestReconcileRelinks_TwoConnectionsCachedSeparately(t *testing.T) {
	peer1 := &fakePeer{items: []connection.PeerArtist{{ID: "jf1-new", Name: "Artist A", Path: "/music/New Name"}}}
	peer2 := &fakePeer{items: []connection.PeerArtist{{ID: "jf2-new", Name: "Artist A", Path: "/music/New Name"}}}
	orig := relinkResolverFactory
	relinkResolverFactory = func(conn *connection.Connection, _ *slog.Logger) (peerArtistResolver, bool) {
		switch conn.ID {
		case "c1":
			return peer1, true
		case "c2":
			return peer2, true
		}
		return nil, false
	}
	t.Cleanup(func() { relinkResolverFactory = orig })

	lister := &fakePlatformLister{ids: []artist.PlatformID{
		{ArtistID: "a1", ConnectionID: "c1", PlatformArtistID: "jf1-old"},
		{ArtistID: "a1", ConnectionID: "c2", PlatformArtistID: "jf2-old"},
	}}
	p := New(Deps{
		ArtistService: lister,
		ArtistGetter: &fakeArtistGetter{artists: map[string]*artist.Artist{
			"a1": {ID: "a1", Name: "Artist A", Path: "/music/New Name"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c1": {ID: "c1", Name: "peer1", Type: connection.TypeJellyfin, URL: "http://peer1", Enabled: true},
			"c2": {ID: "c2", Name: "peer2", Type: connection.TypeJellyfin, URL: "http://peer2", Enabled: true},
		}},
		Logger: silentLogger(),
	})

	p.ReconcileRelinks(context.Background())

	if peer1.lists != 1 {
		t.Errorf("peer1 (c1) listed %d times, want 1", peer1.lists)
	}
	if peer2.lists != 1 {
		t.Errorf("peer2 (c2) listed %d times, want 1", peer2.lists)
	}
	if len(lister.directSets) != 2 {
		t.Fatalf("SetPlatformID calls = %d, want 2: %+v", len(lister.directSets), lister.directSets)
	}
	got := map[string]string{}
	for _, s := range lister.directSets {
		got[s.connectionID] = s.platformArtistID
	}
	if got["c1"] != "jf1-new" {
		t.Errorf("c1 wrote %q, want jf1-new (cross-connection contamination)", got["c1"])
	}
	if got["c2"] != "jf2-new" {
		t.Errorf("c2 wrote %q, want jf2-new (cross-connection contamination)", got["c2"])
	}
}

// --- FIX 3b: cache distinguishes "cached nil" from "not yet fetched" ---

// TestReconcileRelinks_ListErrorCachedNotRetried guards the two-value cache
// read (`items, isCached := rr.cache[conn.ID]`) against the naive
// `if items := rr.cache[conn.ID]; items != nil` mutation, which cannot tell
// "the peer failed, cached as a deliberate nil" from "not fetched yet" and so
// re-lists a down peer once per artist -- a thundering-herd retry against an
// already-failing peer. Two artists share one connection whose listing
// always errors; ListLibraryArtists must be called exactly once.
func TestReconcileRelinks_ListErrorCachedNotRetried(t *testing.T) {
	peer := &fakePeer{listErr: errors.New("peer down")}
	orig := relinkResolverFactory
	relinkResolverFactory = func(_ *connection.Connection, _ *slog.Logger) (peerArtistResolver, bool) {
		return peer, true
	}
	t.Cleanup(func() { relinkResolverFactory = orig })

	lister := &multiArtistPlatformLister{fakePlatformLister: &fakePlatformLister{
		ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c1", PlatformArtistID: "jf-old-a"},
			{ArtistID: "a2", ConnectionID: "c1", PlatformArtistID: "jf-old-b"},
		},
	}}
	p := New(Deps{
		ArtistService: lister,
		ArtistGetter: &fakeArtistGetter{artists: map[string]*artist.Artist{
			"a1": {ID: "a1", Name: "Artist A", Path: "/music/A"},
			"a2": {ID: "a2", Name: "Artist B", Path: "/music/B"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c1": {ID: "c1", Name: "peer", Type: connection.TypeJellyfin, URL: "http://peer", Enabled: true},
		}},
		Logger: silentLogger(),
	})

	p.ReconcileRelinks(context.Background())

	if peer.lists != 1 {
		t.Errorf("ListLibraryArtists called %d times across 2 artists after a listing error, want 1 "+
			"(thundering-herd retry against a down peer)", peer.lists)
	}
}

// --- FIX 3c: ctx cancellation stops the artist loop early ---

// cancelingArtistGetter cancels ctx on its first GetByID call, modeling a
// cancellation landing mid-run, then delegates to the wrapped fake.
type cancelingArtistGetter struct {
	*fakeArtistGetter
	cancel context.CancelFunc
	calls  int
}

func (c *cancelingArtistGetter) GetByID(ctx context.Context, id string, opts ...artist.HydrateOpts) (*artist.Artist, error) {
	c.calls++
	if c.calls == 1 {
		c.cancel()
	}
	return c.fakeArtistGetter.GetByID(ctx, id, opts...)
}

// TestReconcileRelinks_CtxCancellationStopsLoopEarly guards the
// `if ctx.Err() != nil { break }` loop guard in ReconcileRelinks: deleting it
// left the suite green because nothing else in the fake peer/lister chain
// checks ctx. Three artists are supplied; the context is canceled during the
// FIRST artist's load, so the guard must stop the loop before the second and
// third are ever processed.
func TestReconcileRelinks_CtxCancellationStopsLoopEarly(t *testing.T) {
	lister := &fakePlatformLister{ids: []artist.PlatformID{
		{ArtistID: "a1", ConnectionID: "c1", PlatformArtistID: "jf-old-1"},
		{ArtistID: "a2", ConnectionID: "c1", PlatformArtistID: "jf-old-2"},
		{ArtistID: "a3", ConnectionID: "c1", PlatformArtistID: "jf-old-3"},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ag := &cancelingArtistGetter{
		fakeArtistGetter: &fakeArtistGetter{artists: map[string]*artist.Artist{
			"a1": {ID: "a1", Name: "Artist A", Path: "/music/A"},
			"a2": {ID: "a2", Name: "Artist B", Path: "/music/B"},
			"a3": {ID: "a3", Name: "Artist C", Path: "/music/C"},
		}},
		cancel: cancel,
	}
	p := New(Deps{
		ArtistService: lister,
		ArtistGetter:  ag,
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c1": {ID: "c1", Name: "peer", Type: connection.TypeJellyfin, URL: "http://peer", Enabled: true},
		}},
		Logger: silentLogger(),
	})

	p.ReconcileRelinks(ctx)

	if ag.calls != 1 {
		t.Errorf("GetByID called %d times; ctx cancellation after the first artist should have stopped "+
			"the loop before the second and third were processed (want exactly 1)", ag.calls)
	}
}

// --- FIX 3e: a disabled connection is never listed or written ---

// TestReconcileRelinks_DisabledConnectionNeverListedOrWritten guards the
// `!conn.Enabled` check in reconcileMapping: deleting it left the suite
// green because none of the other fixtures exercise a disabled connection.
func TestReconcileRelinks_DisabledConnectionNeverListedOrWritten(t *testing.T) {
	peer := &fakePeer{items: []connection.PeerArtist{{ID: "jf-new", Name: "Artist A", Path: "/music/New Name"}}}
	orig := relinkResolverFactory
	relinkResolverFactory = func(_ *connection.Connection, _ *slog.Logger) (peerArtistResolver, bool) {
		return peer, true
	}
	t.Cleanup(func() { relinkResolverFactory = orig })

	lister := &fakePlatformLister{ids: []artist.PlatformID{
		{ArtistID: "a1", ConnectionID: "c1", PlatformArtistID: "jf-old"},
	}}
	p := New(Deps{
		ArtistService: lister,
		ArtistGetter: &fakeArtistGetter{artists: map[string]*artist.Artist{
			"a1": {ID: "a1", Name: "Artist A", Path: "/music/New Name"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c1": {ID: "c1", Name: "peer", Type: connection.TypeJellyfin, URL: "http://peer", Enabled: false},
		}},
		Logger: silentLogger(),
	})

	origHook := relinkReconcileTestHook
	var disabled, noConn int
	relinkReconcileTestHook = func(rr *relinkReconcileRun) {
		disabled, noConn = rr.skippedDisabled, rr.skippedNoConn
	}
	t.Cleanup(func() { relinkReconcileTestHook = origHook })

	p.ReconcileRelinks(context.Background())

	if peer.lists != 0 {
		t.Errorf("ListLibraryArtists called %d times against a disabled connection, want 0", peer.lists)
	}
	if len(lister.directSets) != 0 {
		t.Errorf("wrote a link for a disabled connection: %+v", lister.directSets)
	}
	// THE COUNTER IT LANDS IN IS THE POINT, not just that it was skipped. A
	// disabled connection is the operator getting what they asked for; "no
	// connection" means the row is missing or unreadable, which is a fault to
	// investigate. Folding the two together (as this did before review) made a
	// deliberate configuration read as links failing to resolve, so an operator
	// scanning the run summary would chase a problem that does not exist.
	if disabled != 1 {
		t.Errorf("skipped_disabled = %d, want 1: a disabled connection must have its own counter", disabled)
	}
	if noConn != 0 {
		t.Errorf("skipped_no_connection = %d, want 0: a disabled connection is not a missing one", noConn)
	}
}

// TestReconcileRelinks_ConnectionLoadErrorIsLogged covers the DB-error branch
// of the connection lookup. It is the sibling of the artist-load and
// platform-id-load tests above, and it exists for the same reason: an
// unattended job's skips have to be readable after the fact. Before the #2426
// review this branch discarded connErr without ever formatting it, so a
// database failure and a deliberately disabled connection were
// indistinguishable in the logs -- both simply vanished into one counter.
func TestReconcileRelinks_ConnectionLoadErrorIsLogged(t *testing.T) {
	var buf bytes.Buffer
	lister := &fakePlatformLister{ids: []artist.PlatformID{
		{ArtistID: "a1", ConnectionID: "c1", PlatformArtistID: "jf-old"},
	}}
	p := New(Deps{
		ArtistService: lister,
		ArtistGetter: &fakeArtistGetter{artists: map[string]*artist.Artist{
			"a1": {ID: "a1", Name: "Artist A", Path: "/music/New Name"},
		}},
		// An EMPTY conns map: fakeConnectionGetter.GetByID returns an error for
		// an id it does not hold, which is the connErr branch (not the nil-conn
		// branch -- that one needs a getter that returns (nil, nil), below).
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{}},
		Logger:            captureLogger(&buf),
	})

	p.ReconcileRelinks(context.Background())

	out := buf.String()
	if !strings.Contains(out, "loading connection") {
		t.Errorf("connection load failure was skipped SILENTLY; logs:\n%s", out)
	}
	if !strings.Contains(out, "c1") {
		t.Errorf("log omitted the connection id, so the skip is untraceable; logs:\n%s", out)
	}
	if len(lister.directSets) != 0 || len(lister.deletedConnIDs) != 0 {
		t.Errorf("mutated a link whose connection could not be loaded: sets=%+v deletes=%v",
			lister.directSets, lister.deletedConnIDs)
	}
}

// TestReconcileRelinks_MissingConnectionRowIsLoggedDistinctly covers the
// (nil, nil) branch: the store answers SUCCESSFULLY that the connection does
// not exist. That is a genuinely different diagnosis from a DB error -- it
// points at an orphaned artist_platform_ids row (a connection deleted out from
// under a mapping), which an operator can actually act on -- so it earns its
// own log line.
//
// Uses nilConnectionGetter from reconcile_test.go: fakeConnectionGetter cannot
// express this case, since a miss there returns an error rather than (nil, nil).
func TestReconcileRelinks_MissingConnectionRowIsLoggedDistinctly(t *testing.T) {
	var buf bytes.Buffer
	lister := &fakePlatformLister{ids: []artist.PlatformID{
		{ArtistID: "a1", ConnectionID: "c1", PlatformArtistID: "jf-old"},
	}}
	p := New(Deps{
		ArtistService: lister,
		ArtistGetter: &fakeArtistGetter{artists: map[string]*artist.Artist{
			"a1": {ID: "a1", Name: "Artist A", Path: "/music/New Name"},
		}},
		ConnectionService: nilConnectionGetter{},
		Logger:            captureLogger(&buf),
	})

	p.ReconcileRelinks(context.Background())

	out := buf.String()
	if !strings.Contains(out, "no longer exists") {
		t.Errorf("an orphaned platform mapping was skipped without flagging the dangling reference; logs:\n%s", out)
	}
	// The two branches must stay DISTINGUISHABLE in the logs. If this ever
	// reports the DB-error wording, the split added in the #2426 review has
	// collapsed back into one message and the diagnosis is lost again.
	if strings.Contains(out, "loading connection") {
		t.Errorf("missing-connection case logged as a load ERROR, collapsing two distinct diagnoses; logs:\n%s", out)
	}
	if len(lister.directSets) != 0 || len(lister.deletedConnIDs) != 0 {
		t.Errorf("mutated a link whose connection row does not exist: sets=%+v deletes=%v",
			lister.directSets, lister.deletedConnIDs)
	}
}

// reReadFailingLister fails GetPlatformIDs only on the SECOND call. The first
// call is reconcileArtist's snapshot (which must succeed, or the reconciler
// never reaches a write at all); the second is the pre-write re-read added by
// the #2426 lost-update fix. A fake with a plain always-fail flag cannot
// express this -- it would abort the pass before the branch under test is
// reachable, and the test would pass VACUOUSLY.
//
// It wraps fakePlatformLister rather than adding a call counter to it, because
// that fake is shared across this package's tests and a counter would change
// behavior for every one of them.
type reReadFailingLister struct {
	*fakePlatformLister
	calls int
}

func (r *reReadFailingLister) GetPlatformIDs(ctx context.Context, artistID string) ([]artist.PlatformID, error) {
	r.calls++
	if r.calls >= 2 {
		return nil, errors.New("db vanished mid-pass")
	}
	return r.fakePlatformLister.GetPlatformIDs(ctx, artistID)
}

// TestReconcileRelinks_ReReadErrorNeverWrites pins the FAIL-SAFE direction of
// the lost-update guard. The guard exists to yield to a concurrent foreground
// write; if the re-read that powers it fails, the reconciler has no idea
// whether such a write landed, and the safe answer is to write NOTHING and try
// again next pass.
//
// WHAT THIS TEST ACTUALLY PINS, precisely, because the difference matters:
// no-write here is DOUBLE-GUARDED, and the early return is the weaker of the
// two. A failed re-read leaves `current` nil, so the lookup loop below never
// runs, stillMatches stays false, and the !stillMatches guard would block the
// write on its own -- deleting the early return alone does not let a write
// through. Measured: that mutation leaves this test GREEN.
//
// The load-bearing assertion is therefore the LOG one, and it is not
// bookkeeping. With the branch removed, a database failure is reported as
// "concurrent write detected ... yielding to it" -- a confidently WRONG
// diagnosis that would send an operator hunting a race that never happened
// while the real fault (their store erroring mid-pass) goes unmentioned.
// Verified by mutation: deleting the branch fails this test on that line.
// The no-write assertions stay as a belt-and-braces check on the pair.
func TestReconcileRelinks_ReReadErrorNeverWrites(t *testing.T) {
	var buf bytes.Buffer
	peer := &fakePeer{items: []connection.PeerArtist{
		{ID: "jf-new", Name: "Artist A", Path: "/music/New Name"},
	}}
	orig := relinkResolverFactory
	relinkResolverFactory = func(_ *connection.Connection, _ *slog.Logger) (peerArtistResolver, bool) {
		return peer, true
	}
	t.Cleanup(func() { relinkResolverFactory = orig })

	inner := &fakePlatformLister{ids: []artist.PlatformID{
		{ArtistID: "a1", ConnectionID: "c1", PlatformArtistID: "jf-old"},
	}}
	lister := &reReadFailingLister{fakePlatformLister: inner}
	p := New(Deps{
		ArtistService: lister,
		ArtistGetter: &fakeArtistGetter{artists: map[string]*artist.Artist{
			"a1": {ID: "a1", Name: "Artist A", Path: "/music/New Name"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c1": {ID: "c1", Name: "peer", Type: connection.TypeJellyfin, URL: "http://peer", Enabled: true},
		}},
		Logger: captureLogger(&buf),
	})

	p.ReconcileRelinks(context.Background())

	// PRECONDITION: the run must actually have reached the re-read, or this
	// test asserts nothing. Without this the test would pass just as happily
	// against a reconciler that bailed out three branches earlier.
	if lister.calls < 2 {
		t.Fatalf("never reached the pre-write re-read (GetPlatformIDs calls=%d, want >=2); "+
			"the assertions below would be vacuous", lister.calls)
	}
	if len(inner.directSets) != 0 || len(inner.deletedConnIDs) != 0 {
		t.Errorf("wrote a link despite being unable to confirm no concurrent write landed: sets=%+v deletes=%v",
			inner.directSets, inner.deletedConnIDs)
	}
	if !strings.Contains(buf.String(), "re-reading platform mappings") {
		t.Errorf("skipped the write SILENTLY on a failed re-read; logs:\n%s", buf.String())
	}
}
