package api

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
	"github.com/sydlexius/stillwater/internal/collision"
	"github.com/sydlexius/stillwater/internal/event"
	img "github.com/sydlexius/stillwater/internal/image"
)

// recordingEventPublisher captures the SSE events the collision notifier emits.
type recordingEventPublisher struct{ events []event.Event }

func (r *recordingEventPublisher) Publish(e event.Event) { r.events = append(r.events, e) }

// decodableBackdropJPEG returns a REAL, decodable JPEG plus its perceptual hash.
// The gradient matters: a solid-color image resamples to a uniform grid and
// hashes to 0, which CompareIdentity treats as unusable -- so a flat fixture
// would make every collision assertion below pass vacuously.
func decodableBackdropJPEG(t *testing.T) ([]byte, uint64) {
	t.Helper()
	const side = 64
	m := image.NewRGBA(image.Rect(0, 0, side, side))
	for y := 0; y < side; y++ {
		for x := 0; x < side; x++ {
			v := uint8((x*4 + y*2) % 256)
			m.Set(x, y, color.RGBA{R: v, G: uint8(255 - int(v)), B: uint8((x * 3) % 256), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, m, nil); err != nil {
		t.Fatalf("encoding fixture jpeg: %v", err)
	}
	h, err := img.PerceptualHash(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("hashing fixture: %v", err)
	}
	if h == 0 {
		t.Fatalf("fixture hashed to 0 (degenerate); collision assertions would be vacuous")
	}
	return buf.Bytes(), h
}

// seedCollidingArtist inserts ANOTHER artist holding a fanart slot with the
// given phash, so BuildFanartIdentityIndex returns a cross-artist entry that the
// incoming backdrop will collide with.
func seedCollidingArtist(t *testing.T, r *Router, phash uint64) {
	t.Helper()
	ctx := context.Background()
	other := &artist.Artist{Name: "Polluting Artist", SortName: "Polluting Artist"}
	if err := r.artistService.Create(ctx, other); err != nil {
		t.Fatalf("creating colliding artist: %v", err)
	}
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, phash)
		 VALUES (?, ?, 'fanart', 0, 1, ?)`,
		uuid.New().String(), other.ID, img.HashHex(phash)); err != nil {
		t.Fatalf("seeding colliding fanart row: %v", err)
	}

	// The registry must actually contain the planted entry, or the "collision"
	// never happens and both branches of this test degrade to fail-open.
	idx, err := r.artistService.BuildFanartIdentityIndex(ctx)
	if err != nil {
		t.Fatalf("building identity index: %v", err)
	}
	found := false
	for _, e := range idx {
		if e.ArtistID == other.ID && e.PHash == phash {
			found = true
		}
	}
	if !found {
		t.Fatalf("planted colliding entry not in the registry (%d entries); the collision would never fire", len(idx))
	}
}

// wireCollisionNotifier attaches a recording notifier to the router and returns
// the captured-event sink plus a pointer to the durable-raise counter.
func wireCollisionNotifier(r *Router) (*recordingEventPublisher, *int) {
	pub := &recordingEventPublisher{}
	raised := 0
	r.collisionNotifier = collision.NewNotifier(pub,
		func(context.Context, string, string, string, string) error { raised++; return nil },
		nil, r.logger)
	return pub, &raised
}

// TestDownloadBackdrop_CollisionNotifiesOnlyAfterSaveSucceeds pins the ordering
// the notification depends on.
//
// The durable half of a collision notification is a FIXABLE Action Queue entry
// whose auto-fix BACKS ARTWORK OUT of the artist. Raising it for an import that
// then failed to write would aim a destructive remediation at a file that does
// not exist, and leave the operator a Fix button acting on nothing. So the
// notification must be emitted only once the save is confirmed -- never merely
// because a collision was detected on bytes we were about to write.
func TestDownloadBackdrop_CollisionNotifiesOnlyAfterSaveSucceeds(t *testing.T) {
	jpegBytes, phash := decodableBackdropJPEG(t)

	t.Run("save fails: no notification and no durable violation", func(t *testing.T) {
		r := testRouterForLibraryOps(t)
		seedCollidingArtist(t, r, phash)
		pub, raised := wireCollisionNotifier(r)

		dl := &mockImageDownloader{
			backdropFn: func(context.Context, string, int) ([]byte, string, error) {
				return jpegBytes, "image/jpeg", nil
			},
		}

		// Make the save fail: the artist's directory is replaced by a read-only
		// one, so saveFanartSlotProtected cannot write the slot. The collision is
		// still detected (same bytes, same registry) -- only the write fails.
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

		a := &artist.Artist{ID: "artist-save-fail", Name: "Save Fails", Path: dir}
		var result populateResult
		r.downloadPlatformImages(context.Background(), dl, "platform-1", nil, []string{"tag1"}, a, "emby", &result)

		// Precondition: the write really did fail, or this proves nothing.
		if result.Images != 0 {
			t.Fatalf("images = %d, want 0: the save was expected to fail", result.Images)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading artist dir: %v", err)
		}
		if len(entries) != 0 {
			t.Fatalf("expected no files written, got %v", entries)
		}

		// The assertions that matter: nothing was imported, so nothing may be
		// reported -- least of all a fixable entry whose fix deletes artwork.
		if len(pub.events) != 0 {
			t.Errorf("SSE collision events = %d, want 0: notified for an import that never landed",
				len(pub.events))
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

		dl := &mockImageDownloader{
			backdropFn: func(context.Context, string, int) ([]byte, string, error) {
				return jpegBytes, "image/jpeg", nil
			},
		}

		dir := t.TempDir()
		a := &artist.Artist{ID: "artist-save-ok", Name: "Save Works", Path: dir}
		var result populateResult
		r.downloadPlatformImages(context.Background(), dl, "platform-2", nil, []string{"tag1"}, a, "emby", &result)

		// NOTIFY-ONLY: the collision must not have blocked the import.
		if result.Images != 1 {
			t.Fatalf("images = %d, want 1: the collision blocked the import", result.Images)
		}
		matches, _ := filepath.Glob(filepath.Join(dir, "fanart*"))
		if len(matches) == 0 {
			t.Error("no fanart file on disk: notify-only must never skip the write")
		}

		// And the notification fired, naming the colliding artist.
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
}
