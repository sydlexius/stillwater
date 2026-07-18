package publish

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/collision"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/event"
	img "github.com/sydlexius/stillwater/internal/image"
)

// recordingPublisher captures the SSE events the collision notifier publishes.
type recordingPublisher struct{ events []event.Event }

func (r *recordingPublisher) Publish(e event.Event) { r.events = append(r.events, e) }

// staticIdentityIndex is a FanartIdentityIndexer returning a fixed registry.
type staticIdentityIndex struct {
	entries []img.FanartIdentityEntry
	err     error
}

func (s *staticIdentityIndex) BuildFanartIdentityIndex(context.Context) ([]img.FanartIdentityEntry, error) {
	return s.entries, s.err
}

// seedDecodableJPG writes a REAL, decodable JPEG and returns its perceptual
// hash. The shared seedJPG helper writes a 4-byte SOI+EOI stub, which
// PerceptualHash cannot decode -- fine for the existence-only tests, useless
// here, since a hash of 0 would make CompareIdentity fail open and the
// collision assertions vacuous. The gradient matters too: a solid-color image
// resamples to a uniform grid and hashes to 0, which is treated as unusable.
func seedDecodableJPG(t *testing.T, dir, name string) uint64 {
	t.Helper()
	const side = 64
	m := image.NewRGBA(image.Rect(0, 0, side, side))
	for y := 0; y < side; y++ {
		for x := 0; x < side; x++ {
			// Diagonal gradient: guarantees a non-uniform, non-zero dHash.
			v := uint8((x*4 + y*2) % 256)
			m.Set(x, y, color.RGBA{R: v, G: uint8(255 - int(v)), B: uint8((x * 3) % 256), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, m, nil); err != nil {
		t.Fatalf("encoding fixture jpeg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o600); err != nil {
		t.Fatalf("seeding %s: %v", name, err)
	}
	h, err := img.PerceptualHash(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("hashing fixture %s: %v", name, err)
	}
	if h == 0 {
		t.Fatalf("fixture %s hashed to 0 (degenerate); the collision assertions would be vacuous", name)
	}
	return h
}

// TestSyncAllFanart_CollisionNotifiesAndStillUploads is the core #2540
// notify-only contract at the outbound chokepoint. A backdrop that perceptually
// matches ANOTHER artist's fanart must (a) raise the toast and the durable
// fixable violation, and (b) STILL be uploaded. Asserting only the notification
// would let a regression that skips the push pass silently, which is the exact
// behavior the maintainer ruled out.
func TestSyncAllFanart_CollisionNotifiesAndStillUploads(t *testing.T) {
	dir := t.TempDir()
	ownHash := seedDecodableJPG(t, dir, "fanart.jpg")

	// TWO platform destinations, deliberately. With a single connection the
	// per-file cross-platform notification de-dup is never exercised, and
	// boolean "did it notify?" checks would pass even if the operator got one
	// toast and one durable violation PER PLATFORM. So every case asserts EXACT
	// counts: the collision is a property of the image, so it notifies once,
	// while the upload still happens to both platforms.
	tests := []struct {
		name        string
		entries     []img.FanartIdentityEntry
		wantEvents  int
		wantRaises  int
		wantUploads int32
	}{
		{
			// The polluting case: the identical hash is registered under a
			// DIFFERENT artist -> IdentityMismatch -> notify ONCE, upload twice.
			name:        "collision notifies once across both platforms and still uploads to both",
			entries:     []img.FanartIdentityEntry{{ArtistID: "other-artist", PHash: ownHash}},
			wantEvents:  1,
			wantRaises:  1,
			wantUploads: 2,
		},
		{
			// Own-artist entries are excluded by CompareIdentity -> IdentityMatch
			// -> no notification, and both uploads are unaffected.
			name:        "own-artist entry does not notify but still uploads to both",
			entries:     []img.FanartIdentityEntry{{ArtistID: "a1", PHash: ownHash}},
			wantEvents:  0,
			wantRaises:  0,
			wantUploads: 2,
		},
		{
			// Empty registry -> IdentityIndeterminate -> fail-open: no
			// notification, both uploads unaffected.
			name:        "empty registry fails open and still uploads to both",
			entries:     nil,
			wantEvents:  0,
			wantRaises:  0,
			wantUploads: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hits := &uploadHits{}
			srv := newImageUploadServer(hits)
			defer srv.Close()

			pub := &recordingPublisher{}
			raised := 0
			notifier := collision.NewNotifier(pub,
				func(context.Context, string, string, string, string) error { raised++; return nil },
				nil, silentLogger())

			p := New(Deps{
				Logger: silentLogger(),
				// Two connected platforms, one fanart file: the same image is
				// pushed twice, so a per-(file, platform) notification would
				// double-notify while a per-file one notifies once.
				ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
					{ConnectionID: "c1", PlatformArtistID: "p1"},
					{ConnectionID: "c2", PlatformArtistID: "p2"},
				}},
				ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
					"c1": {ID: "c1", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, Status: "ok",
						Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: true}},
					"c2": {ID: "c2", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, Status: "ok",
						Emby: &connection.EmbyConfig{PlatformUserID: "u2", FeatureImageWrite: true}},
				}},
			})
			p.SetCollisionNotifier(notifier, &staticIdentityIndex{entries: tc.entries})

			warnings := p.SyncAllFanartToPlatforms(context.Background(),
				&artist.Artist{ID: "a1", Path: dir, Name: "Dest"})
			if len(warnings) != 0 {
				t.Errorf("expected 0 warnings; got %d: %v", len(warnings), warnings)
			}

			// (b) The push ALWAYS happens, to EVERY platform -- notify-only must
			// never block or skip a destination. waitForUploads only waits for
			// ">= want", so assert the exact total too: an extra upload is just
			// as wrong as a missing one.
			waitForUploads(t, hits, tc.wantUploads)
			if got := hits.uploads.Load(); got != tc.wantUploads {
				t.Errorf("uploads = %d, want exactly %d", got, tc.wantUploads)
			}

			// (a) EXACT notification counts. Booleans here would hide the
			// double-notify regression this two-platform setup exists to catch.
			if got := len(pub.events); got != tc.wantEvents {
				t.Errorf("SSE events = %d, want exactly %d (one per colliding FILE, not per platform)",
					got, tc.wantEvents)
			}
			if raised != tc.wantRaises {
				t.Errorf("durable violation raises = %d, want exactly %d (upsert is idempotent, but "+
					"re-raising per platform means redundant writes and repeated toasts)",
					raised, tc.wantRaises)
			}
			if tc.wantEvents > 0 {
				if pub.events[0].Type != event.BackdropCollision {
					t.Errorf("event type = %q, want %q", pub.events[0].Type, event.BackdropCollision)
				}
				if got := pub.events[0].Data["colliding_artist_id"]; got != "other-artist" {
					t.Errorf("colliding_artist_id = %v, want other-artist", got)
				}
			}
		})
	}
}

// TestSyncAllFanart_IndexErrorFailsOpen proves a registry build failure degrades
// to "no collision checking" rather than a blocked push.
func TestSyncAllFanart_IndexErrorFailsOpen(t *testing.T) {
	dir := t.TempDir()
	seedJPG(t, dir, "fanart.jpg")

	hits := &uploadHits{}
	srv := newImageUploadServer(hits)
	defer srv.Close()

	pub := &recordingPublisher{}
	notifier := collision.NewNotifier(pub, nil, nil, silentLogger())

	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ConnectionID: "c", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c": {ID: "c", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, Status: "ok",
				Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: true}},
		}},
	})
	p.SetCollisionNotifier(notifier, &staticIdentityIndex{err: context.DeadlineExceeded})

	p.SyncAllFanartToPlatforms(context.Background(), &artist.Artist{ID: "a1", Path: dir, Name: "Dest"})

	waitForUploads(t, hits, 1)
	if len(pub.events) != 0 {
		t.Errorf("index build failed: expected no notifications, got %d", len(pub.events))
	}
}
