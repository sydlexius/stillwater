package publish

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection"
)

// errSimulatedCrash stands in for the process dying inside a resync (#3147).
var errSimulatedCrash = errors.New("simulated crash between the resync's delete and upload loops")

// crashingResyncClient wraps the REAL resync client and lets the first
// uploadsLanded uploads through, then fails every later one. With every
// delete already issued, that leaves the platform holding exactly the first
// uploadsLanded local images: the state a process death mid-resync leaves.
// Wrapping the real client (not a fake) keeps the deletes and the landed
// uploads on the production wire path.
type crashingResyncClient struct {
	fanartResyncClient
	uploadsLanded int
	uploads       int
}

func (c *crashingResyncClient) UploadImageAtIndex(ctx context.Context, id, imageType string, idx int, data []byte, ct string) error {
	if c.uploads >= c.uploadsLanded {
		return errSimulatedCrash
	}
	c.uploads++
	return c.fanartResyncClient.UploadImageAtIndex(ctx, id, imageType, idx, data, ct)
}

// crashResyncAfter runs fn with newFanartResyncClient wrapped so the resync
// dies after uploadsLanded uploads, and restores the real seam before
// returning, so whatever runs next (the reconciler) is the production path.
func crashResyncAfter(t *testing.T, uploadsLanded int, fn func()) {
	t.Helper()
	orig := newFanartResyncClient
	newFanartResyncClient = func(conn *connection.Connection, logger *slog.Logger) fanartResyncClient {
		real := orig(conn, logger)
		if real == nil {
			return nil
		}
		return &crashingResyncClient{fanartResyncClient: real, uploadsLanded: uploadsLanded}
	}
	defer func() { newFanartResyncClient = orig }()
	fn()
}

// TestResyncCrash_ReconcilerConvergesJellyfin is the #3147 regression. An
// operator-triggered full-set push (a Backdrops-tab reorder) is interrupted
// between the resync's delete loop and the end of its upload loop, leaving
// the platform holding only a prefix of the local set. The real reconciler
// then runs against real SQLite and a peer that models Jellyfin's backdrop
// list (appends whatever the index, renumbers on delete). The platform must
// converge to EXACTLY the local set, slot by slot, and a second pass must
// write nothing.
//
// "local duplicate, prefix holds every distinct image" is the case the
// count-only deficit check (#3144) cannot see: local A,A,B and a crash after
// two uploads leaves A,A, whose count equals the distinct-image count, so the
// reconciler read it as converged and left B off the platform for good.
func TestResyncCrash_ReconcilerConvergesJellyfin(t *testing.T) {
	A, B, C := bandJPEG(t, 51), bandJPEG(t, 52), bandJPEG(t, 53)
	cases := []struct {
		name    string
		local   [][]byte
		seed    [][]byte // the platform before the interrupted push
		landed  int
		crashed [][]byte // what the crash must leave (precondition)
	}{
		{"zero uploads landed", [][]byte{A, B, C}, [][]byte{C, B, A}, 0, nil},
		{"a prefix landed", [][]byte{A, B, C}, [][]byte{C, B, A}, 2, [][]byte{A, B}},
		{"local duplicate, prefix holds every distinct image", [][]byte{A, A, B}, [][]byte{B, A, A}, 2, [][]byte{A, A}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			peer := &statefulBackdropPeer{appendAll: true, data: append([][]byte(nil), tc.seed...)}
			p, a := durabilityHarness(t, connection.TypeJellyfin, peer, tc.local)

			var warnings []string
			crashResyncAfter(t, tc.landed, func() { warnings = p.SyncAllFanartToPlatforms(ctx, a) })
			// PRECONDITION: the crash really landed between the loops. Every
			// old backdrop was deleted, exactly `landed` uploads arrived, and
			// the push reported the failure rather than success.
			got, _ := peer.state()
			assertPeerHolds(t, "after the interrupted push", got, tc.crashed)
			if peer.deleteCount() != len(tc.seed) {
				t.Fatalf("precondition: %d deletes issued, want %d (the whole old set)", peer.deleteCount(), len(tc.seed))
			}
			if len(warnings) == 0 {
				t.Fatalf("precondition: the interrupted push reported no warning")
			}

			p.ReconcileArtworkToPlatforms(ctx)
			got, writes1 := peer.state()
			assertPeerHolds(t, "after reconciler pass 1", got, tc.local)

			p.ReconcileArtworkToPlatforms(ctx)
			got, writes2 := peer.state()
			assertPeerHolds(t, "after reconciler pass 2", got, tc.local)
			if writes2 != writes1 {
				t.Errorf("reconciler pass 2 issued %d platform writes, want 0", writes2-writes1)
			}
		})
	}
}

// TestResyncCrash_PrunedDuplicateStaysPruned pins the #3144 contract against
// the identity check this issue added: a platform holding every distinct local
// image IN LOCAL FIRST-OCCURRENCE ORDER (what the prune leaves) is converged,
// even below the local file count, so the reconciler must not rebuild it.
func TestResyncCrash_PrunedDuplicateStaysPruned(t *testing.T) {
	A, B := bandJPEG(t, 54), bandJPEG(t, 55)
	ctx := context.Background()
	peer := &statefulBackdropPeer{appendAll: true, data: [][]byte{A, B}}
	p, _ := durabilityHarness(t, connection.TypeJellyfin, peer, [][]byte{A, B, A})
	p.ReconcileArtworkToPlatforms(ctx)
	got, writes := peer.state()
	assertPeerHolds(t, "after reconciler pass", got, [][]byte{A, B})
	if writes != 0 {
		t.Errorf("reconciler issued %d writes against a pruned platform, want 0", writes)
	}
}

// TestResyncCrash_EmbyNeverResyncs is the Emby contrast: a partial Emby
// platform (what an interrupted per-file push leaves) is repaired by the
// indexed per-file upload, never by the resync. No resync client is built and
// no backdrop is deleted.
func TestResyncCrash_EmbyNeverResyncs(t *testing.T) {
	A, B, C := bandJPEG(t, 56), bandJPEG(t, 57), bandJPEG(t, 58)
	ctx := context.Background()
	peer := &statefulBackdropPeer{data: [][]byte{A}}
	p, _ := durabilityHarness(t, connection.TypeEmby, peer, [][]byte{A, B, C})

	built := 0
	orig := newFanartResyncClient
	newFanartResyncClient = func(conn *connection.Connection, logger *slog.Logger) fanartResyncClient {
		built++
		return orig(conn, logger)
	}
	t.Cleanup(func() { newFanartResyncClient = orig })

	for pass := 1; pass <= 2; pass++ {
		p.ReconcileArtworkToPlatforms(ctx)
		got, _ := peer.state()
		assertPeerHolds(t, "after reconciler pass", got, [][]byte{A, B, C})
	}
	if built != 0 {
		t.Errorf("an Emby reconcile built %d resync clients, want 0", built)
	}
	if d := peer.deleteCount(); d != 0 {
		t.Errorf("an Emby reconcile deleted %d backdrops, want 0", d)
	}
}

// erroringBackdropReader fails every platform byte read.
type erroringBackdropReader struct{ reads int }

func (r *erroringBackdropReader) GetArtistDetail(context.Context, string) (*connection.ArtistPlatformState, error) {
	return nil, errSimulatedCrash
}

func (r *erroringBackdropReader) GetArtistBackdrop(context.Context, string, int) ([]byte, string, error) {
	r.reads++
	return nil, "", errSimulatedCrash
}

// TestFanartDeficit_IdentityTierFailsClosed pins tier 3's fail direction: when
// the platform's bytes cannot be read (or the peer cannot read them at all),
// a count between the distinct and file counts is NOT a deficit this pass, so
// a peer read failure never rebuilds a pruned platform (#3144).
func TestFanartDeficit_IdentityTierFailsClosed(t *testing.T) {
	dir := t.TempDir()
	A, B := bandJPEG(t, 59), bandJPEG(t, 60)
	writeFile(t, dir+"/fanart.jpg", A)
	writeFile(t, dir+"/fanart2.jpg", A)
	writeFile(t, dir+"/fanart3.jpg", B)
	p := New(Deps{Logger: silentLogger()})
	state := &connection.ArtistPlatformState{BackdropCount: 2}
	ctx := context.Background()

	r := &erroringBackdropReader{}
	if p.fanartDeficit(ctx, "a1", dir, state, r, "p1") {
		t.Error("a platform read error flagged a deficit, want none this pass")
	}
	if r.reads == 0 {
		t.Fatal("precondition: the identity tier never read the platform")
	}
	if p.fanartDeficit(ctx, "a1", dir, state, nil, "p1") {
		t.Error("a peer with no backdrop reader flagged a deficit, want none")
	}
	// Control: below the distinct count is a deficit with no platform read.
	if !p.fanartDeficit(ctx, "a1", dir, &connection.ArtistPlatformState{BackdropCount: 1}, nil, "p1") {
		t.Error("a platform below the distinct count was not flagged")
	}
}
