package publish

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/publish/publishtest"
)

// durabilityHarness wires a Publisher onto REAL SQLite-backed artist and
// connection services, one artist whose image directory holds local, mapped
// to one connection of connType served by peer.
func durabilityHarness(t *testing.T, connType string, peer *publishtest.Peer, local [][]byte) (*Publisher, *artist.Artist) {
	t.Helper()
	srv := httptest.NewServer(peer)
	t.Cleanup(srv.Close)
	return durabilityPublisher(t, connType, srv.URL, "k", "u1", "p1", local)
}

// durabilityPublisher is the peer-agnostic half of durabilityHarness, shared
// with the live integration test: real SQLite services, one image-write
// enabled connection at url, one artist mapped to platformArtistID.
func durabilityPublisher(t *testing.T, connType, url, apiKey, userID, platformArtistID string, local [][]byte) (*Publisher, *artist.Artist) {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(filepath.Join(t.TempDir(), "sw.db"))
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}
	artistSvc := artist.NewService(db)
	connSvc := connection.NewService(db, enc)

	conn := &connection.Connection{Name: "peer", Type: connType, URL: url, APIKey: apiKey, Enabled: true, Status: "ok"}
	if connType == connection.TypeEmby {
		conn.Emby = &connection.EmbyConfig{PlatformUserID: userID, FeatureImageWrite: true}
	} else {
		conn.Jellyfin = &connection.JellyfinConfig{PlatformUserID: userID, FeatureImageWrite: true}
	}
	if err := connSvc.Create(ctx, conn); err != nil {
		t.Fatalf("creating connection: %v", err)
	}

	dir := t.TempDir()
	names := []string{"fanart.jpg", "fanart2.jpg", "fanart3.jpg", "fanart4.jpg", "fanart5.jpg"}
	for i, b := range local {
		if err := os.WriteFile(filepath.Join(dir, names[i]), b, 0o600); err != nil {
			t.Fatalf("writing local fanart: %v", err)
		}
	}
	a := &artist.Artist{Name: "Durability Artist", SortName: "Durability Artist", Path: dir}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if err := artistSvc.SetPlatformID(ctx, a.ID, conn.ID, platformArtistID); err != nil {
		t.Fatalf("mapping artist: %v", err)
	}

	p := New(Deps{
		Logger:            silentLogger(),
		ArtistService:     artistSvc,
		ArtistLister:      artistSvc,
		ArtistGetter:      artistSvc,
		ConnectionService: connSvc,
		ImageWriteGate:    allowGate{},
	})
	return p, a
}

// assertPeerHolds compares the peer's backdrop list to want, slot by slot.
func assertPeerHolds(t *testing.T, step string, got, want [][]byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: platform holds %d backdrops, want %d", step, len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("%s: backdrop %d does not hold the expected image", step, i)
		}
	}
}

// TestPruneThenReconcile_DriveTheRealLoop is the #3144 regression: it drives
// prune -> reconciler pass -> reconciler pass against a peer that models its
// backdrop state, on both peer shapes, and asserts the platform SET after
// every step.
//
// The cases separate the two things the reconciler must do at once:
//
//   - "local duplicates": the local set holds a byte-identical copy (A,B,A).
//     The prune can only leave one A on the platform, so the platform ends
//     BELOW the local FILE count while holding every distinct local image.
//     Before the fix the reconciler read that as a deficit and re-pushed,
//     restoring the copy the prune removed. This case fails without the fix.
//   - "deficit revealed by the prune": the platform genuinely lacks local
//     images. The reconciler's #1869 purpose is to repair exactly that, and it
//     must still do so even when the local set itself carries a duplicate.
//   - "distinct local set": the common production shape (no local duplicates),
//     a control showing the prune holds there.
func TestPruneThenReconcile_DriveTheRealLoop(t *testing.T) {
	A, B, C := bandJPEG(t, 31), bandJPEG(t, 32), bandJPEG(t, 33)
	cases := []struct {
		name        string
		local       [][]byte
		seed        [][]byte
		wantRemoved int
		afterPrune  [][]byte
		afterPass1  [][]byte // pass 2 must leave this unchanged, with no write
	}{
		{"distinct local set", [][]byte{A, B, C}, [][]byte{A, B, C, A, B}, 2, [][]byte{A, B, C}, [][]byte{A, B, C}},
		{"local duplicates", [][]byte{A, B, A}, [][]byte{A, B, A}, 1, [][]byte{A, B}, [][]byte{A, B}},
		{"deficit revealed by the prune", [][]byte{A, B, A, C}, [][]byte{A, A}, 1, [][]byte{A}, [][]byte{A, B, A, C}},
	}
	for _, typ := range []string{connection.TypeEmby, connection.TypeJellyfin} {
		for _, tc := range cases {
			t.Run(typ+"/"+tc.name, func(t *testing.T) {
				ctx := context.Background()
				peer := publishtest.NewPeer(typ, tc.seed)
				p, a := durabilityHarness(t, typ, peer, tc.local)

				res, err := p.PrunePlatformBackdropDuplicates(ctx, PlatformBackdropPruneScope{ArtistID: a.ID})
				if err != nil {
					t.Fatalf("prune: %v", err)
				}
				// PRECONDITION: the prune really removed copies, so the passes
				// below are measuring durability rather than a no-op prune.
				if res.BackdropsRemoved != tc.wantRemoved || len(res.Failures) != 0 {
					t.Fatalf("precondition: prune removed %d (failures %v), want %d", res.BackdropsRemoved, res.Failures, tc.wantRemoved)
				}
				got, _ := peer.State()
				assertPeerHolds(t, "after prune", got, tc.afterPrune)

				p.ReconcileArtworkToPlatforms(ctx)
				got, writes1 := peer.State()
				assertPeerHolds(t, "after reconciler pass 1", got, tc.afterPass1)

				p.ReconcileArtworkToPlatforms(ctx)
				got, writes2 := peer.State()
				assertPeerHolds(t, "after reconciler pass 2", got, tc.afterPass1)
				if writes2 != writes1 {
					t.Errorf("reconciler pass 2 issued %d platform writes, want 0: a converged platform must not be re-pushed", writes2-writes1)
				}
			})
		}
	}
}

// TestLocalFanartHashes_UnreadableCountsAsDistinct pins the fail-open
// direction: a file that cannot be hashed cannot be proven a duplicate, so it
// counts as distinct and keeps the reconciler's repair armed.
func TestLocalFanartHashes_UnreadableCountsAsDistinct(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "fanart.jpg"), filepath.Join(dir, "fanart2.jpg")
	writeFile(t, a, bandJPEG(t, 41))
	writeFile(t, b, bandJPEG(t, 41))
	missing := filepath.Join(dir, "fanart3.jpg")
	p := New(Deps{Logger: silentLogger()})
	distinct, unreadable := p.localFanartHashes(context.Background(), []string{a, b, missing})
	if len(distinct) != 1 || unreadable != 1 {
		t.Errorf("localFanartHashes = %d distinct + %d unreadable, want 1 + 1 (one distinct image plus one unreadable file)", len(distinct), unreadable)
	}
}
