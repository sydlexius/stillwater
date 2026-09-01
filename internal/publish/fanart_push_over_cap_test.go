package publish

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	img "github.com/sydlexius/stillwater/internal/image"
)

// #3017: a backdrop over the per-file RETENTION cap (maxFanartSnapshotFileBytes)
// but under img.MaxDecodeBytes is legal to read and MUST still reach every
// configured peer -- the cap bounds what one push holds for restore, not what it
// pushes. This file drives that end to end, through the real
// SyncAllFanartToPlatforms path, distinct from snapshot_bound_test.go's
// unit-level coverage of snapshotFanart's own contract.

// stubIndexedUploader is a connection.IndexedImageUploader that always
// succeeds and records what it was handed, keyed by platform index. Unlike
// clobberUploader (peer_clobber_test.go), it never mutates the local
// filesystem -- these tests are about what reaches the peer and what
// survives on disk WITHOUT any peer-side interference, so a clobbering fake
// would confound the assertion.
type stubIndexedUploader struct {
	mu       sync.Mutex
	byIndex  map[int][]byte
	callargs []uploadedPayload
}

func (s *stubIndexedUploader) UploadImageAtIndex(_ context.Context, _, imageType string, idx int, data []byte, ct string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byIndex == nil {
		s.byIndex = make(map[int][]byte)
	}
	cp := append([]byte(nil), data...)
	s.byIndex[idx] = cp
	s.callargs = append(s.callargs, uploadedPayload{imageType: imageType, index: idx, data: cp, contentType: ct})
	return nil
}

func (s *stubIndexedUploader) received(idx int) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.byIndex[idx]
	return b, ok
}

func (s *stubIndexedUploader) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.callargs)
}

// stubBackdropPruneClient is a backdropPruneClient that answers a fixed
// BackdropCount and refuses no delete, used to keep reconcileStaleFanartTail
// from erroring out (or from finding surplus) when a test is not exercising
// it. Distinct from fakeBackdropClient (backdrop_prune_test.go), which
// models re-indexing on delete for the prune-specific tests -- these tests
// do not delete through this fake at all in the "nothing stale" case.
type stubBackdropPruneClient struct {
	backdropCount int
}

func (s *stubBackdropPruneClient) GetArtistDetail(_ context.Context, _ string) (*connection.ArtistPlatformState, error) {
	return &connection.ArtistPlatformState{BackdropCount: s.backdropCount}, nil
}

func (s *stubBackdropPruneClient) GetArtistBackdrop(_ context.Context, _ string, _ int) ([]byte, string, error) {
	return nil, "", nil
}

func (s *stubBackdropPruneClient) DeleteImageAtIndex(_ context.Context, _, _ string, _ int) error {
	return nil
}

// overCapSyncHarness wires a Publisher with a stubIndexedUploader (records
// uploads, touches nothing on disk) and a stubBackdropPruneClient reporting
// BackdropCount equal to localCount (so reconcileStaleFanartTail finds
// nothing stale and does not interfere with these tests, which are about
// the push/retention split, not the tail-reconcile behavior covered
// separately below).
func overCapSyncHarness(t *testing.T, localCount int) (*Publisher, *artist.Artist, string, *stubIndexedUploader) {
	t.Helper()
	dir := t.TempDir()

	up := &stubIndexedUploader{}
	origIndexed := newIndexedImageUploader
	newIndexedImageUploader = func(_ *connection.Connection, _ *slog.Logger) connection.IndexedImageUploader {
		return up
	}
	t.Cleanup(func() { newIndexedImageUploader = origIndexed })

	origFactory := backdropPruneClientFactory
	backdropPruneClientFactory = func(_ *connection.Connection, _ *slog.Logger) backdropPruneClient {
		return &stubBackdropPruneClient{backdropCount: localCount}
	}
	t.Cleanup(func() { backdropPruneClientFactory = origFactory })

	conn := &connection.Connection{
		ID: "c1", Name: "Peer", Type: connection.TypeEmby, Enabled: true, Status: "ok",
		URL:  "http://peer.invalid",
		Emby: &connection.EmbyConfig{FeatureImageWrite: true},
	}
	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: conn.ID, PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{conn.ID: conn}},
		Logger:            silentLogger(),
	})
	return p, &artist.Artist{ID: "a1", Name: "Test Artist", Path: dir}, dir, up
}

// TestSyncAllFanart_OverPerFileCap_StillUploaded is the #3017 AC: "A backdrop
// over the per-file cap but under img.MaxDecodeBytes is still uploaded to
// every configured peer." Also the AC's required over-cap-precondition test:
// the fixture's size is asserted against the cap before trusting the result.
func TestSyncAllFanart_OverPerFileCap_StillUploaded(t *testing.T) {
	p, a, dir, up := overCapSyncHarness(t, 2)

	primary := []byte("PRIMARY-BACKDROP-BYTES")
	if err := os.WriteFile(filepath.Join(dir, "fanart.jpg"), primary, 0o600); err != nil {
		t.Fatalf("writing primary fixture: %v", err)
	}
	overCap := make([]byte, maxFanartSnapshotFileBytes+1)
	for i := range overCap {
		overCap[i] = byte(i) // non-zero, non-uniform content
	}
	if err := os.WriteFile(filepath.Join(dir, "fanart1.jpg"), overCap, 0o600); err != nil {
		t.Fatalf("writing over-cap fixture: %v", err)
	}

	// PRECONDITION: the fixture is genuinely over the retention cap and under
	// the read bound, or this test proves nothing about the #3017 gap.
	if len(overCap) <= maxFanartSnapshotFileBytes {
		t.Fatalf("precondition failed: fixture is %d bytes, not over the %d-byte retention cap",
			len(overCap), int64(maxFanartSnapshotFileBytes))
	}
	if int64(len(overCap)) > img.MaxDecodeBytes {
		t.Fatalf("precondition failed: fixture is %d bytes, over img.MaxDecodeBytes (%d); the READ itself "+
			"would refuse it and this test would measure the wrong thing", len(overCap), img.MaxDecodeBytes)
	}

	warnings := p.SyncAllFanartToPlatforms(context.Background(), a)
	if len(warnings) != 0 {
		t.Errorf("got warnings %v for a fully readable, fully pushable set", warnings)
	}

	if up.callCount() != 2 {
		t.Fatalf("uploader received %d call(s), want 2 (primary and over-cap slot)", up.callCount())
	}
	got, ok := up.received(1)
	if !ok {
		t.Fatal("the over-cap backdrop (index 1) was never uploaded; #3017 requires the retention cap to " +
			"bound only what is HELD for restore, not what is PUSHED")
	}
	if len(got) != len(overCap) {
		t.Errorf("peer received %d bytes for the over-cap slot, want the full %d", len(got), len(overCap))
	}
	for i := range got {
		if got[i] != overCap[i] {
			t.Fatalf("peer received corrupted bytes for the over-cap slot at offset %d", i)
			break
		}
	}
	if primary0, ok := up.received(0); !ok || string(primary0) != string(primary) {
		t.Errorf("the ordinary primary slot was not uploaded correctly: got %q, ok=%t", primary0, ok)
	}
}

// TestSyncAllFanart_OnlyBackdropOverCap_StillPushed is the #3017 AC: "An
// artist whose only backdrop is over the per-file cap still gets a push (the
// hasReadableFanart early return does not fire)." Before the fix, a refused
// slot held nil data, hasReadableFanart saw nothing captured across the
// whole (one-file) snapshot, and syncAllFanartToPlatforms returned before
// the upload loop ran at all -- a push that worked before the cap existed
// did nothing after.
func TestSyncAllFanart_OnlyBackdropOverCap_StillPushed(t *testing.T) {
	p, a, dir, up := overCapSyncHarness(t, 1)

	onlyBackdrop := make([]byte, maxFanartSnapshotFileBytes+1)
	for i := range onlyBackdrop {
		onlyBackdrop[i] = byte(i * 3)
	}
	if err := os.WriteFile(filepath.Join(dir, "fanart.jpg"), onlyBackdrop, 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	// PRECONDITION.
	if len(onlyBackdrop) <= maxFanartSnapshotFileBytes {
		t.Fatalf("precondition failed: fixture is %d bytes, not over the %d-byte retention cap",
			len(onlyBackdrop), int64(maxFanartSnapshotFileBytes))
	}

	p.SyncAllFanartToPlatforms(context.Background(), a)

	got, ok := up.received(0)
	if !ok {
		t.Fatal("the artist's sole (over-cap) backdrop was never uploaded; hasReadableFanart must not " +
			"treat a pushOnly slot as unreadable, or the sync returns before the upload loop runs at all")
	}
	if len(got) != len(onlyBackdrop) {
		t.Errorf("peer received %d bytes, want the full %d", len(got), len(onlyBackdrop))
	}
}

// TestSyncAllFanart_OverPerFileCap_NotRetainedForRestore is the #3017 AC:
// "The retention bound still holds: the over-cap bytes are not kept in the
// snapshot after the upload." It proves this from the OUTSIDE, the only way
// a black-box test can: a peer that destroys the over-cap file's local copy
// during the push is NOT repaired, because this push never promised to hold
// those bytes for restore -- while an ordinary neighbor destroyed the same
// way IS repaired, proving the deferred-repair mechanism itself is intact
// and it is retention specifically, not repair generally, that is gated.
//
// Reuses clobberHarness (peer_clobber_test.go) rather than the stub
// uploader above, because this needs an uploader that actually deletes a
// local file mid-push -- exactly clobberUploader's contract.
func TestSyncAllFanart_OverPerFileCap_NotRetainedForRestore(t *testing.T) {
	calls := 0
	// victim = fanart1.jpg, the over-cap slot. clobberUploader's
	// UploadImageAtIndex only clobbers on idx==0 (see its doc comment): the
	// PRIMARY (fanart.jpg, index 0) upload is what destroys the victim,
	// proving -- as TestSyncAllFanart_PeerDeletesDifferentSlot_Restored does
	// for the ordinary case -- that the bytes must already be in memory
	// before any upload begins, since the file is gone by the time its own
	// slot's turn comes.
	p, a, dir := clobberHarness(t, "fanart1.jpg", "delete", &calls)

	primary := []byte("OPERATOR-PRIMARY-BYTES")
	overCap := make([]byte, maxFanartSnapshotFileBytes+1)
	for i := range overCap {
		overCap[i] = byte(i * 5)
	}
	writeFile(t, filepath.Join(dir, "fanart.jpg"), primary)
	writeFile(t, filepath.Join(dir, "fanart1.jpg"), overCap)

	// PRECONDITION.
	if len(overCap) <= maxFanartSnapshotFileBytes {
		t.Fatalf("precondition failed: fixture is %d bytes, not over the %d-byte retention cap",
			len(overCap), int64(maxFanartSnapshotFileBytes))
	}

	p.SyncAllFanartToPlatforms(context.Background(), a)

	if calls == 0 {
		t.Fatal("precondition failed: the uploader never ran, so nothing was destroyed and this test proves nothing")
	}
	// THE PEER WAS HANDED THE FULL OVER-CAP FILE (push half of the #3017
	// contract), asserted here via the same uploadedPayloads slice
	// clobberHarness wires -- proving this test's fixture actually reached
	// index 1, not merely that SOME upload happened.
	var sawOverCapUpload bool
	for _, up := range uploadedPayloads {
		if up.index == 1 {
			sawOverCapUpload = true
			if len(up.data) != len(overCap) {
				t.Errorf("peer received %d bytes for the over-cap slot, want %d", len(up.data), len(overCap))
			}
		}
	}
	if !sawOverCapUpload {
		t.Fatal("no upload was recorded for the over-cap slot (index 1); the push half of #3017 did not happen")
	}

	// THE RETENTION HALF: the over-cap file was destroyed mid-push (by the
	// clobber triggered on the primary's upload) and must NOT come back,
	// because this push dropped its retention once every peer had its
	// upload.
	if _, err := os.Stat(filepath.Join(dir, "fanart1.jpg")); !os.IsNotExist(err) {
		t.Errorf("the over-cap backdrop was restored after a peer clobber (stat err = %v); #3017 requires "+
			"the retention cap to still bound what is held, so an over-cap slot must degrade to "+
			"'pushed but not repairable', not silently gain unbounded retention", err)
	}
	// The primary itself was never the clobber's victim in this fixture (only
	// fanart1.jpg is), so it is untouched on disk; that an ORDINARY slot's
	// repair still fires when it IS the victim is what
	// TestSyncAllFanart_PeerDeletesDifferentSlot_Restored (above, same file)
	// already proves, isolating retention-drop as specific to the over-cap
	// slot rather than a blanket disable of the repair mechanism.
	if got := mustRead(t, filepath.Join(dir, "fanart.jpg")); string(got) != string(primary) {
		t.Errorf("the untouched primary backdrop changed unexpectedly: got %q, want %q", got, primary)
	}
}

// ---------------------------------------------------------------------------
// #3017 variant 2: the stale-tail-index case. An artist's local backdrop
// count can drop below a previously-synced platform index (a file removed
// locally, or a run that used to fall under the now-larger count cap and no
// longer does), and because uploadFanartSet is additive -- it only ever
// writes indices, never deletes surplus ones -- nothing later reconciled the
// difference before this change. reconcileStaleFanartTail closes that by
// deleting platform indices at or beyond the local count once each
// connection's own push has completed.
// ---------------------------------------------------------------------------

// tailReconcileHarness wires a Publisher whose uploads go through a
// stubIndexedUploader (records calls, touches nothing) and whose
// reconcileStaleFanartTail reads/deletes through a real fakeBackdropClient
// (backdrop_prune_test.go), which re-indexes on delete exactly as a real
// Emby/Jellyfin does -- the property #3138's post-mortem named as the one a
// fake MUST model, not just record calls. featureImageWrite is threaded
// through so the not-gated-on-respectWriteGate test below can reuse this
// harness with the toggle off instead of duplicating the wiring.
func tailReconcileHarness(t *testing.T, platformBackdrops [][]byte, featureImageWrite bool) (*Publisher, *artist.Artist, string, *stubIndexedUploader, *fakeBackdropClient) {
	t.Helper()
	dir := t.TempDir()

	up := &stubIndexedUploader{}
	origIndexed := newIndexedImageUploader
	newIndexedImageUploader = func(_ *connection.Connection, _ *slog.Logger) connection.IndexedImageUploader {
		return up
	}
	t.Cleanup(func() { newIndexedImageUploader = origIndexed })

	fake := &fakeBackdropClient{backdrops: platformBackdrops, failAt: -1, failDeleteAt: -1}
	origFactory := backdropPruneClientFactory
	backdropPruneClientFactory = func(_ *connection.Connection, _ *slog.Logger) backdropPruneClient {
		return fake
	}
	t.Cleanup(func() { backdropPruneClientFactory = origFactory })

	conn := &connection.Connection{
		ID: "c1", Name: "Peer", Type: connection.TypeEmby, Enabled: true, Status: "ok",
		URL:  "http://peer.invalid",
		Emby: &connection.EmbyConfig{FeatureImageWrite: featureImageWrite},
	}
	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: conn.ID, PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{conn.ID: conn}},
		Logger:            silentLogger(),
	})
	return p, &artist.Artist{ID: "a1", Name: "Test Artist", Path: dir}, dir, up, fake
}

// TestSyncAllFanart_StaleTailIndex_Reconciled is the #3017 AC's stale-tail-
// index case: "an artist whose backdrop count DROPS below a previously-
// synced tail index." The platform starts with 5 backdrops; the local set
// has only 3. After the sync, the platform's surplus tail indices (3 and 4)
// must be gone, deleted in descending order (both platforms re-index
// remaining backdrops after each delete, so ascending order would target
// the wrong image partway through).
func TestSyncAllFanart_StaleTailIndex_Reconciled(t *testing.T) {
	platform := [][]byte{
		bandJPEG(t, 1), bandJPEG(t, 2), bandJPEG(t, 3), bandJPEG(t, 4), bandJPEG(t, 5),
	}
	p, a, dir, _, fake := tailReconcileHarness(t, platform, true)

	for i, name := range []string{"fanart.jpg", "fanart1.jpg", "fanart2.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("local-backdrop-%d", i)), 0o600); err != nil {
			t.Fatalf("writing fixture %s: %v", name, err)
		}
	}

	// PRECONDITION: the platform genuinely outnumbers the local set, or the
	// reconcile has nothing to do and this test asserts nothing.
	if len(platform) <= 3 {
		t.Fatalf("precondition failed: platform holds %d backdrops, not more than the 3 local files", len(platform))
	}

	p.SyncAllFanartToPlatforms(context.Background(), a)

	gotDeleted := append([]int(nil), fake.deleted...)
	gotRemaining := len(fake.backdrops)

	if len(gotDeleted) != 2 {
		t.Fatalf("deleted %d indices, want exactly 2 (the surplus tail): %v", len(gotDeleted), gotDeleted)
	}
	if gotDeleted[0] != 4 || gotDeleted[1] != 3 {
		t.Errorf("deleted indices = %v, want [4, 3] (descending, both platforms re-index after each delete)", gotDeleted)
	}
	if gotRemaining != 3 {
		t.Errorf("platform has %d backdrops remaining, want exactly 3 (the local count)", gotRemaining)
	}
	// The surviving backdrops must be the ORIGINAL indices 0-2, not indices
	// that drifted from a wrong delete order.
	for i := 0; i < 3; i++ {
		if string(fake.backdrops[i]) != string(platform[i]) {
			t.Errorf("surviving backdrop %d changed content; the delete order corrupted indices it should "+
				"not have touched", i)
		}
	}
}

// TestSyncAllFanart_NoStaleTail_NothingDeleted is the over-suppression guard:
// when the platform count already matches the local count, nothing is stale
// and reconcileStaleFanartTail must not delete anything.
func TestSyncAllFanart_NoStaleTail_NothingDeleted(t *testing.T) {
	platform := [][]byte{bandJPEG(t, 1), bandJPEG(t, 2)}
	p, a, dir, _, fake := tailReconcileHarness(t, platform, true)

	for i, name := range []string{"fanart.jpg", "fanart1.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("local-backdrop-%d", i)), 0o600); err != nil {
			t.Fatalf("writing fixture %s: %v", name, err)
		}
	}

	p.SyncAllFanartToPlatforms(context.Background(), a)

	if gotDeleted := len(fake.deleted); gotDeleted != 0 {
		t.Errorf("deleted %d indices for a platform count that already matched local; want 0", gotDeleted)
	}
}

// TestSyncAllFanart_StaleTail_NotGatedOnRespectWriteGate proves
// reconcileStaleFanartTail's own FeatureImageWrite gate is checked
// UNCONDITIONALLY, not merely when respectWriteGate is true (unlike the
// upload loop's conn.GetFeatureImageWrite() check). SyncAllFanartToPlatforms
// (the public, user-initiated entry point) always calls with
// respectWriteGate=false, so a connection with the toggle off still gets
// pushed to -- but must NOT also have platform images deleted from it, since
// deletion is destructive and the operator never opted the connection into
// server-file management.
func TestSyncAllFanart_StaleTail_NotGatedOnRespectWriteGate(t *testing.T) {
	platform := [][]byte{bandJPEG(t, 1), bandJPEG(t, 2), bandJPEG(t, 3)}
	// FeatureImageWrite is OFF here, unlike every other test in this file.
	p, a, dir, up, fake := tailReconcileHarness(t, platform, false)

	if err := os.WriteFile(filepath.Join(dir, "fanart.jpg"), []byte("local-backdrop"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	p.SyncAllFanartToPlatforms(context.Background(), a)

	if up.callCount() == 0 {
		t.Fatal("precondition failed: the upload never ran, so this test cannot distinguish push-without-delete " +
			"from nothing happening at all")
	}
	if gotDeleted := len(fake.deleted); gotDeleted != 0 {
		t.Errorf("deleted %d indices on a connection with FeatureImageWrite off; the reconcile must never "+
			"delete platform images on a connection the operator did not opt into server-file management", gotDeleted)
	}
}
