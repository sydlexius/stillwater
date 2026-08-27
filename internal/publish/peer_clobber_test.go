package publish

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/platform"
)

// #2698: a platform peer can DESTROY the operator's local image during the very
// upload Stillwater hands it. Measured on Emby 4.10 in UAT and twice in
// production: the peer stores its own copy under its metadata directory and
// then removes what it considers the previous image -- which, on a shared
// filesystem, is the file Stillwater wrote moments earlier.
//
// These tests substitute an uploader that performs that destruction, because it
// is the only way to exercise the repair without standing up a real Emby. Each
// asserts the OUTCOME on disk (bytes present and correct), never a return code.

// clobberUploader is an ImageUploader that mutates the local filesystem during
// UploadImage, exactly as a real peer does. victim is the path it destroys,
// which is deliberately NOT required to be the file being uploaded: the fanart
// case proves a peer deletes a DIFFERENT slot's file.
type clobberUploader struct {
	victim string
	mode   string // "delete", "overwrite", or "none"
	calls  *int
	// indices records the platform index each fanart upload was sent to, so a
	// test can prove slot numbers are not compacted.
	indices *[]int
	// received records what the peer was actually HANDED, per call. Without it
	// these tests would prove only that the file is restored, and a corruption
	// of the payload or content-type on the way out would pass silently: the
	// restored file would still match, because the restore uses the same bytes
	// the upload was built from.
	received *[]uploadedPayload
}

// uploadedPayload is one upload as the peer saw it.
type uploadedPayload struct {
	imageType   string
	index       int // -1 for the single-image (non-indexed) path
	data        []byte
	contentType string
}

func (c *clobberUploader) record(imageType string, idx int, data []byte, ct string) {
	*c.calls++
	if c.received != nil {
		// Copy: the caller owns data and the publisher may reuse the slice.
		cp := append([]byte(nil), data...)
		*c.received = append(*c.received, uploadedPayload{imageType: imageType, index: idx, data: cp, contentType: ct})
	}
}

func (c *clobberUploader) UploadImage(_ context.Context, _, imageType string, data []byte, ct string) error {
	c.record(imageType, -1, data, ct)
	return c.clobber()
}

func (c *clobberUploader) UploadImageAtIndex(_ context.Context, _, imageType string, idx int, data []byte, ct string) error {
	c.record(imageType, idx, data, ct)
	if c.indices != nil {
		*c.indices = append(*c.indices, idx)
	}
	// Only the FIRST slot's upload destroys the victim, so the test proves a
	// file can be lost before the loop ever reaches it.
	if idx != 0 {
		return nil
	}
	return c.clobber()
}

// GetArtistDetail and GetArtistBackdrop make clobberUploader satisfy
// fanartReplaceClient (#3125 F3: uploadOneImageForSync's fanart branch now
// reads platform state via resolveFanartReplaceTarget before writing). Every
// test in this file predates F3 and is about the clobber-and-repair
// mechanism, not about WHICH index gets written, so this reports an EMPTY
// platform unconditionally -- resolveFanartReplaceTarget's degenerate "zero
// backdrops" case, which always resolves to index 0 with no platform read
// beyond this GetArtistDetail call. That preserves every existing
// assertion in this file (idx0, idx1, ... exactly as recorded before) while
// still exercising the real F3 code path rather than bypassing it.
func (c *clobberUploader) GetArtistDetail(_ context.Context, _ string) (*connection.ArtistPlatformState, error) {
	return &connection.ArtistPlatformState{BackdropCount: 0}, nil
}

func (c *clobberUploader) GetArtistBackdrop(_ context.Context, _ string, _ int) ([]byte, string, error) {
	return nil, "", errors.New("clobberUploader reports zero backdrops; GetArtistBackdrop should never be called")
}

func (c *clobberUploader) clobber() error {
	switch c.mode {
	case "overwrite":
		return os.WriteFile(c.victim, []byte("PEER-OWN-BYTES"), 0o600)
	case "none":
		return nil
	case "delete-then-fail":
		// The peer destroys the file and THEN fails the request. This is the
		// most dangerous real shape (Emby deleting, then a 500 or a deadline),
		// and the repair must still run for it.
		_ = os.Remove(c.victim)
		return errors.New("peer rejected the upload after destroying the file")
	default:
		return os.Remove(c.victim)
	}
}

// uploadedIndices records the platform indices the fake peer was asked to write,
// reset by each clobberHarness call. Package-level because the tests in this
// file are serial (they swap package-level uploader factories) -- do not add
// t.Parallel() here without reworking both.
var uploadedIndices []int

// uploadedPayloads records what the fake peer was handed, reset by each
// clobberHarness call. Same serial-tests caveat as uploadedIndices.
var uploadedPayloads []uploadedPayload

// assertHandedToPeer checks the peer received the operator's exact bytes under
// the right content type. CR flagged that asserting only the restored file
// leaves the outbound payload unverified.
func assertHandedToPeer(t *testing.T, want []byte, wantType, wantCT string) {
	t.Helper()
	if len(uploadedPayloads) == 0 {
		t.Fatal("precondition failed: the peer was handed nothing")
	}
	got := uploadedPayloads[0]
	if got.imageType != wantType {
		t.Errorf("peer received image type %q, want %q", got.imageType, wantType)
	}
	if string(got.data) != string(want) {
		t.Errorf("peer received %q, want the operator's bytes %q", got.data, want)
	}
	if got.contentType != wantCT {
		t.Errorf("peer received content type %q, want %q", got.contentType, wantCT)
	}
}

// clobberHarness wires a Publisher whose uploader destroys victim during the
// push. It returns the publisher and the artist rooted at a temp library dir.
func clobberHarness(t *testing.T, victimName, mode string, calls *int) (*Publisher, *artist.Artist, string) {
	t.Helper()

	dir := t.TempDir()
	conn := &connection.Connection{
		ID: "c1", Name: "Peer", Type: connection.TypeEmby, Enabled: true, Status: "ok",
		URL: "http://peer.invalid",
	}
	conn.FeatureManageServerFiles = true

	victim := filepath.Join(dir, victimName)
	uploadedIndices = nil
	uploadedPayloads = nil
	up := &clobberUploader{victim: victim, mode: mode, calls: calls, indices: &uploadedIndices, received: &uploadedPayloads}

	origSingle := newImageUploader
	origIndexed := newIndexedImageUploader
	origFanartReplace := newFanartReplaceClient
	newImageUploader = func(_ *connection.Connection, _ *slog.Logger) connection.ImageUploader {
		return up
	}
	newIndexedImageUploader = func(_ *connection.Connection, _ *slog.Logger) connection.IndexedImageUploader {
		return up
	}
	newFanartReplaceClient = func(_ *connection.Connection, _ *slog.Logger) fanartReplaceClient {
		return up
	}
	t.Cleanup(func() {
		newImageUploader = origSingle
		newIndexedImageUploader = origIndexed
		newFanartReplaceClient = origFanartReplace
	})

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: conn.ID, PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{conn.ID: conn}},
		Logger:            silentLogger(),
	})
	return p, &artist.Artist{ID: "a1", Name: "Test Artist", Path: dir}, dir
}

func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("image missing after push (the peer destroyed it and it was NOT restored): %v", err)
	}
	return b
}

// TestSyncImage_PeerDeletesLocalFile_Restored is the #2698 regression: the peer
// deletes the operator's banner during the upload, and Stillwater must put the
// operator's exact bytes back.
func TestSyncImage_PeerDeletesLocalFile_Restored(t *testing.T) {
	calls := 0
	p, a, dir := clobberHarness(t, "banner.jpg", "delete", &calls)

	want := []byte("OPERATOR-BANNER-BYTES")
	writeFile(t, filepath.Join(dir, "banner.jpg"), want)

	p.SyncImageToPlatforms(context.Background(), a, "banner")

	if calls == 0 {
		t.Fatal("precondition failed: the uploader never ran, so nothing was destroyed and this test proves nothing")
	}
	assertHandedToPeer(t, want, "banner", "image/jpeg")
	if got := mustRead(t, filepath.Join(dir, "banner.jpg")); string(got) != string(want) {
		t.Errorf("restored bytes = %q, want the operator's original %q", got, want)
	}
}

// TestSyncImage_PeerFailsAfterDeleting_StillRestored guards the branch that a
// previous review found unrepaired: the upload errors AFTER the peer already
// destroyed the file. Gating the repair on upload SUCCESS skipped exactly the
// case where the file is most likely gone.
func TestSyncImage_PeerFailsAfterDeleting_StillRestored(t *testing.T) {
	calls := 0
	p, a, dir := clobberHarness(t, "banner.jpg", "delete-then-fail", &calls)

	want := []byte("OPERATOR-BANNER-BYTES")
	writeFile(t, filepath.Join(dir, "banner.jpg"), want)

	warnings := p.SyncImageToPlatforms(context.Background(), a, "banner")

	if calls == 0 {
		t.Fatal("precondition failed: the uploader never ran")
	}
	if len(warnings) == 0 {
		t.Fatal("precondition failed: the upload was expected to report a failure")
	}
	if got := mustRead(t, filepath.Join(dir, "banner.jpg")); string(got) != string(want) {
		t.Errorf("restored bytes = %q, want %q (a failed upload must not skip the repair)", got, want)
	}
}

// TestSyncImage_FanartPeerFailsAfterDeleting_StillRestored is the fanart-branch
// twin of TestSyncImage_PeerFailsAfterDeleting_StillRestored (#3125 review,
// round 1, item 4). The #3125 fix split the per-connection upload into a
// fanart path (indexed) and a non-fanart path (plain); the #2698/#2712
// invariant that a connection is recorded in uploadedTo BEFORE the upload
// result is known -- so a failed upload still triggers the post-push repair
// -- was carried into the fanart branch by construction, but nothing in the
// suite proved it: a mutation from `return true` to `return false` on the
// fanart error path (uploadOneImageForSync's `return true,
// truncateWarning(...)` inside the UploadImageAtIndex error branch) leaves
// the rest of ./internal/publish/ fully green, because every other fanart
// test either does not exercise a failing upload or does not check
// restoration. This test WIRES THE MISBEHAVIOR the same way the banner test
// does: the fake peer deletes the local file and THEN fails the request
// (clobberHarness's "delete-then-fail" mode, driven through
// UploadImageAtIndex since fanart now goes through the indexed uploader),
// and demands the operator's exact bytes come back.
func TestSyncImage_FanartPeerFailsAfterDeleting_StillRestored(t *testing.T) {
	calls := 0
	p, a, dir := clobberHarness(t, "fanart.jpg", "delete-then-fail", &calls)

	// bandJPEG (phash_platform_test.go, same package), not an arbitrary byte
	// string: resolveFanartReplaceTarget's exact-byte comparisons need this
	// fixture to be byte-distinct from whatever else a test compares it
	// against; it is a valid, established fixture, not a decode requirement
	// (CR review round -- the resolver compares raw bytes via
	// image.ContentHash and never decodes or perceptually hashes them; the
	// one decode-shaped check left anywhere is that the resolver rejects
	// empty bytes).
	want := bandJPEG(t, 1)
	writeFile(t, filepath.Join(dir, "fanart.jpg"), want)

	warnings := p.SyncImageToPlatforms(context.Background(), a, "fanart")

	if calls == 0 {
		t.Fatal("precondition failed: the uploader never ran")
	}
	if len(warnings) == 0 {
		t.Fatal("precondition failed: the upload was expected to report a failure")
	}
	if got := mustRead(t, filepath.Join(dir, "fanart.jpg")); string(got) != string(want) {
		t.Errorf("restored bytes = %q, want %q (a failed fanart upload must not skip the repair)", got, want)
	}
}

// TestSyncImage_PeerOverwritesLocalFile_Restored covers the #2533 crop-clobber
// shape: the peer REWRITES the file with its own bytes rather than deleting it.
// An existence check would call this clean, which is why the guard compares
// content.
func TestSyncImage_PeerOverwritesLocalFile_Restored(t *testing.T) {
	calls := 0
	p, a, dir := clobberHarness(t, "banner.jpg", "overwrite", &calls)

	want := []byte("OPERATOR-CROPPED-BYTES")
	writeFile(t, filepath.Join(dir, "banner.jpg"), want)

	p.SyncImageToPlatforms(context.Background(), a, "banner")

	if calls == 0 {
		t.Fatal("precondition failed: the uploader never ran")
	}
	got := mustRead(t, filepath.Join(dir, "banner.jpg"))
	if string(got) == "PEER-OWN-BYTES" {
		t.Fatal("the peer's bytes survived: the operator's cropped image was clobbered (#2533 regression)")
	}
	if string(got) != string(want) {
		t.Errorf("restored bytes = %q, want the operator's original %q", got, want)
	}
}

// TestSyncImage_PngUsesPngContentType proves the content type follows the file
// extension rather than being hardcoded. A peer handed image/jpeg for a PNG can
// reject or transcode it, and the restored-file assertions alone would not
// notice because the restore reuses the same bytes.
func TestSyncImage_PngUsesPngContentType(t *testing.T) {
	calls := 0
	p, a, dir := clobberHarness(t, "logo.png", "delete", &calls)

	want := []byte("OPERATOR-LOGO-BYTES")
	writeFile(t, filepath.Join(dir, "logo.png"), want)

	p.SyncImageToPlatforms(context.Background(), a, "logo")

	if calls == 0 {
		t.Fatal("precondition failed: the uploader never ran")
	}
	assertHandedToPeer(t, want, "logo", "image/png")
	if got := mustRead(t, filepath.Join(dir, "logo.png")); string(got) != string(want) {
		t.Errorf("restored bytes = %q, want %q", got, want)
	}
}

// TestSyncAllFanart_PeerDeletesDifferentSlot_Restored is the cross-file case
// found in UAT: uploading slot 0 made the peer delete slot 1's file, before the
// loop had read it. A per-file check after each upload cannot repair that --
// only bytes snapshotted before the first upload can.
func TestSyncAllFanart_PeerDeletesDifferentSlot_Restored(t *testing.T) {
	calls := 0
	// The victim is the SECOND backdrop, destroyed while slot 0 is uploading.
	p, a, dir := clobberHarness(t, "fanart1.jpg", "delete", &calls)

	wantPrimary := []byte("OPERATOR-BACKDROP-0")
	wantSecond := []byte("OPERATOR-BACKDROP-1")
	writeFile(t, filepath.Join(dir, "fanart.jpg"), wantPrimary)
	writeFile(t, filepath.Join(dir, "fanart1.jpg"), wantSecond)

	p.SyncAllFanartToPlatforms(context.Background(), a)

	if calls == 0 {
		t.Fatal("precondition failed: the uploader never ran")
	}
	assertHandedToPeer(t, wantPrimary, "fanart", "image/jpeg")
	if got := mustRead(t, filepath.Join(dir, "fanart1.jpg")); string(got) != string(wantSecond) {
		t.Errorf("restored slot-1 bytes = %q, want %q", got, wantSecond)
	}
	if got := mustRead(t, filepath.Join(dir, "fanart.jpg")); string(got) != string(wantPrimary) {
		t.Errorf("slot-0 bytes = %q, want %q", got, wantPrimary)
	}
}

// TestReassertLocalImage_UnreadableFile_LeftAlone asserts absent != unreadable:
// when the file cannot be read for a reason other than non-existence, the guard
// must NOT blindly rewrite it, since an unknown state is not a known-absent one.
//
// The unreadable stand-in is a REAL FILE with its permissions removed, not a
// directory. A directory version of this test passes even with the guard
// deleted, because WriteFileAtomic's rename cannot replace a directory anyway --
// it asserted the filesystem's behavior, not ours.
func TestReassertLocalImage_UnreadableFile_LeftAlone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not make a file unreadable")
	}
	dir := t.TempDir()
	victim := filepath.Join(dir, "banner.jpg")
	original := []byte("OPERATOR-BYTES-UNREADABLE")
	writeFile(t, victim, original)
	if err := os.Chmod(victim, 0o000); err != nil {
		t.Fatalf("removing read permission: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(victim, 0o600) })

	p := New(Deps{Logger: silentLogger()})
	a := &artist.Artist{ID: "a1", Name: "Test Artist", Path: dir}

	p.reassertLocalImage(context.Background(), a, "banner", victim, []byte("REPLACEMENT"), time.Now().Add(-time.Hour), pushScope{dir: dir, at: time.Now()}, []string{"Peer"}, repairAllDamage)

	if err := os.Chmod(victim, 0o600); err != nil {
		t.Fatalf("restoring read permission: %v", err)
	}
	got := mustRead(t, victim)
	if string(got) != string(original) {
		t.Errorf("the guard rewrote a file it could not read: got %q, want the untouched %q", got, original)
	}
}

// TestSyncAllFanart_NoReadableSlots_NoUpload asserts that a fanart set whose
// every file failed to read uploads nothing and repairs nothing -- there are no
// bytes to send and none to put back.
func TestSyncAllFanart_NoReadableSlots_NoUpload(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not make a file unreadable")
	}
	calls := 0
	p, a, dir := clobberHarness(t, "unused.jpg", "none", &calls)

	only := filepath.Join(dir, "fanart.jpg")
	writeFile(t, only, []byte("SLOT-0"))
	if err := os.Chmod(only, 0o000); err != nil {
		t.Fatalf("removing read permission: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(only, 0o600) })

	p.SyncAllFanartToPlatforms(context.Background(), a)

	if calls != 0 {
		t.Errorf("uploaded %d time(s) with no readable fanart; expected none", calls)
	}
}

// TestSyncAllFanart_UnreadableSlot_KeepsIndices guards the platform gallery
// against re-indexing. A slot whose bytes cannot be captured must still consume
// its index, or every later backdrop shifts down one on the peer -- and since
// this sync never deletes surplus indices, the stale tail image would survive
// indefinitely.
func TestSyncAllFanart_UnreadableSlot_KeepsIndices(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not make a file unreadable")
	}
	calls := 0
	p, a, dir := clobberHarness(t, "does-not-matter.jpg", "none", &calls)

	// fanart.jpg is slot 0 and is unreadable; fanart1.jpg is slot 1.
	unreadable := filepath.Join(dir, "fanart.jpg")
	writeFile(t, unreadable, []byte("SLOT-0"))
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatalf("removing read permission: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) })
	writeFile(t, filepath.Join(dir, "fanart1.jpg"), []byte("SLOT-1"))

	p.SyncAllFanartToPlatforms(context.Background(), a)

	if len(uploadedIndices) != 1 {
		t.Fatalf("expected exactly one upload (slot 1), got indices %v", uploadedIndices)
	}
	if uploadedIndices[0] != 1 {
		t.Errorf("slot 1 was uploaded at platform index %d; compaction shifted the gallery", uploadedIndices[0])
	}
}

// cancelingClobberUploader destroys the victim file and then CANCELS the
// caller's context, reproducing the exact ordering that makes the deferred
// repair load-bearing: the peer has already eaten the operator's file by the
// time the request goes away.
type cancelingClobberUploader struct {
	victim string
	cancel context.CancelFunc
	calls  int
}

func (c *cancelingClobberUploader) UploadImageAtIndex(_ context.Context, _, _ string, _ int, _ []byte, _ string) error {
	c.calls++
	_ = os.Remove(c.victim)
	c.cancel()
	return nil
}

// TestSyncAllFanart_CanceledAfterPeerDestroyedFile_StillRestored is the
// INTERACTION guard for the #2934 cancellation stop.
//
// Stopping the snapshot/upload path on cancellation must NOT cost the deferred
// re-assertion, and losing that repair would be strictly worse than the defect
// the stop fixes: a cancellation is precisely WHEN a peer is most likely to have
// destroyed a file and nothing else will put it back. The repair deliberately
// runs on a detached context (context.WithoutCancel plus its own deadline) for
// that reason, and this test is what proves the new early return did not put
// itself in front of it.
//
// ORDERING IS THE FIXTURE. The peer deletes the operator's backdrop and THEN
// cancels the request, so the cancellation lands after a peer was genuinely
// reached -- the case the early return must not intercept. The new stop sits in
// the SNAPSHOT, which has already completed by this point, and the repair is
// registered before the upload loop and gated on uploadedTo, which is non-empty
// here. The assertion is the bytes on disk, not a counter.
func TestSyncAllFanart_CanceledAfterPeerDestroyedFile_StillRestored(t *testing.T) {
	dir := t.TempDir()
	want := []byte("OPERATOR-BACKDROP-BYTES")
	victim := filepath.Join(dir, "fanart.jpg")
	writeFile(t, victim, want)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	up := &cancelingClobberUploader{victim: victim, cancel: cancel}
	origIndexed := newIndexedImageUploader
	newIndexedImageUploader = func(_ *connection.Connection, _ *slog.Logger) connection.IndexedImageUploader {
		return up
	}
	t.Cleanup(func() { newIndexedImageUploader = origIndexed })

	conn := &connection.Connection{
		ID: "c1", Name: "Peer", Type: connection.TypeEmby, Enabled: true, Status: "ok",
		URL: "http://peer.invalid",
	}
	conn.FeatureManageServerFiles = true
	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: conn.ID, PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{conn.ID: conn}},
		Logger:            silentLogger(),
	})

	p.SyncAllFanartToPlatforms(ctx, &artist.Artist{ID: "a1", Name: "Test Artist", Path: dir})

	if up.calls == 0 {
		t.Fatal("precondition failed: the uploader never ran, so no peer was reached and the repair " +
			"was never owed -- this test would prove nothing about the interaction")
	}
	if ctx.Err() == nil {
		t.Fatal("precondition failed: the context was not canceled during the push")
	}
	got := mustRead(t, victim)
	if string(got) != string(want) {
		t.Errorf("restored bytes = %q, want the operator's original %q; the cancellation stop is "+
			"preventing the deferred repair from running, which loses operator data for exactly the "+
			"case the repair exists to cover", got, want)
	}
}

// ---------------------------------------------------------------------------
// #2712: the repair must tell a PEER clobber from an OPERATOR delete.
//
// Before this, reassertLocalImage was attribution-blind: it saw only the bytes
// captured before the push and ENOENT afterwards, so an operator deleting a slot
// while a background push for that slot was in flight got the artwork put back.
// The delete handlers now record an explicit intent marker (img.MarkDeleteIntent)
// before they unlink, and the ENOENT branch consults it against pushScope.at --
// the instant THE PUSH BEGAN, stamped at the sync function's entry, not the
// later instant at which it read the bytes.
//
// pushScope.at is set to a PAST instant in these tests so that a marker written
// by img.MarkDeleteIntent (which stamps time.Now) lands strictly after it -- the
// same ordering a real in-flight push sees when the operator deletes mid-push.
//
// pushScope.dir is the RESOLVED image directory, which these tests pass
// explicitly rather than letting the repair re-derive it from the file path.
// TestReassertLocalImage_NestedNamingPattern_OperatorDelete_NotRestored below
// covers the configuration where those two differ.
// ---------------------------------------------------------------------------

// TestReassertLocalImage_OperatorDeletedDuringPush_NotRestored is the #2712
// regression. The file is gone AND the operator's intent to remove it was
// recorded after the push snapshotted its bytes, so the repair must stand down.
func TestReassertLocalImage_OperatorDeletedDuringPush_NotRestored(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "banner.jpg")

	// The push captured these bytes, then the operator deleted the slot. The
	// file's absence is the fixture; there is nothing to write here.
	data := []byte("OPERATOR-BANNER-BYTES")
	snapAt := time.Now().Add(-time.Second)
	img.MarkDeleteIntent(dir, "banner")

	if _, err := os.Stat(victim); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("precondition failed: the victim must be absent for the ENOENT branch to run, stat gave %v", err)
	}
	if !img.DeleteIntentAfter(dir, "banner", snapAt) {
		t.Fatal("precondition failed: the delete marker is not visible to the repair, so this test would " +
			"pass for the wrong reason (it would be asserting nothing about the gate)")
	}

	p := New(Deps{Logger: silentLogger()})
	a := &artist.Artist{ID: "a1", Name: "Test Artist", Path: dir}

	p.reassertLocalImage(context.Background(), a, "banner", victim, data, time.Now().Add(-time.Hour), pushScope{dir: dir, at: snapAt}, []string{"Peer"}, repairAllDamage)

	if _, err := os.Stat(victim); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the repair resurrected an image the operator deliberately deleted (stat err = %v); "+
			"an operator would have to delete it a second time", err)
	}
}

// TestReassertLocalImage_PeerDeleteNoIntent_Restored is the anti-over-suppression
// guard. With no operator delete recorded, an ENOENT after a push is a peer
// clobber and #2698's repair must still fire. A gate made unconditional -- or one
// that treated "no marker" as "stand down" -- fails here.
func TestReassertLocalImage_PeerDeleteNoIntent_Restored(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "banner.jpg")
	want := []byte("OPERATOR-BANNER-BYTES")
	snapAt := time.Now().Add(-time.Second)

	if img.DeleteIntentAfter(dir, "banner", snapAt) {
		t.Fatal("precondition failed: this directory already carries a delete marker, so a stand-down " +
			"would be correct here and the test would prove nothing")
	}

	p := New(Deps{Logger: silentLogger()})
	a := &artist.Artist{ID: "a1", Name: "Test Artist", Path: dir}

	p.reassertLocalImage(context.Background(), a, "banner", victim, want, time.Now().Add(-time.Hour), pushScope{dir: dir, at: snapAt}, []string{"Peer"}, repairAllDamage)

	got := mustRead(t, victim)
	if string(got) != string(want) {
		t.Errorf("restored bytes = %q, want %q; the delete gate is suppressing a genuine peer clobber (#2698 regression)", got, want)
	}
}

// TestReassertLocalImage_StaleIntentPredatingSnapshot_Restored pins the `since`
// comparison at the level that matters. A delete the operator performed BEFORE
// the push looked at the file says nothing about what happened during the push,
// so a gate that merely asked "is there a marker?" would wrongly abandon a real
// peer clobber for the rest of the retention window.
func TestReassertLocalImage_StaleIntentPredatingSnapshot_Restored(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "banner.jpg")
	want := []byte("OPERATOR-REPLACED-BANNER")

	// The operator deleted, then saved a new image; the push snapshotted AFTER
	// both. Any marker on record is older than the snapshot.
	img.MarkDeleteIntent(dir, "banner")
	snapAt := time.Now().Add(time.Millisecond)

	if !img.DeleteIntentAfter(dir, "banner", time.Time{}) {
		t.Fatal("precondition failed: no marker was recorded at all, so this test is not exercising the since comparison")
	}
	if img.DeleteIntentAfter(dir, "banner", snapAt) {
		t.Fatal("precondition failed: the marker is not older than the snapshot")
	}

	p := New(Deps{Logger: silentLogger()})
	a := &artist.Artist{ID: "a1", Name: "Test Artist", Path: dir}

	p.reassertLocalImage(context.Background(), a, "banner", victim, want, time.Now().Add(-time.Hour), pushScope{dir: dir, at: snapAt}, []string{"Peer"}, repairAllDamage)

	got := mustRead(t, victim)
	if string(got) != string(want) {
		t.Errorf("restored bytes = %q, want %q; a delete predating the snapshot must not suppress the repair", got, want)
	}
}

// TestSyncImage_FirstEverSave_PeerClobber_Restored is the case the WITHDRAWN
// exists-flag guard broke: the most ordinary save there is, of a slot nobody has
// ever deleted, clobbered by a peer. It goes through the full push path rather
// than calling the repair directly, so it also proves the new snapAt plumbing in
// SyncImageToPlatforms does not accidentally suppress the common case.
func TestSyncImage_FirstEverSave_PeerClobber_Restored(t *testing.T) {
	calls := 0
	p, a, dir := clobberHarness(t, "banner.jpg", "delete", &calls)

	want := []byte("OPERATOR-FIRST-EVER-BANNER")
	writeFile(t, filepath.Join(dir, "banner.jpg"), want)

	if img.DeleteIntentAfter(dir, "banner", time.Time{}) {
		t.Fatal("precondition failed: a first-ever save must have no delete marker of any age")
	}

	p.SyncImageToPlatforms(context.Background(), a, "banner")

	if calls == 0 {
		t.Fatal("precondition failed: the uploader never ran, so nothing was destroyed")
	}
	if got := mustRead(t, filepath.Join(dir, "banner.jpg")); string(got) != string(want) {
		t.Errorf("restored bytes = %q, want %q; a first-ever save must still be repaired", got, want)
	}
}

// TestReassertLocalImage_OperatorDeleteSuppressesRenumberedPath is THE test the
// key shape exists for, and the one a per-slot marker fails.
//
// The operator deletes one fanart slot. RenumberFanart then COMPACTS survivors
// to contiguous indices, so the file that was fanart5.jpg is renamed down and
// fanart5.jpg no longer exists. A push that snapshotted the set BEFORE the
// renumber still holds fanart5.jpg's path and bytes, reads ENOENT, and -- under a
// marker keyed by (dir, type, SLOT) -- would find no marker for slot 5 and
// restore it. That resurrects deleted artwork under a filename the operator
// never chose and re-grows the set, which is #2712's own bug reproduced by its
// fix. A type-wide marker suppresses it.
//
// The fixture reproduces the compaction concretely rather than asserting on the
// marker API, so it stays honest if the key shape is ever changed underneath.
func TestReassertLocalImage_OperatorDeleteSuppressesRenumberedPath(t *testing.T) {
	dir := t.TempDir()

	// Pre-delete set: six backdrops, slots 0..5.
	slots := []string{"fanart.jpg", "fanart1.jpg", "fanart2.jpg", "fanart3.jpg", "fanart4.jpg", "fanart5.jpg"}
	for i, name := range slots {
		writeFile(t, filepath.Join(dir, name), []byte("OPERATOR-BACKDROP-"+string(rune('0'+i))))
	}
	// The push snapshotted the whole set here, holding slot 5's path and bytes.
	snapAt := time.Now().Add(-time.Second)
	tailPath := filepath.Join(dir, "fanart5.jpg")
	tailData := []byte("OPERATOR-BACKDROP-5")

	// The operator deletes slot 2. The handler marks intent, unlinks, renumbers.
	img.MarkDeleteIntent(dir, "fanart")
	if err := os.Remove(filepath.Join(dir, "fanart2.jpg")); err != nil {
		t.Fatalf("deleting slot 2: %v", err)
	}
	// Compaction: 3->2, 4->3, 5->4. fanart5.jpg ceases to exist.
	for _, mv := range [][2]string{{"fanart3.jpg", "fanart2.jpg"}, {"fanart4.jpg", "fanart3.jpg"}, {"fanart5.jpg", "fanart4.jpg"}} {
		if err := os.Rename(filepath.Join(dir, mv[0]), filepath.Join(dir, mv[1])); err != nil {
			t.Fatalf("renumbering %s -> %s: %v", mv[0], mv[1], err)
		}
	}

	// PRECONDITIONS. Without these the test could pass because the renumber
	// silently did not happen, which is the vacuous version of this assertion.
	if _, err := os.Stat(tailPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("precondition failed: fanart5.jpg must be gone after compaction, stat gave %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the fanart dir: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("precondition failed: expected 5 survivors after deleting 1 of 6, found %d", len(entries))
	}

	p := New(Deps{Logger: silentLogger()})
	a := &artist.Artist{ID: "a1", Name: "Test Artist", Path: dir}

	// The in-flight push verifies its PRE-renumber view of slot 5.
	p.reassertLocalImage(context.Background(), a, "fanart", tailPath, tailData, time.Now().Add(-time.Hour), pushScope{dir: dir, at: snapAt}, []string{"Peer"}, repairAllDamage)

	if _, statErr := os.Stat(tailPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the repair recreated fanart5.jpg after a renumber (stat err = %v); the operator's "+
			"deleted backdrop is back under a shifted filename and the set has re-grown -- this is "+
			"exactly what a per-slot delete marker cannot prevent", statErr)
	}
	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("re-reading the fanart dir: %v", err)
	}
	if len(after) != 5 {
		t.Errorf("the fanart set holds %d files after the repair, want the 5 survivors", len(after))
	}
}

// TestReassertLocalImage_OverwriteIgnoresDeleteIntent keeps the gate confined to
// the ENOENT branch. A delete marker says nothing about a REWRITE, and gating the
// bytes-mismatch branch on it would disable the #2533 crop-clobber repair -- the
// case the whole re-assertion mechanism exists to serve. Deleting one slot must
// not make a peer's overwrite of another survive.
func TestReassertLocalImage_OverwriteIgnoresDeleteIntent(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "banner.jpg")
	want := []byte("OPERATOR-CROPPED-BYTES")
	writeFile(t, victim, []byte("PEER-OWN-BYTES"))

	snapAt := time.Now().Add(-time.Second)
	img.MarkDeleteIntent(dir, "banner")
	if !img.DeleteIntentAfter(dir, "banner", snapAt) {
		t.Fatal("precondition failed: no live delete marker, so this test does not exercise the gate's scope")
	}

	p := New(Deps{Logger: silentLogger()})
	a := &artist.Artist{ID: "a1", Name: "Test Artist", Path: dir}

	p.reassertLocalImage(context.Background(), a, "banner", victim, want, time.Now().Add(-time.Hour), pushScope{dir: dir, at: snapAt}, []string{"Peer"}, repairAllDamage)

	got := mustRead(t, victim)
	if string(got) == "PEER-OWN-BYTES" {
		t.Fatal("the peer's bytes survived an overwrite because a delete marker was present; the delete " +
			"gate has leaked past the ENOENT branch and disabled the #2533 crop-clobber repair")
	}
	if string(got) != string(want) {
		t.Errorf("restored bytes = %q, want the operator's original %q", got, want)
	}
}

// TestReassertLocalImage_NestedNamingPattern_OperatorDelete_NotRestored is the
// regression for the reader and the writer deriving DIFFERENT keys.
//
// THE DEFECT. The delete handlers key their marker on the RESOLVED image
// directory (Router.imageDir). The repair used to key its lookup on
// filepath.Dir of the artwork file it had discovered. Those two strings are
// equal for every default naming pattern, which is why every other test in this
// package passes either way -- but they are not equal in general.
//
// HOW THEY COME APART. platform.ValidateImageNaming rejects a filename holding
// a path separator, and it is called from exactly two places, both on the
// platform-profile create/update handlers. The SETTINGS IMPORT path does not
// call it: platform.ImportCreateTx persists the marshaled ImageNaming
// directly. So an imported profile can carry "sub/folder.jpg", and
// FindExistingImageStrict, which simply joins the directory with the pattern,
// then returns "<dir>/sub/folder.jpg". The repair keyed on "<dir>/sub" while
// the handler marked "<dir>", the keys never met, the gate never fired, and the
// operator's delete was resurrected -- silently, and only on the exact
// configuration that had bypassed validation.
//
// THE FIXTURE drives the real push so the key is derived the way production
// derives it, rather than being handed to reassertLocalImage by the test. The
// operator's delete is recorded during the push's first step (the same prologue
// hook the prologue tests use), and the file itself is destroyed later by the
// clobbering uploader, which is the shape a real mid-push delete presents.
//
// It goes RED against the filepath.Dir derivation: the marker on "<dir>" is
// invisible to a lookup on "<dir>/sub", so the repair rewrites the backdrop the
// operator deleted.
func TestReassertLocalImage_NestedNamingPattern_OperatorDelete_NotRestored(t *testing.T) {
	calls := 0
	p, a, dir := clobberHarness(t, filepath.Join("sub", "folder.jpg"), "delete", &calls)

	// An imported profile whose banner pattern descends into a subdirectory.
	// ValidateImageNaming would refuse this on the API, and the import path
	// never asks it.
	p.platformService = &fakePlatformProvider{profile: &platform.Profile{
		ImageNaming: platform.ImageNaming{Banner: []string{filepath.Join("sub", "folder.jpg")}},
	}}

	victim := filepath.Join(dir, "sub", "folder.jpg")
	if err := os.MkdirAll(filepath.Dir(victim), 0o750); err != nil {
		t.Fatalf("creating the nested artwork directory: %v", err)
	}
	want := []byte("OPERATOR-NESTED-BANNER")
	writeFile(t, victim, want)

	// PRECONDITION: the fixture is only meaningful if the artwork actually sits
	// below the resolved image directory. A pattern that quietly lost its
	// separator would make this test pass against the defect.
	if filepath.Dir(victim) == filepath.Clean(dir) {
		t.Fatalf("precondition failed: %s is not nested below the image directory %s, so the reader's "+
			"old key and the writer's key would coincide and this test proves nothing", victim, dir)
	}

	marker := installPrologueMarker(t, p, dir, "banner")

	p.SyncImageToPlatforms(context.Background(), a, "banner")

	if !marker.called {
		t.Fatal("precondition failed: the prologue never asked for platform IDs, so no delete was ever " +
			"recorded during the push and this test asserts nothing")
	}
	if calls == 0 {
		t.Fatal("precondition failed: the uploader never ran, so the nested banner was never destroyed " +
			"and the repair was never owed")
	}
	// The peer was handed the NESTED file's bytes, which proves discovery
	// honored the pattern rather than finding some default-named file.
	assertHandedToPeer(t, want, "banner", "image/jpeg")
	if !img.DeleteIntentAfter(dir, "banner", time.Time{}) {
		t.Fatal("precondition failed: no delete marker is on record against the resolved image directory")
	}

	if _, err := os.Stat(victim); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the repair resurrected a banner the operator deleted during the push (stat err = %v); "+
			"the lookup key was derived from the artwork path instead of the resolved image directory, "+
			"so it never matched the key the delete handler wrote", err)
	}
}
