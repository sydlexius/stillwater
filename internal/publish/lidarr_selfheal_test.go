package publish

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/lidarr"
)

// fakeLidarrLister stands in for a *lidarr.Client's GetArtists so the self-heal
// tests never touch a real Lidarr peer. calls counts invocations so a test can
// assert the idempotency/disabled short-circuits actually avoid the round-trip.
type fakeLidarrLister struct {
	artists []lidarr.Artist
	err     error
	calls   int
}

func (f *fakeLidarrLister) GetArtists(_ context.Context) ([]lidarr.Artist, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.artists, nil
}

// withFakeLidarrListers swaps lidarrArtistListerFactory for the duration of a
// test, dispatching by connection ID so multi-connection tests can rig one
// connection to error while another matches. A connection absent from the map
// gets an empty lister (no artists -> no match). Restored on cleanup, mirroring
// withFakePathUpdater / swapMergeRefresherFactory. Not parallel-safe.
func withFakeLidarrListers(t *testing.T, byConn map[string]*fakeLidarrLister) {
	t.Helper()
	orig := lidarrArtistListerFactory
	lidarrArtistListerFactory = func(conn *connection.Connection, _ *slog.Logger) lidarrArtistLister {
		if l, ok := byConn[conn.ID]; ok {
			return l
		}
		return &fakeLidarrLister{}
	}
	t.Cleanup(func() { lidarrArtistListerFactory = orig })
}

func lidarrConn(id string, enabled bool) *connection.Connection {
	return &connection.Connection{ID: id, Name: id, Type: connection.TypeLidarr, URL: "http://" + id, Enabled: enabled}
}

// --- selfHealLidarrLinks unit tests (cases a, c, d, e, f, g + best-effort) ---

// (a) A Lidarr artist whose ForeignArtistID matches the MBID is stamped via
// SetPlatformID and returned in the linked map keyed by connection ID with the
// numeric platform ID. Broken-behavior guard: an implementation that returned
// the map but skipped SetPlatformID (or vice versa) fails one of the two
// assertions; matching on the WRONG artist (e.g. name fallback) would stamp the
// wrong numeric ID and fail the value check.
func TestSelfHealLidarrLinks_Match(t *testing.T) {
	withFakeLidarrListers(t, map[string]*fakeLidarrLister{
		"c-lid": {artists: []lidarr.Artist{
			{ID: 7, ForeignArtistID: "other-mbid"},
			{ID: 42, ForeignArtistID: "MBID-ABC"}, // case-insensitive match
		}},
	})
	lister := &fakePlatformLister{}
	p := New(Deps{
		ArtistService:     lister,
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{"c-lid": lidarrConn("c-lid", true)}},
		Logger:            silentLogger(),
	})

	got := p.selfHealLidarrLinks(context.Background(), "a1", "mbid-abc", map[string]bool{})

	if len(got) != 1 || got["c-lid"] != "42" {
		t.Fatalf("linked = %v, want {c-lid:42}", got)
	}
	if len(lister.setCalls) != 1 {
		t.Fatalf("SetPlatformID calls = %d, want 1", len(lister.setCalls))
	}
	if c := lister.setCalls[0]; c.artistID != "a1" || c.connectionID != "c-lid" || c.platformArtistID != "42" {
		t.Errorf("SetPlatformID call = %+v, want {a1 c-lid 42}", c)
	}
}

// (c) No Lidarr artist matches the MBID: nothing is stamped or returned, but
// the connection WAS queried (calls==1). Guard: an implementation that stamped
// on no-match would populate setCalls.
func TestSelfHealLidarrLinks_NoMatch(t *testing.T) {
	l := &fakeLidarrLister{artists: []lidarr.Artist{{ID: 7, ForeignArtistID: "nope"}}}
	withFakeLidarrListers(t, map[string]*fakeLidarrLister{"c-lid": l})
	lister := &fakePlatformLister{}
	p := New(Deps{
		ArtistService:     lister,
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{"c-lid": lidarrConn("c-lid", true)}},
		Logger:            silentLogger(),
	})

	got := p.selfHealLidarrLinks(context.Background(), "a1", "mbid-abc", map[string]bool{})

	if len(got) != 0 {
		t.Errorf("linked = %v, want empty", got)
	}
	if len(lister.setCalls) != 0 {
		t.Errorf("SetPlatformID calls = %d, want 0", len(lister.setCalls))
	}
	if l.calls != 1 {
		t.Errorf("GetArtists calls = %d, want 1", l.calls)
	}
}

// (d) A Lidarr GetArtists error is swallowed: the returned map is empty, no
// stamp happens, and crucially selfHealLidarrLinks does not panic or propagate
// (it has no error return). The rename/merge best-effort guarantee at the call
// site is covered by TestSyncRename_SelfHealError_RenameStillSucceeds and
// TestSyncMergeRefresh_SelfHealRefreshError_NoOuterError below.
func TestSelfHealLidarrLinks_GetArtistsError_BestEffort(t *testing.T) {
	withFakeLidarrListers(t, map[string]*fakeLidarrLister{
		"c-lid": {err: errors.New("lidarr 500")},
	})
	lister := &fakePlatformLister{}
	p := New(Deps{
		ArtistService:     lister,
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{"c-lid": lidarrConn("c-lid", true)}},
		Logger:            silentLogger(),
	})

	got := p.selfHealLidarrLinks(context.Background(), "a1", "mbid-abc", map[string]bool{})

	if len(got) != 0 {
		t.Errorf("linked = %v, want empty on GetArtists error", got)
	}
	if len(lister.setCalls) != 0 {
		t.Errorf("SetPlatformID calls = %d, want 0 on GetArtists error", len(lister.setCalls))
	}
}

// (e) Idempotency: a connection already present in alreadyLinked is skipped
// BEFORE GetArtists, so no wasted round-trip and no re-stamp. Guard: an
// implementation that queried first and filtered later would leave calls==1.
func TestSelfHealLidarrLinks_Idempotent_AlreadyLinked(t *testing.T) {
	l := &fakeLidarrLister{artists: []lidarr.Artist{{ID: 42, ForeignArtistID: "mbid-abc"}}}
	withFakeLidarrListers(t, map[string]*fakeLidarrLister{"c-lid": l})
	lister := &fakePlatformLister{}
	p := New(Deps{
		ArtistService:     lister,
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{"c-lid": lidarrConn("c-lid", true)}},
		Logger:            silentLogger(),
	})

	got := p.selfHealLidarrLinks(context.Background(), "a1", "mbid-abc", map[string]bool{"c-lid": true})

	if len(got) != 0 {
		t.Errorf("linked = %v, want empty (already linked)", got)
	}
	if l.calls != 0 {
		t.Errorf("GetArtists calls = %d, want 0 (skipped before round-trip)", l.calls)
	}
	if len(lister.setCalls) != 0 {
		t.Errorf("SetPlatformID calls = %d, want 0", len(lister.setCalls))
	}
}

// (f) Two enabled Lidarr connections both match: both are linked and stamped.
// Guard: an implementation that returned on the first match would only link one.
func TestSelfHealLidarrLinks_MultiConn(t *testing.T) {
	withFakeLidarrListers(t, map[string]*fakeLidarrLister{
		"c-lid1": {artists: []lidarr.Artist{{ID: 42, ForeignArtistID: "mbid-abc"}}},
		"c-lid2": {artists: []lidarr.Artist{{ID: 99, ForeignArtistID: "mbid-abc"}}},
	})
	lister := &fakePlatformLister{}
	p := New(Deps{
		ArtistService: lister,
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-lid1": lidarrConn("c-lid1", true),
			"c-lid2": lidarrConn("c-lid2", true),
		}},
		Logger: silentLogger(),
	})

	got := p.selfHealLidarrLinks(context.Background(), "a1", "mbid-abc", map[string]bool{})

	if len(got) != 2 || got["c-lid1"] != "42" || got["c-lid2"] != "99" {
		t.Fatalf("linked = %v, want {c-lid1:42, c-lid2:99}", got)
	}
	if len(lister.setCalls) != 2 {
		t.Errorf("SetPlatformID calls = %d, want 2", len(lister.setCalls))
	}
}

// (g) A disabled Lidarr connection is skipped: never queried, never linked.
// Guard: gating on the wrong field (or not gating) would query the disabled
// connection (calls==1) and link it.
func TestSelfHealLidarrLinks_DisabledConnSkipped(t *testing.T) {
	l := &fakeLidarrLister{artists: []lidarr.Artist{{ID: 42, ForeignArtistID: "mbid-abc"}}}
	withFakeLidarrListers(t, map[string]*fakeLidarrLister{"c-off": l})
	lister := &fakePlatformLister{}
	p := New(Deps{
		ArtistService:     lister,
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{"c-off": lidarrConn("c-off", false)}},
		Logger:            silentLogger(),
	})

	got := p.selfHealLidarrLinks(context.Background(), "a1", "mbid-abc", map[string]bool{})

	if len(got) != 0 {
		t.Errorf("linked = %v, want empty (disabled)", got)
	}
	if l.calls != 0 {
		t.Errorf("GetArtists calls = %d, want 0 (disabled connection not queried)", l.calls)
	}
}

// A ListByType error is swallowed: empty map, no stamp, no propagation.
func TestSelfHealLidarrLinks_ListByTypeError_BestEffort(t *testing.T) {
	withFakeLidarrListers(t, map[string]*fakeLidarrLister{})
	lister := &fakePlatformLister{}
	p := New(Deps{
		ArtistService:     lister,
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{}, listErr: errors.New("db down")},
		Logger:            silentLogger(),
	})

	got := p.selfHealLidarrLinks(context.Background(), "a1", "mbid-abc", map[string]bool{})

	if len(got) != 0 {
		t.Errorf("linked = %v, want empty on ListByType error", got)
	}
	if len(lister.setCalls) != 0 {
		t.Errorf("SetPlatformID calls = %d, want 0", len(lister.setCalls))
	}
}

// A SetPlatformID error is swallowed: the connection is NOT reported as linked
// (so the caller does not try to refresh/push a link that was never persisted),
// and no panic escapes. Guard: an implementation that added to the map before
// checking the stamp error would report a phantom link.
func TestSelfHealLidarrLinks_SetPlatformIDError_BestEffort(t *testing.T) {
	withFakeLidarrListers(t, map[string]*fakeLidarrLister{
		"c-lid": {artists: []lidarr.Artist{{ID: 42, ForeignArtistID: "mbid-abc"}}},
	})
	lister := &fakePlatformLister{setErr: errors.New("write failed")}
	p := New(Deps{
		ArtistService:     lister,
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{"c-lid": lidarrConn("c-lid", true)}},
		Logger:            silentLogger(),
	})

	got := p.selfHealLidarrLinks(context.Background(), "a1", "mbid-abc", map[string]bool{})

	if len(got) != 0 {
		t.Errorf("linked = %v, want empty when SetPlatformID fails", got)
	}
}

// --- mbidFor / no-MBID (case b) ---

// (b) An artist with no MusicBrainz ID short-circuits: mbidFor returns "" and
// SyncRename never touches Lidarr even though a matching Lidarr artist exists.
// This proves the guard is at the MBID gate, not deeper. Guard: an
// implementation that self-healed regardless of MBID would push to the fake
// path updater.
func TestSyncRename_NoMBID_NoHeal(t *testing.T) {
	fake := &fakePathUpdater{}
	withFakePathUpdater(t, fake)
	l := &fakeLidarrLister{artists: []lidarr.Artist{{ID: 42, ForeignArtistID: "mbid-abc"}}}
	withFakeLidarrListers(t, map[string]*fakeLidarrLister{"c-lid": l})

	p := New(Deps{
		ArtistService:     &fakePlatformLister{ids: nil}, // no existing links
		ArtistGetter:      &fakeArtistGetter{artists: map[string]*artist.Artist{"a1": {ID: "a1", MusicBrainzID: ""}}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{"c-lid": lidarrConn("c-lid", true)}},
		Logger:            silentLogger(),
	})

	results, err := p.SyncRename(context.Background(), "a1", "/old", "/new")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %v, want none (no MBID -> no heal, no links)", results)
	}
	if fake.called != 0 {
		t.Errorf("path updater called %d times, want 0", fake.called)
	}
	if l.calls != 0 {
		t.Errorf("GetArtists calls = %d, want 0 (no MBID short-circuits before listing)", l.calls)
	}
}

// --- Insertion A: SyncRename self-heal + propagate (case a end-to-end) ---

// The normal journey: zero existing platform_ids, so without self-heal the
// rename would no-op. Self-heal links the Lidarr connection by MBID and the new
// path is pushed through UpdateArtistPath. Guard: asserts BOTH the stamp
// (SetPlatformID) AND the actual path push happened with the resolved numeric
// ID -- a heal that linked but did not feed the syncOne loop would leave
// fake.called==0.
func TestSyncRename_SelfHealPropagatesToLidarr(t *testing.T) {
	fake := &fakePathUpdater{}
	withFakePathUpdater(t, fake)
	withFakeLidarrListers(t, map[string]*fakeLidarrLister{
		"c-lid": {artists: []lidarr.Artist{{ID: 42, ForeignArtistID: "mbid-abc"}}},
	})
	lister := &fakePlatformLister{ids: nil}
	p := New(Deps{
		ArtistService:     lister,
		ArtistGetter:      &fakeArtistGetter{artists: map[string]*artist.Artist{"a1": {ID: "a1", MusicBrainzID: "mbid-abc"}}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{"c-lid": lidarrConn("c-lid", true)}},
		Logger:            silentLogger(),
	})

	results, err := p.SyncRename(context.Background(), "a1", "/old", "/new")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if len(results) != 1 || results[0].Result != artist.PlatformRemapOK {
		t.Fatalf("results = %+v, want one OK entry", results)
	}
	if fake.called != 1 {
		t.Fatalf("path updater called %d times, want 1", fake.called)
	}
	if fake.gotID != "42" {
		t.Errorf("path pushed to platform ID %q, want 42", fake.gotID)
	}
	if fake.gotPath != "/new" {
		t.Errorf("path pushed = %q, want /new", fake.gotPath)
	}
	if len(lister.setCalls) != 1 || lister.setCalls[0].platformArtistID != "42" {
		t.Errorf("SetPlatformID calls = %+v, want one stamp of 42", lister.setCalls)
	}
}

// (d at the rename call site) A Lidarr GetArtists error during self-heal must
// not fail the rename: with no other platforms, SyncRename returns (nil, nil)
// exactly as it would with no Lidarr connection at all. Guard: a heal that
// propagated the error would surface a non-nil outer error or a failed entry.
func TestSyncRename_SelfHealError_RenameStillSucceeds(t *testing.T) {
	withFakeLidarrListers(t, map[string]*fakeLidarrLister{
		"c-lid": {err: errors.New("lidarr unreachable")},
	})
	p := New(Deps{
		ArtistService:     &fakePlatformLister{ids: nil},
		ArtistGetter:      &fakeArtistGetter{artists: map[string]*artist.Artist{"a1": {ID: "a1", MusicBrainzID: "mbid-abc"}}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{"c-lid": lidarrConn("c-lid", true)}},
		Logger:            silentLogger(),
	})

	results, err := p.SyncRename(context.Background(), "a1", "/old", "/new")
	if err != nil {
		t.Fatalf("SyncRename returned outer error on best-effort heal failure: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %+v, want none (heal failed, no other platforms)", results)
	}
}

// --- Insertion B: SyncMergeRefresh unions the freshly-linked conn (case h) ---

// A fully-unlinked merge: the survivor has no platform_ids and the affected
// connection set is empty (nothing captured the Lidarr link pre-delete). The
// relaxed len==0 guard lets self-heal run; it discovers the Lidarr link and
// unions it into BOTH survivorByConn (so refreshOne gets the numeric survivor
// ID) and connectionIDs (so the loop visits it). Guard: asserts the refresher
// was called for the freshly linked conn WITH the resolved numeric survivor ID
// -- a union that populated connectionIDs but not survivorByConn would refresh
// with an empty ID; the old len==0 early return would skip the merge entirely.
func TestSyncMergeRefresh_UnionsFreshlyLinkedConn(t *testing.T) {
	rec := &refreshRecorder{}
	swapMergeRefresherFactory(t, func(conn *connection.Connection, _ *slog.Logger) (mergeRefresher, bool) {
		return rec.forConn(conn), true
	})
	withFakeLidarrListers(t, map[string]*fakeLidarrLister{
		"c-lid": {artists: []lidarr.Artist{{ID: 55, ForeignArtistID: "mbid-abc"}}},
	})
	lister := &fakePlatformLister{ids: nil} // survivor unmapped everywhere
	p := New(Deps{
		ArtistService:     lister,
		ArtistGetter:      &fakeArtistGetter{artists: map[string]*artist.Artist{"surv": {ID: "surv", MusicBrainzID: "mbid-abc"}}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{"c-lid": lidarrConn("c-lid", true)}},
		Logger:            silentLogger(),
	})

	// Empty connectionIDs = fully-unlinked merge.
	results, err := p.SyncMergeRefresh(context.Background(), "surv", nil, nil)
	if err != nil {
		t.Fatalf("SyncMergeRefresh: %v", err)
	}
	if len(results) != 1 || results[0].Result != artist.PlatformRemapOK {
		t.Fatalf("results = %+v, want one OK entry for the healed conn", results)
	}
	// The refresher must have been called for c-lid with the resolved numeric ID.
	rec.assertCalled(t, "c-lid", "55")
	if len(lister.setCalls) != 1 || lister.setCalls[0].platformArtistID != "55" {
		t.Errorf("SetPlatformID calls = %+v, want one stamp of 55", lister.setCalls)
	}
}

// (best-effort at the merge call site) Even when the healed connection's
// refresh itself errors, SyncMergeRefresh returns a nil outer error (the merge
// already committed on disk); the failure lands in the per-connection result.
func TestSyncMergeRefresh_SelfHealRefreshError_NoOuterError(t *testing.T) {
	rec := &refreshRecorder{err: errors.New("refresh 500")}
	swapMergeRefresherFactory(t, func(conn *connection.Connection, _ *slog.Logger) (mergeRefresher, bool) {
		return rec.forConn(conn), true
	})
	withFakeLidarrListers(t, map[string]*fakeLidarrLister{
		"c-lid": {artists: []lidarr.Artist{{ID: 55, ForeignArtistID: "mbid-abc"}}},
	})
	p := New(Deps{
		ArtistService:     &fakePlatformLister{ids: nil},
		ArtistGetter:      &fakeArtistGetter{artists: map[string]*artist.Artist{"surv": {ID: "surv", MusicBrainzID: "mbid-abc"}}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{"c-lid": lidarrConn("c-lid", true)}},
		Logger:            silentLogger(),
	})

	results, err := p.SyncMergeRefresh(context.Background(), "surv", nil, nil)
	if err != nil {
		t.Fatalf("SyncMergeRefresh returned outer error on best-effort refresh failure: %v", err)
	}
	if len(results) != 1 || results[0].Result != artist.PlatformRemapFailed {
		t.Fatalf("results = %+v, want one failed entry", results)
	}
}
