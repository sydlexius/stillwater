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
}

func (c *clobberUploader) UploadImage(_ context.Context, _, _ string, _ []byte, _ string) error {
	*c.calls++
	return c.clobber()
}

func (c *clobberUploader) UploadImageAtIndex(_ context.Context, _, _ string, idx int, _ []byte, _ string) error {
	*c.calls++
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
	up := &clobberUploader{victim: victim, mode: mode, calls: calls, indices: &uploadedIndices}

	origSingle := newImageUploader
	origIndexed := newIndexedImageUploader
	newImageUploader = func(_ *connection.Connection, _ *slog.Logger) connection.ImageUploader {
		return up
	}
	newIndexedImageUploader = func(_ *connection.Connection, _ *slog.Logger) connection.IndexedImageUploader {
		return up
	}
	t.Cleanup(func() {
		newImageUploader = origSingle
		newIndexedImageUploader = origIndexed
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

	p.reassertLocalImage(a, "banner", victim, []byte("REPLACEMENT"), time.Now().Add(-time.Hour), []string{"Peer"})

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
