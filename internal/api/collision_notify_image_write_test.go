package api

// #2565 -- the two OPERATOR image-write chokepoints wired into #2540's
// cross-artist backdrop collision seam.
//
// These tests share the fixtures #2613 built for the platform-import path
// (collision_notify_populate_test.go): decodableBackdropJPEG, seedCollidingArtist
// and wireCollisionNotifier. The behavior under test is the same contract at two
// more call sites, so it is deliberately pinned the same way.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/event"
	img "github.com/sydlexius/stillwater/internal/image"
)

// seedOwnFanart plants a fanart row with the given phash under artistID ITSELF.
// CompareIdentity excludes the destination artist from the registry, so this
// produces a registry that is populated but yields NO mismatch -- the fail-open
// verdict the "no notification" assertions need to be non-vacuous. Without it,
// an empty registry would make those assertions pass for the wrong reason.
func seedOwnFanart(t *testing.T, r *Router, artistID string, phash uint64) {
	t.Helper()
	if _, err := r.db.ExecContext(context.Background(),
		`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, phash)
		 VALUES (?, ?, 'fanart', 0, 1, ?)`,
		uuid.New().String(), artistID, img.HashHex(phash)); err != nil {
		t.Fatalf("seeding own fanart row: %v", err)
	}
	idx, err := r.artistService.BuildFanartIdentityIndex(context.Background())
	if err != nil {
		t.Fatalf("building identity index: %v", err)
	}
	if len(idx) == 0 {
		t.Fatalf("registry is empty; a 'no collision' assertion against it would be vacuous")
	}
}

// TestProcessAndAppendFanart_CollisionNotifiesOnlyAfterSaveSucceeds pins the
// notify-after-confirmed-save ordering on the fanart APPEND chokepoint.
//
// The durable half of a collision notification is a fixable Action Queue entry
// whose auto-fix BACKS ARTWORK OUT of the artist. Emitting it for an append that
// then failed would point a destructive remediation at a file that was never
// created. So the verdict is computed early (while the converted bytes are in
// hand) but announced only once img.Save has confirmed the write.
func TestProcessAndAppendFanart_CollisionNotifiesOnlyAfterSaveSucceeds(t *testing.T) {
	jpegBytes, phash := decodableBackdropJPEG(t)

	t.Run("save fails: no notification and no durable violation", func(t *testing.T) {
		r := testRouterForLibraryOps(t)
		seedCollidingArtist(t, r, phash)
		pub, raised := wireCollisionNotifier(r)

		// A read-only directory: still scannable (so the collision IS evaluated),
		// but img.Save cannot write into it.
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

		a := &artist.Artist{ID: "append-save-fail", Name: "Append Fails", Path: dir}
		saved, err := r.processAndAppendFanart(context.Background(), r.newImageWriteScope(a), dir, jpegBytes, nil)

		// Preconditions: the write really did fail and left nothing behind. Without
		// these the "no notification" assertions below would prove nothing.
		if err == nil {
			t.Fatalf("processAndAppendFanart returned nil error; the save was expected to fail (saved=%v)", saved)
		}
		if len(saved) != 0 {
			t.Fatalf("saved = %v, want none", saved)
		}
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			t.Fatalf("reading artist dir: %v", readErr)
		}
		if len(entries) != 0 {
			t.Fatalf("expected no files written, got %v", entries)
		}

		if len(pub.events) != 0 {
			t.Errorf("SSE collision events = %d, want 0: notified for an append that never landed", len(pub.events))
		}
		if *raised != 0 {
			t.Errorf("durable violations raised = %d, want 0: a fixable back-out entry now points at "+
				"artwork that was never written", *raised)
		}
	})

	t.Run("save succeeds: notification raised and image still appended", func(t *testing.T) {
		r := testRouterForLibraryOps(t)
		seedCollidingArtist(t, r, phash)
		pub, raised := wireCollisionNotifier(r)

		dir := t.TempDir()
		a := &artist.Artist{ID: "append-save-ok", Name: "Append Works", Path: dir}
		saved, err := r.processAndAppendFanart(context.Background(), r.newImageWriteScope(a), dir, jpegBytes, nil)
		if err != nil {
			t.Fatalf("processAndAppendFanart: %v", err)
		}

		// NOTIFY-ONLY: the collision must never have blocked the write.
		if len(saved) == 0 {
			t.Fatal("no file saved: notify-only must never skip the write")
		}
		matches, _ := filepath.Glob(filepath.Join(dir, "fanart*"))
		if len(matches) == 0 {
			t.Error("no fanart file on disk despite a reported save")
		}

		if len(pub.events) != 1 {
			t.Fatalf("SSE collision events = %d, want exactly 1", len(pub.events))
		}
		if pub.events[0].Type != event.BackdropCollision {
			t.Errorf("event type = %q, want %q", pub.events[0].Type, event.BackdropCollision)
		}
		if *raised != 1 {
			t.Errorf("durable violations raised = %d, want exactly 1", *raised)
		}
	})

	t.Run("no cross-artist collision: image appended with no notification", func(t *testing.T) {
		r := testRouterForLibraryOps(t)
		pub, raised := wireCollisionNotifier(r)

		dir := t.TempDir()
		a := &artist.Artist{ID: "append-no-collision", Name: "No Collision", Path: dir}
		// The registry holds this SAME artist's fanart, so it is populated but
		// cannot mismatch: CompareIdentity excludes the destination artist.
		seedOwnFanart(t, r, a.ID, phash)

		saved, err := r.processAndAppendFanart(context.Background(), r.newImageWriteScope(a), dir, jpegBytes, nil)
		if err != nil {
			t.Fatalf("processAndAppendFanart: %v", err)
		}
		if len(saved) == 0 {
			t.Fatal("no file saved")
		}
		if len(pub.events) != 0 || *raised != 0 {
			t.Errorf("events = %d, raised = %d, want 0 and 0: the artist's own fanart is not a cross-artist collision",
				len(pub.events), *raised)
		}
	})
}

// TestProcessAndSaveImage_CollisionNotifiesOnlyAfterSaveSucceeds pins the same
// ordering on the OVERWRITE chokepoint, and pins the fanart gate.
func TestProcessAndSaveImage_CollisionNotifiesOnlyAfterSaveSucceeds(t *testing.T) {
	jpegBytes, phash := decodableBackdropJPEG(t)

	t.Run("save fails: no notification and no durable violation", func(t *testing.T) {
		r := testRouterForLibraryOps(t)
		seedCollidingArtist(t, r, phash)
		pub, raised := wireCollisionNotifier(r)

		// A regular FILE used as the destination directory: every write into it
		// fails, while the collision check ahead of it still runs on the bytes.
		fileAsDir := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(fileAsDir, []byte("x"), 0o600); err != nil {
			t.Fatalf("writing blocker file: %v", err)
		}

		a := &artist.Artist{ID: "overwrite-save-fail", Name: "Overwrite Fails", Path: fileAsDir}
		saved, err := r.processAndSaveImage(context.Background(), r.newImageWriteScope(a), fileAsDir, "fanart", jpegBytes, nil)

		if err == nil {
			t.Fatalf("processAndSaveImage returned nil error; the save was expected to fail (saved=%v)", saved)
		}
		if len(saved) != 0 {
			t.Fatalf("saved = %v, want none", saved)
		}

		if len(pub.events) != 0 {
			t.Errorf("SSE collision events = %d, want 0: notified for a save that never landed", len(pub.events))
		}
		if *raised != 0 {
			t.Errorf("durable violations raised = %d, want 0: a fixable back-out entry now points at "+
				"artwork that was never written", *raised)
		}
	})

	t.Run("save succeeds: notification raised and image still written", func(t *testing.T) {
		r := testRouterForLibraryOps(t)
		seedCollidingArtist(t, r, phash)
		pub, raised := wireCollisionNotifier(r)

		dir := t.TempDir()
		a := &artist.Artist{ID: "overwrite-save-ok", Name: "Overwrite Works", Path: dir}
		saved, err := r.processAndSaveImage(context.Background(), r.newImageWriteScope(a), dir, "fanart", jpegBytes, nil)
		if err != nil {
			t.Fatalf("processAndSaveImage: %v", err)
		}
		if len(saved) == 0 {
			t.Fatal("no file saved: notify-only must never skip the write")
		}
		matches, _ := filepath.Glob(filepath.Join(dir, "fanart*"))
		if len(matches) == 0 {
			t.Error("no fanart file on disk despite a reported save")
		}

		if len(pub.events) != 1 {
			t.Fatalf("SSE collision events = %d, want exactly 1", len(pub.events))
		}
		if *raised != 1 {
			t.Errorf("durable violations raised = %d, want exactly 1", *raised)
		}
	})

	t.Run("non-fanart image type does not fire the check", func(t *testing.T) {
		r := testRouterForLibraryOps(t)
		// A genuine cross-artist collision IS present in the registry -- the only
		// thing stopping the notification is the fanart gate.
		seedCollidingArtist(t, r, phash)
		pub, raised := wireCollisionNotifier(r)

		dir := t.TempDir()
		a := &artist.Artist{ID: "thumb-write", Name: "Thumb Writer", Path: dir}
		scope := r.newImageWriteScope(a)

		saved, err := r.processAndSaveImage(context.Background(), scope, dir, "thumb", jpegBytes, nil)
		if err != nil {
			t.Fatalf("processAndSaveImage: %v", err)
		}
		if len(saved) == 0 {
			t.Fatal("no thumb saved")
		}

		if len(pub.events) != 0 || *raised != 0 {
			t.Errorf("events = %d, raised = %d, want 0 and 0: the registry holds fanart only, so a thumb "+
				"write must not raise a backdrop collision", len(pub.events), *raised)
		}
		// And it must not even have paid for the whole-library scan.
		if scope.builds != 0 {
			t.Errorf("identity index builds = %d, want 0: a non-fanart write must not build the registry",
				scope.builds)
		}
	})
}

// TestImageWriteScope_IdentityIndexBuiltOncePerScope pins the once-per-scope
// caching contract (design-2540.md section 4).
//
// BuildFanartIdentityIndex is a WHOLE-LIBRARY scan and deliberately does no
// caching of its own, so honoring "once per scope" is this guard's job. The
// batch fanart-append handler pushes up to 20 images through a single scope; a
// per-image build would repeat that scan for every one of them.
func TestImageWriteScope_IdentityIndexBuiltOncePerScope(t *testing.T) {
	jpegBytes, phash := decodableBackdropJPEG(t)

	r := testRouterForLibraryOps(t)
	seedCollidingArtist(t, r, phash)
	pub, raised := wireCollisionNotifier(r)

	dir := t.TempDir()
	a := &artist.Artist{ID: "batch-append", Name: "Batch Appender", Path: dir}
	scope := r.newImageWriteScope(a)
	if scope == nil {
		t.Fatal("newImageWriteScope returned nil with the seam fully wired")
	}

	const images = 4
	for i := 0; i < images; i++ {
		if _, err := r.processAndAppendFanart(context.Background(), scope, dir, jpegBytes, nil); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	if scope.builds != 1 {
		t.Errorf("identity index builds = %d across %d images, want exactly 1: the whole-library scan "+
			"is being repeated per image", scope.builds, images)
	}

	// Precondition on the above: the check really did run every time, so builds==1
	// reflects caching rather than the check being skipped.
	if len(pub.events) != images || *raised != images {
		t.Errorf("events = %d, raised = %d, want %d each: the per-image check did not run every time",
			len(pub.events), *raised, images)
	}
}
