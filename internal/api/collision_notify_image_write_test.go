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
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/event"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/platform"
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

// breakFanartRegistry hides the table BuildFanartIdentityIndex reads, so the
// NEXT index build fails; restoreFanartRegistry puts it back.
//
// Reversibility is the point: it lets these tests prove the once-per-scope
// caching contract BEHAVIORALLY instead of by reading a counter on the
// production object. "Cached" and "rebuilt" then produce OPPOSITE outcomes --
// broken after a good build, a reused index still collides while a rebuilt one
// cannot; repaired after a FAILED build, a cached failure still finds nothing
// while a retried build suddenly does.
func breakFanartRegistry(t *testing.T, r *Router) {
	t.Helper()
	if _, err := r.db.ExecContext(context.Background(),
		`ALTER TABLE artist_images RENAME TO artist_images_hidden`); err != nil {
		t.Fatalf("hiding artist_images: %v", err)
	}
}

func restoreFanartRegistry(t *testing.T, r *Router) {
	t.Helper()
	if _, err := r.db.ExecContext(context.Background(),
		`ALTER TABLE artist_images_hidden RENAME TO artist_images`); err != nil {
		t.Fatalf("restoring artist_images: %v", err)
	}
}

// unwritableFanartName activates a platform profile whose fanart primary name is
// a LEGAL but maximum-length filename, so the write itself is the first thing
// that fails while the destination directory stays perfectly ordinary.
//
// Placement is the whole point. Both chokepoints do real work between computing
// the collision verdict and saving -- the append path scans the directory for the
// next free fanart index, the overwrite path backs the existing slot up -- and a
// fault planted on the DIRECTORY (a destination under a regular file, say) fires
// at one of those instead, which guards a step this test does not claim to guard.
//
// A 254-byte name clears every one of them: MaxFanartIndex just reads the
// directory, and BackupSlot's widest probe is the ".jpeg" variant at 255 bytes,
// exactly POSIX NAME_MAX. img.Save then appends a uniquifying ".<random>.tmp" to
// build its atomic temp file, overshoots NAME_MAX, and fails at the write.
//
// Structural rather than a permission check, so it holds whatever uid the suite
// runs as -- unlike chmod, which a root container silently ignores.
func unwritableFanartName(t *testing.T, r *Router) {
	t.Helper()
	p := &platform.Profile{
		Name:        "Unwritable Fanart Name",
		ImageNaming: platform.ImageNaming{Fanart: []string{strings.Repeat("f", 250) + ".jpg"}},
	}
	if err := r.platformService.Create(context.Background(), p); err != nil {
		t.Fatalf("creating platform profile: %v", err)
	}
	if err := r.platformService.SetActive(context.Background(), p.ID); err != nil {
		t.Fatalf("activating platform profile: %v", err)
	}
}

// assertFailedAtSave pins WHICH step failed, not merely that something did.
//
// Every "no notification after a failed write" assertion below is satisfied just
// as well by a fault that fires BEFORE the save is ever attempted -- err is
// non-nil, the directory is empty, nothing notified -- so without this check the
// test silently degrades into guarding an earlier step while still claiming to
// guard img.Save. That is not hypothetical: this test previously blocked the
// destination directory, which tripped the fanart index scan, and it read as
// passing for a full review round.
//
// upstreamMarkers name the wrapping text of the steps that must NOT have failed.
func assertFailedAtSave(t *testing.T, err error, upstreamMarkers ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("no error; the save was expected to fail")
	}
	msg := err.Error()
	// img.Save wraps its write as "writing <path>: ..." and WriteFileAtomic's
	// temp-file step as "creating temp file: ...".
	if !strings.Contains(msg, "writing ") || !strings.Contains(msg, "creating temp file") {
		t.Fatalf("error %q is not img.Save's write failure; the injected fault fired somewhere else", msg)
	}
	for _, marker := range upstreamMarkers {
		if strings.Contains(msg, marker) {
			t.Fatalf("error %q came from the %q step, which runs BEFORE img.Save; this test must fail AT the save", msg, marker)
		}
	}
}

// assertNothingOnDisk fails unless dir is an existing, empty directory: the
// failed write left no file, and no half-written temp file either.
func assertNothingOnDisk(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading destination directory: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("destination holds %v, want nothing: the save was supposed to leave no artwork behind", names)
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

		// An ORDINARY destination directory with a fanart name img.Save cannot write
		// atomically. The directory has to stay valid: the append path scans it for
		// the next free fanart index first, and a broken directory would fail THERE,
		// leaving the save untested. See unwritableFanartName.
		dir := t.TempDir()
		unwritableFanartName(t, r)

		a := &artist.Artist{ID: "append-save-fail", Name: "Append Fails", Path: dir}

		// Precondition: a genuine cross-artist collision really is reachable for
		// these bytes, so "no notification" below reflects the failed write and not
		// an absent verdict. Probed on a THROWAWAY scope so the scope under test
		// starts cold.
		if v := r.newImageWriteScope(a).collisionVerdict(context.Background(), jpegBytes); v == nil {
			t.Fatal("no collision verdict for these bytes; the assertions below would pass for the wrong reason")
		}

		saved, err := r.processAndAppendFanart(context.Background(), r.newImageWriteScope(a), dir, jpegBytes, nil)

		// Preconditions: the write really did fail, it failed AT img.Save rather
		// than at the index scan ahead of it, and it left nothing behind. Without
		// these the "no notification" assertions below would prove nothing.
		if err == nil {
			t.Fatalf("processAndAppendFanart returned nil error; the save was expected to fail (saved=%v)", saved)
		}
		if len(saved) != 0 {
			t.Fatalf("saved = %v, want none", saved)
		}
		assertFailedAtSave(t, err, "scanning fanart")
		assertNothingOnDisk(t, dir)

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

		// Same fault as the append case, and for the same reason: this path takes a
		// BACKUP of the existing slot before it writes, so a broken destination
		// directory fails at the backup and never reaches img.Save at all.
		dir := t.TempDir()
		unwritableFanartName(t, r)

		a := &artist.Artist{ID: "overwrite-save-fail", Name: "Overwrite Fails", Path: dir}
		saved, err := r.processAndSaveImage(context.Background(), r.newImageWriteScope(a), dir, "fanart", jpegBytes, nil)

		if err == nil {
			t.Fatalf("processAndSaveImage returned nil error; the save was expected to fail (saved=%v)", saved)
		}
		if len(saved) != 0 {
			t.Fatalf("saved = %v, want none", saved)
		}
		assertFailedAtSave(t, err, "backing up")
		assertNothingOnDisk(t, dir)

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

		// And it must not even have paid for the whole-library scan. Proven
		// observably: break the registry AFTER the thumb write, then push FANART
		// through the SAME scope. A scope that never built has to build now, and
		// that build fails, so nothing collides. Had the thumb write cached the
		// colliding index, it would be reused here and WOULD notify.
		if scope.built {
			t.Error("scope reports an index build after a non-fanart write")
		}
		breakFanartRegistry(t, r)

		fanartSaved, err := r.processAndAppendFanart(context.Background(), scope, dir, jpegBytes, nil)
		if err != nil {
			t.Fatalf("processAndAppendFanart: %v", err)
		}
		if len(fanartSaved) == 0 {
			t.Fatal("no fanart saved: fail-open must never cost the write")
		}
		if len(pub.events) != 0 || *raised != 0 {
			t.Errorf("events = %d, raised = %d, want 0 and 0: the thumb write built and cached the registry, "+
				"which a non-fanart write must never do", len(pub.events), *raised)
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
		// scan of artist_images, so hiding that table is the transient-DB-failure case
		// in miniature.
		breakFanartRegistry(t, r)

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
		if !scope.built {
			t.Error("scope reports no index build; the build must have been attempted")
		}
		if len(pub.events) != 0 {
			t.Errorf("SSE collision events = %d, want 0: a verdict was announced without a registry to reach it",
				len(pub.events))
		}
		if *raised != 0 {
			t.Errorf("durable violations raised = %d, want 0: a back-out fix was armed on a check that never ran",
				*raised)
		}

		// And that FAILED build is cached, not retried once per image. Proven
		// observably: repair the registry, then push a second image through the SAME
		// scope. A cached failure keeps yielding no verdict; a per-image retry would
		// now succeed and surface the seeded cross-artist collision.
		restoreFanartRegistry(t, r)

		second, err := r.processAndAppendFanart(context.Background(), scope, dir, jpegBytes, nil)
		if err != nil {
			t.Fatalf("second append: %v", err)
		}
		if len(second) == 0 {
			t.Fatal("no file saved on the second append")
		}
		if len(pub.events) != 0 || *raised != 0 {
			t.Errorf("events = %d, raised = %d, want 0 and 0: the failed index build was retried per image "+
				"instead of being cached for the scope", len(pub.events), *raised)
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
//
// "Once" has two halves and they fail differently. The index must be REUSED
// across the images that follow the first, and it must be built AT MOST ONCE
// while serving any single one of them. A guard that caches correctly but builds
// twice on the way there still doubles the scan the contract exists to prevent,
// so both halves are pinned below.
func TestImageWriteScope_IdentityIndexBuiltOncePerScope(t *testing.T) {
	jpegBytes, phash := decodableBackdropJPEG(t)

	t.Run("the index is reused for every later image in the scope", func(t *testing.T) {
		testIdentityIndexReusedAcrossImages(t, jpegBytes, phash)
	})

	// The reuse case above cannot see a scope that builds the index more than once
	// while serving ONE image: both builds would succeed, the second would overwrite
	// the first with an identical result, and every count would still line up.
	//
	// A build that FAILS does announce itself -- identityIndex warns once per
	// attempt, an operator-facing log line rather than a counter bolted onto the
	// production object -- so breaking the registry turns build attempts into
	// something observable and counts them.
	t.Run("the index is built at most once while serving a single image", func(t *testing.T) {
		r := testRouterForLibraryOps(t)
		seedCollidingArtist(t, r, phash)
		pub, raised := wireCollisionNotifier(r)

		var logs bytes.Buffer
		r.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

		breakFanartRegistry(t, r)

		dir := t.TempDir()
		a := &artist.Artist{ID: "single-image-build", Name: "Single Image Build", Path: dir}
		scope := r.newImageWriteScope(a)

		saved, err := r.processAndAppendFanart(context.Background(), scope, dir, jpegBytes, nil)
		if err != nil {
			t.Fatalf("processAndAppendFanart: %v", err)
		}
		if len(saved) == 0 {
			t.Fatal("no file saved: a failed collision check must never cost the operator their write")
		}

		// Precondition: the scope did attempt a build, so a count of 1 below means
		// "built exactly once" and not "the warning never reached the buffer".
		if !scope.built {
			t.Fatal("scope reports no index build; this count would be meaningless")
		}
		// Matched on the clause unique to the LOG MESSAGE. The bare
		// "building fanart identity index" prefix also appears in the wrapped error
		// this same line carries, so counting that would count every attempt twice.
		const buildAttempt = "skipping cross-artist collision check for this write"
		if n := strings.Count(logs.String(), buildAttempt); n != 1 {
			t.Errorf("whole-library index builds while writing one image = %d, want exactly 1: the scan is "+
				"being repeated within a single write instead of built once and cached", n)
		}
		if len(pub.events) != 0 || *raised != 0 {
			t.Errorf("events = %d, raised = %d, want 0 and 0: a verdict was announced with no registry to reach it",
				len(pub.events), *raised)
		}
	})
}

func testIdentityIndexReusedAcrossImages(t *testing.T, jpegBytes []byte, phash uint64) {
	t.Helper()

	r := testRouterForLibraryOps(t)
	seedCollidingArtist(t, r, phash)
	pub, raised := wireCollisionNotifier(r)

	dir := t.TempDir()
	a := &artist.Artist{ID: "batch-append", Name: "Batch Appender", Path: dir}
	scope := r.newImageWriteScope(a)
	if scope == nil {
		t.Fatal("newImageWriteScope returned nil with the seam fully wired")
	}

	// Image 1 builds the index and finds the seeded cross-artist collision.
	if _, err := r.processAndAppendFanart(context.Background(), scope, dir, jpegBytes, nil); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if len(pub.events) != 1 || *raised != 1 {
		t.Fatalf("events = %d, raised = %d after the first image, want 1 each: the check did not run",
			len(pub.events), *raised)
	}

	// Now make any REBUILD fail. From here on a scope that reuses its cached index
	// keeps colliding, while one that rebuilds per image gets a nil index and
	// silently stops -- opposite outcomes, which is what makes this a test of
	// reuse rather than an inspection of a counter.
	breakFanartRegistry(t, r)

	const images = 4
	for i := 1; i < images; i++ {
		if _, err := r.processAndAppendFanart(context.Background(), scope, dir, jpegBytes, nil); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	// Every later image still collided, so every one of them saw the index built
	// for image 1. A per-image build would have yielded nil here and left the
	// counts pinned at 1.
	if len(pub.events) != images || *raised != images {
		t.Errorf("events = %d, raised = %d, want %d each: images after the first saw no registry, so the "+
			"whole-library scan is being repeated per image instead of cached for the scope",
			len(pub.events), *raised, images)
	}
}
