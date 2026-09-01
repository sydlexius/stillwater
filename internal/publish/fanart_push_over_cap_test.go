package publish

import (
	"context"
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

// overCapSyncHarness wires a Publisher with a stubIndexedUploader that
// records uploads and touches nothing on disk.
func overCapSyncHarness(t *testing.T) (*Publisher, *artist.Artist, string, *stubIndexedUploader) {
	t.Helper()
	dir := t.TempDir()

	up := &stubIndexedUploader{}
	origIndexed := newIndexedImageUploader
	newIndexedImageUploader = func(_ *connection.Connection, _ *slog.Logger) connection.IndexedImageUploader {
		return up
	}
	t.Cleanup(func() { newIndexedImageUploader = origIndexed })

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
	p, a, dir, up := overCapSyncHarness(t)

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
	p, a, dir, up := overCapSyncHarness(t)

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

// TestSyncAllFanart_OverPerFileCap_ClobberedByPeer_StillRestored is the
// #3017 C2 regression guard (hostile DO-NOT-SHIP review round).
//
// A prior shape of this test asserted the OPPOSITE of what is correct here,
// and that was itself the C2 defect: dropOverCapRetention ran at function
// RETURN, before the deferred repairAfterPush closure got a chance to run
// (a defer fires LAST, at return, not first), so an over-cap slot's bytes
// were nil'd before the repair could use them. Net effect: a peer clobbering
// the operator's local over-cap file during the push destroyed it with
// NOTHING to restore from -- a 12MB+ backdrop that was completely safe
// before this branch (never pushed, so never touched by a peer) became
// unrecoverable after it. See #2698/#2712 for why repairAfterPush exists at
// all: an Emby 4.10 peer was measured deleting the operator's local file
// during a push it was never asked to delete.
//
// The fix moved dropOverCapRetention INSIDE the deferred closure, after
// repairAfterPush's two passes, so the bytes are still resident when the
// repair needs them. This test proves that ordering: the over-cap file is
// clobbered mid-push exactly as an ordinary backdrop would be, and it comes
// back byte-identical, exactly as TestSyncAllFanart_PeerDeletesDifferentSlot_Restored
// proves for an ordinary (under-cap) backdrop. The retention cap still
// governs what is held ACROSS pushes (nothing outlives this function call
// either way, since snapshot is function-scoped) -- what it must never do is
// cost the CURRENT push's own repair its only copy.
//
// Reuses clobberHarness (peer_clobber_test.go) rather than the stub
// uploader above, because this needs an uploader that actually deletes a
// local file mid-push -- exactly clobberUploader's contract.
func TestSyncAllFanart_OverPerFileCap_ClobberedByPeer_StillRestored(t *testing.T) {
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

	// THE C2 ASSERTION: the over-cap file was destroyed mid-push (by the
	// clobber triggered on the primary's upload) and MUST come back
	// byte-identical, because repairAfterPush ran while the bytes were still
	// resident -- the drop only happens after both its passes complete.
	got := mustRead(t, filepath.Join(dir, "fanart1.jpg"))
	if string(got) != string(overCap) {
		t.Errorf("restored over-cap backdrop does not match what was pushed (got %d bytes, want %d); the "+
			"repair must put back exactly the bytes this push captured", len(got), len(overCap))
	}
	// The primary was also the trigger for the clobber but was never itself
	// destroyed in this fixture (only fanart1.jpg is), so it is untouched.
	if got := mustRead(t, filepath.Join(dir, "fanart.jpg")); string(got) != string(primary) {
		t.Errorf("the untouched primary backdrop changed unexpectedly: got %q, want %q", got, primary)
	}
}

// TestDropOverCapRetention_NilsOverCapEntriesOnly is the direct, unit-level
// half of the #3017 AC "the over-cap bytes are not kept in the snapshot
// after the upload" -- the black-box clobber test above cannot observe this
// on its own, because `snapshot` is local to syncAllFanartToPlatforms and
// goes out of scope (eligible for GC) the instant that function returns
// regardless of whether any entry was nil'd. What actually matters, and
// what this drives directly, is the CONTRACT dropOverCapRetention itself
// promises: given a snapshot with a captured over-cap entry, it nils that
// entry's bytes and leaves every other entry (nil-data, under-cap,
// already-nil) untouched.
func TestDropOverCapRetention_NilsOverCapEntriesOnly(t *testing.T) {
	p := New(Deps{Logger: silentLogger()})

	underCap := []byte("an ordinary backdrop")
	overCapBytes := make([]byte, maxFanartSnapshotFileBytes+1)
	for i := range overCapBytes {
		overCapBytes[i] = byte(i * 11)
	}
	// PRECONDITION: the fixture is genuinely over the cap, or the entry
	// below is not exercising the branch this test is about.
	if int64(len(overCapBytes)) <= maxFanartSnapshotFileBytes {
		t.Fatalf("precondition failed: fixture is %d bytes, not over the %d-byte cap",
			len(overCapBytes), int64(maxFanartSnapshotFileBytes))
	}

	snapshot := []fanartSnapshot{
		{path: "under.jpg", index: 0, data: underCap},
		{path: "over.jpg", index: 1, data: overCapBytes},
		{path: "refused.jpg", index: 2, data: nil}, // never captured; must stay nil, not panic
	}

	p.dropOverCapRetention("a1", snapshot)

	if snapshot[0].data == nil {
		t.Error("the under-cap entry lost its bytes; dropOverCapRetention must only touch over-cap entries")
	}
	if string(snapshot[0].data) != string(underCap) {
		t.Error("the under-cap entry's bytes changed")
	}
	if snapshot[1].data != nil {
		t.Errorf("the over-cap entry retained %d bytes; dropOverCapRetention did not drop it", len(snapshot[1].data))
	}
	if snapshot[2].data != nil {
		t.Error("a nil-data entry gained bytes; dropOverCapRetention must be a pure drop, never a write")
	}
}
