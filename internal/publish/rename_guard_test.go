package publish

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
)

// guardFixture builds a publisher with one connection of the given type and a
// single platform_ids row, returning the fake updater so a test can assert
// whether the push was ever attempted.
func guardFixture(t *testing.T, connType string, mappings []connection.PathMapping) (*Publisher, *fakePathUpdater) {
	t.Helper()
	fake := &fakePathUpdater{}
	orig := renamePathUpdaterFactory
	renamePathUpdaterFactory = func(_ *connection.Connection, _ *slog.Logger) (pathUpdater, bool) {
		return fake, true
	}
	t.Cleanup(func() { renamePathUpdaterFactory = orig })
	// These cases exercise the pre-flight ROOT GUARD, not the post-update
	// read-back. Give them a peer that honors the write so a push that clears the
	// guard still lands on "ok" and the guard's own refusal branches stay the
	// thing under test. Without this the verifier would fall through to a real
	// HTTP client aimed at "http://peer" -- an env-dependent test.
	withFakePeer(t, &fakePeer{honorsPath: true, updater: fake,
		roots: []string{"/music", "/data", "/data/media", "/new", "/old"}})

	conn := &connection.Connection{
		ID: "c1", Name: "peer", Type: connType, URL: "http://peer", Enabled: true,
		PathMappings: mappings,
	}
	switch connType {
	case connection.TypeEmby:
		conn.Emby = &connection.EmbyConfig{PlatformUserID: "u1"}
	case connection.TypeJellyfin:
		conn.Jellyfin = &connection.JellyfinConfig{PlatformUserID: "u1"}
	case connection.TypeLidarr:
		conn.Lidarr = &connection.LidarrConfig{}
	}

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c1", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{"c1": conn}},
		Logger:            silentLogger(),
	})
	return p, fake
}

// TestSyncRename_RefusesHostPathOutsideJellyfinRoots is THE #2380 acceptance
// scenario, and the single most important test in this change.
//
// Reproduces the reported bug exactly: Stillwater sees the library at
// /host/music while the Jellyfin CONTAINER sees it at /music. With
// no path mapping configured, the old code pushed the raw host path; Jellyfin
// accepted the call, silently kept its old path, and Stillwater reported a green
// "ok". Its NFO saver then re-created the merged-away directory, which the next
// scan re-imported as a duplicate artist.
//
// The push must now be REFUSED before it leaves the process:
//   - result is "failed", never "ok"
//   - UpdateArtistPath is NEVER called
//   - the error names the remedy (configure a path mapping)
func TestSyncRename_RefusesHostPathOutsideJellyfinRoots(t *testing.T) {
	p, fake := guardFixture(t, connection.TypeJellyfin, nil) // NO mappings: the bug's precondition
	withFakeRootLister(t, fakeRootLister{roots: []string{"/music"}})

	results, err := p.SyncRename(context.Background(), "a1", "/host/music/Old", "/host/music/New")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v, want exactly one entry", results)
	}
	if results[0].Result != artist.PlatformRemapFailed {
		t.Fatalf("result = %q, want %q: a host path pushed into the peer's container namespace "+
			"MUST NOT report ok (#2380)", results[0].Result, artist.PlatformRemapFailed)
	}
	if fake.called != 0 {
		t.Errorf("UpdateArtistPath called %d times, want 0: the out-of-root path must be refused "+
			"BEFORE it reaches the peer, not pushed and then reported", fake.called)
	}
	if !strings.Contains(results[0].Error, "path mapping") {
		t.Errorf("error must name the remedy (configure a path mapping); got: %s", results[0].Error)
	}
}

// TestSyncRename_MappedPathInsideRootsIsPushed is the positive half: with the
// mapping the operator should have had all along, the SAME host path translates
// into the peer's namespace, lands inside its roots, and IS pushed.
func TestSyncRename_MappedPathInsideRootsIsPushed(t *testing.T) {
	p, fake := guardFixture(t, connection.TypeJellyfin, []connection.PathMapping{
		{HostPrefix: "/host/music", PlatformPrefix: "/music"},
	})
	withFakeRootLister(t, fakeRootLister{roots: []string{"/music"}})

	results, err := p.SyncRename(context.Background(), "a1", "/host/music/Old", "/host/music/New")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if len(results) != 1 || results[0].Result != artist.PlatformRemapOK {
		t.Fatalf("results = %+v, want one ok entry", results)
	}
	if fake.called != 1 {
		t.Fatalf("UpdateArtistPath called %d times, want 1", fake.called)
	}
	if fake.gotPath != "/music/New" {
		t.Errorf("pushed path = %q, want %q (the mapping must translate host -> container)", fake.gotPath, "/music/New")
	}
}

// TestSyncRename_GuardFailsClosedWhenRootsUnreadable pins the fail-closed
// contract. A peer whose root list cannot be read is UNVERIFIABLE, and
// "unverifiable" must never degrade into "push it anyway" -- that degradation is
// the whole shape of the bug (#2380): a wrong path that peers accept while
// reporting success. Refusing loudly is correct; the rename itself already
// committed on disk and is not rolled back.
func TestSyncRename_GuardFailsClosedWhenRootsUnreadable(t *testing.T) {
	p, fake := guardFixture(t, connection.TypeLidarr, nil)
	withFakeRootLister(t, fakeRootLister{err: errors.New("peer unreachable")})

	results, err := p.SyncRename(context.Background(), "a1", "/music/Old", "/music/New")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if len(results) != 1 || results[0].Result != artist.PlatformRemapFailed {
		t.Fatalf("results = %+v, want one failed entry (unverifiable roots must fail closed)", results)
	}
	if fake.called != 0 {
		t.Errorf("UpdateArtistPath called %d times, want 0: an unverifiable path must not be pushed", fake.called)
	}
}

// TestSyncRename_GuardFailsClosedOnZeroRoots: a peer reporting NO roots gives the
// guard nothing to verify against. Same reasoning as an unreadable list -- an
// empty root set is not a license to push.
func TestSyncRename_GuardFailsClosedOnZeroRoots(t *testing.T) {
	p, fake := guardFixture(t, connection.TypeEmby, nil)
	withFakeRootLister(t, fakeRootLister{roots: nil})

	results, err := p.SyncRename(context.Background(), "a1", "/music/Old", "/music/New")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if len(results) != 1 || results[0].Result != artist.PlatformRemapFailed {
		t.Fatalf("results = %+v, want one failed entry (zero roots must fail closed)", results)
	}
	if fake.called != 0 {
		t.Errorf("UpdateArtistPath called %d times, want 0", fake.called)
	}
}

// TestSyncRename_GuardConsultsThePeer proves the guard actually performs the
// root lookup rather than being short-circuited by some earlier branch -- a
// guard that never runs is exactly the failure mode #2380 replaced (Lidarr's
// former verifyArtistPath guard, removed in #2419, compared got != sent against
// a peer that echoes back whatever it was sent, so it could never fire).
func TestSyncRename_GuardConsultsThePeer(t *testing.T) {
	p, _ := guardFixture(t, connection.TypeEmby, nil)

	lister := fakeRootLister{roots: []string{"/music"}}
	orig := renameRootListerFactory
	l := &lister
	renameRootListerFactory = func(_ *connection.Connection, _ *slog.Logger) (rootLister, bool) {
		return l, true
	}
	t.Cleanup(func() { renameRootListerFactory = orig })

	if _, err := p.SyncRename(context.Background(), "a1", "/music/Old", "/music/New"); err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if got := atomic.LoadInt32(&l.called); got != 1 {
		t.Errorf("root lister consulted %d times, want 1: the guard must actually query the peer's roots", got)
	}
}
