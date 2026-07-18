package api

// #2565 -- the two OPERATOR image-write chokepoints wired into #2540's
// cross-artist backdrop collision seam.
//
// These tests share the fixtures #2613 built for the platform-import path
// (collision_notify_populate_test.go): decodableBackdropJPEG, seedCollidingArtist
// and wireCollisionNotifier. The behavior under test is the same contract at two
// more call sites, so it is deliberately pinned the same way.

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
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

// flatBackdropJPEG returns a REAL, decodable but SOLID-COLOR JPEG. A uniform
// image resamples to a uniform 9x8 grid, so every dHash column comparison is a
// tie and PerceptualHash returns 0 -- the "hashed but unusable" case.
func flatBackdropJPEG(t *testing.T) []byte {
	t.Helper()
	m := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			m.Set(x, y, color.RGBA{R: 128, G: 128, B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, m, nil); err != nil {
		t.Fatalf("encoding flat fixture jpeg: %v", err)
	}
	h, err := img.PerceptualHash(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("hashing flat fixture: %v", err)
	}
	if h != 0 {
		t.Fatalf("flat fixture hashed to %#x, want 0; this test needs the degenerate case", h)
	}
	return buf.Bytes()
}

// TestImageWriteScope_CheckFailureNeverBlocksTheWrite pins the FAIL-OPEN posture
// of the #2540 collision check at the #2565 operator chokepoints.
//
// The check is a notify-only advisory bolted onto a write the operator asked
// for. Every way it can fail to reach a verdict -- an unbuildable registry, an
// empty one, an image it cannot hash, an image whose hash is unusable -- must
// cost the operator NOTHING. The write lands, and no notification is invented
// from a verdict that was never actually computed.
//
// Each case asserts the observable outcome on all three surfaces: the file is on
// disk, no SSE event was published, and no durable Action Queue violation was
// raised. A false violation is the expensive half: its auto-fix BACKS ARTWORK
// OUT, so a check that guesses "collision" when it could not decide would aim a
// destructive remediation at a legitimate image.
func TestImageWriteScope_CheckFailureNeverBlocksTheWrite(t *testing.T) {
	jpegBytes, phash := decodableBackdropJPEG(t)

	t.Run("identity index build fails: image still written, no notification", func(t *testing.T) {
		r := testRouterForLibraryOps(t)
		// A genuine cross-artist collision IS present, so the ONLY thing suppressing
		// the notification below is the failed index build.
		seedCollidingArtist(t, r, phash)
		pub, raised := wireCollisionNotifier(r)

		dir := t.TempDir()
		a := &artist.Artist{ID: "index-build-fails", Name: "Index Build Fails", Path: dir}
		scope := r.newImageWriteScope(a)

		// Break the registry at its source: BuildFanartIdentityIndex is a whole-library
		// DB scan, so a closed handle is the transient-DB-failure case in miniature.
		if err := r.db.Close(); err != nil {
			t.Fatalf("closing db: %v", err)
		}

		saved, err := r.processAndAppendFanart(context.Background(), scope, dir, jpegBytes, nil)
		if err != nil {
			t.Fatalf("processAndAppendFanart: a failed collision check must not fail the write: %v", err)
		}
		if len(saved) == 0 {
			t.Fatal("no file saved: a failed collision check must never cost the operator their write")
		}
		matches, _ := filepath.Glob(filepath.Join(dir, "fanart*"))
		if len(matches) == 0 {
			t.Error("no fanart file on disk despite a reported save")
		}

		// Precondition: the build was ATTEMPTED (and failed) rather than skipped.
		if scope.builds != 1 {
			t.Errorf("identity index builds = %d, want 1: the build must have been attempted", scope.builds)
		}
		if len(pub.events) != 0 {
			t.Errorf("SSE collision events = %d, want 0: a verdict was announced without a registry to reach it",
				len(pub.events))
		}
		if *raised != 0 {
			t.Errorf("durable violations raised = %d, want 0: a back-out fix was armed on a check that never ran",
				*raised)
		}
	})

	t.Run("empty registry: image still written, no notification", func(t *testing.T) {
		r := testRouterForLibraryOps(t)
		pub, raised := wireCollisionNotifier(r)

		dir := t.TempDir()
		a := &artist.Artist{ID: "empty-registry", Name: "Empty Registry", Path: dir}
		scope := r.newImageWriteScope(a)

		// Precondition: the library genuinely holds no fanart at all, so the registry
		// really is empty rather than merely unbuilt.
		idx, err := r.artistService.BuildFanartIdentityIndex(context.Background())
		if err != nil {
			t.Fatalf("building identity index: %v", err)
		}
		if len(idx) != 0 {
			t.Fatalf("registry has %d entries, want 0: this case needs an empty library", len(idx))
		}

		saved, err := r.processAndAppendFanart(context.Background(), scope, dir, jpegBytes, nil)
		if err != nil {
			t.Fatalf("processAndAppendFanart: %v", err)
		}
		if len(saved) == 0 {
			t.Fatal("no file saved: the first backdrop in an empty library must still be written")
		}
		if len(pub.events) != 0 || *raised != 0 {
			t.Errorf("events = %d, raised = %d, want 0 and 0: nothing to collide with in an empty registry",
				len(pub.events), *raised)
		}
	})

	t.Run("unhashable image: image still written, no notification", func(t *testing.T) {
		r := testRouterForLibraryOps(t)
		seedCollidingArtist(t, r, phash)
		pub, raised := wireCollisionNotifier(r)

		// A TRUNCATED JPEG. ConvertFormat passes non-WebP bytes through on the
		// strength of the magic number alone (internal/image/processor.go), so a
		// corrupt-but-well-headed upload reaches the hasher intact and fails to
		// decode there. This is the operator uploading a partial file, not a
		// synthetic fault.
		truncated := jpegBytes[:len(jpegBytes)/2]
		if _, err := img.PerceptualHash(bytes.NewReader(truncated)); err == nil {
			t.Fatal("truncated fixture still hashes; this case needs an undecodable image")
		}

		dir := t.TempDir()
		a := &artist.Artist{ID: "unhashable", Name: "Unhashable", Path: dir}
		saved, err := r.processAndAppendFanart(context.Background(), r.newImageWriteScope(a), dir, truncated, nil)
		if err != nil {
			t.Fatalf("processAndAppendFanart: an unhashable image must not fail the write: %v", err)
		}
		if len(saved) == 0 {
			t.Fatal("no file saved: an unhashable image must still be written")
		}
		matches, _ := filepath.Glob(filepath.Join(dir, "fanart*"))
		if len(matches) == 0 {
			t.Error("no fanart file on disk despite a reported save")
		}

		if len(pub.events) != 0 || *raised != 0 {
			t.Errorf("events = %d, raised = %d, want 0 and 0: a collision was reported on bytes that were never hashed",
				len(pub.events), *raised)
		}
	})

	t.Run("degenerate zero hash: image still written, no notification", func(t *testing.T) {
		r := testRouterForLibraryOps(t)
		pub, raised := wireCollisionNotifier(r)

		// A flat image hashes to 0, which is indistinguishable from "never hashed"
		// and so cannot be compared against anything. The registry is populated with
		// a REAL cross-artist hash here, so the write below is the one case where a
		// populated registry still yields no verdict -- the candidate, not the
		// reference, is the unusable side.
		//
		// Two layers enforce this: collisionVerdict's own zero check and
		// CompareIdentity's (internal/image/identity.go). Belt and braces on purpose,
		// since admitting a zero hash scores a PERFECT similarity against any other
		// zero and would manufacture collisions between unrelated unhashable images.
		// What is pinned here is the OUTCOME, which must hold whichever layer catches
		// it: a flat backdrop is a legitimate image and gets written, silently.
		seedCollidingArtist(t, r, phash)

		flat := flatBackdropJPEG(t)
		dir := t.TempDir()
		a := &artist.Artist{ID: "zero-hash", Name: "Zero Hash", Path: dir}
		scope := r.newImageWriteScope(a)

		saved, err := r.processAndAppendFanart(context.Background(), scope, dir, flat, nil)
		if err != nil {
			t.Fatalf("processAndAppendFanart: %v", err)
		}
		if len(saved) == 0 {
			t.Fatal("no file saved: a flat image is still a legitimate backdrop")
		}

		// Precondition: the registry was built and IS populated, so "no collision"
		// reflects the zero-hash guard rather than an empty index.
		if scope.builds != 1 {
			t.Errorf("identity index builds = %d, want 1", scope.builds)
		}
		if len(scope.idx) == 0 {
			t.Fatal("registry is empty; this assertion would pass for the wrong reason")
		}

		if len(pub.events) != 0 || *raised != 0 {
			t.Errorf("events = %d, raised = %d, want 0 and 0: two unhashable images were matched to each other",
				len(pub.events), *raised)
		}
	})
}

// TestImageWriteScope_NilScopeIsASafeNoOp pins the documented nil-tolerance
// contract on imageWriteScope: newImageWriteScope returns nil whenever the seam
// is not wired, and callers hand that nil straight through without a check of
// their own. Every method has to absorb it.
//
// This is a live contract, not defensive padding: handlers_connection_library.go
// passes a literal nil on the named-image path, and newImageWriteScope returns
// nil for any router without a collision notifier.
func TestImageWriteScope_NilScopeIsASafeNoOp(t *testing.T) {
	ctx := context.Background()
	jpegBytes, _ := decodableBackdropJPEG(t)

	t.Run("newImageWriteScope yields nil when the seam is unwired", func(t *testing.T) {
		r := testRouterForLibraryOps(t)
		// Deliberately NOT calling wireCollisionNotifier: no notifier, no scope.
		if scope := r.newImageWriteScope(&artist.Artist{ID: "x", Name: "X"}); scope != nil {
			t.Errorf("newImageWriteScope = %v, want nil with no collision notifier wired", scope)
		}
		if scope := r.newImageWriteScope(nil); scope != nil {
			t.Errorf("newImageWriteScope(nil artist) = %v, want nil", scope)
		}
	})

	t.Run("every method absorbs a nil scope", func(t *testing.T) {
		var scope *imageWriteScope

		if idx := scope.identityIndex(ctx); idx != nil {
			t.Errorf("identityIndex on a nil scope = %v, want nil", idx)
		}
		if res := scope.collisionVerdict(ctx, jpegBytes); res != nil {
			t.Errorf("collisionVerdict on a nil scope = %v, want nil", res)
		}
		// notifyCollision must swallow both a nil scope and a nil verdict rather than
		// dereference either; a panic here would take down the operator's write.
		scope.notifyCollision(ctx, nil)
	})

	t.Run("a nil scope still writes the image", func(t *testing.T) {
		r := testRouterForLibraryOps(t)
		pub, raised := wireCollisionNotifier(r)

		dir := t.TempDir()
		saved, err := r.processAndAppendFanart(ctx, nil, dir, jpegBytes, nil)
		if err != nil {
			t.Fatalf("processAndAppendFanart with a nil scope: %v", err)
		}
		if len(saved) == 0 {
			t.Fatal("no file saved: a nil scope disables the check, never the write")
		}
		if len(pub.events) != 0 || *raised != 0 {
			t.Errorf("events = %d, raised = %d, want 0 and 0: a nil scope must not notify",
				len(pub.events), *raised)
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
