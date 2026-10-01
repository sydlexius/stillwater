//go:build integration

package publish

import (
	"context"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
)

// livePruneClient is what the live #3144 scenario needs from a real peer.
type livePruneClient interface {
	backdropClearer
	UploadImage(ctx context.Context, platformArtistID, imageType string, data []byte, contentType string) error
	GetArtistBackdrop(ctx context.Context, platformArtistID string, index int) ([]byte, string, error)
}

// livePruneReconcile is the #3144 acceptance measurement against a REAL peer:
// seed the item, run the real prune, then run the real reconciler pass twice,
// logging the backdrop count before the prune, after it, and after each pass,
// and asserting the exact platform set at every step.
//
// It CLEARS every backdrop on the target item first and again on cleanup, so
// the item must be a scratch item.
func livePruneReconcile(t *testing.T, connType, url, apiKey, userID, itemID string, client livePruneClient, local, seed, afterPrune, afterPass [][]byte) {
	ctx, cancel := context.WithTimeout(context.Background(), liveBackdropTestTimeout)
	defer cancel()
	clearAllBackdrops(ctx, t, itemID, client)
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), liveBackdropCleanupTimeout)
		defer ccancel()
		clearAllBackdrops(cctx, t, itemID, client)
	})

	// Seed by NON-indexed upload, which appends on both peers.
	for i, b := range seed {
		if err := client.UploadImage(ctx, itemID, "fanart", b, "image/jpeg"); err != nil {
			t.Fatalf("seeding backdrop %d: %v", i, err)
		}
	}
	p, a := durabilityPublisher(t, connType, url, apiKey, userID, itemID, local)

	measure := func(step string, want [][]byte) {
		t.Helper()
		st := pollBackdropDetail(ctx, t, client, itemID, len(want))
		t.Logf("BackdropCount %s = %d (want %d)", step, st.BackdropCount, len(want))
		if st.BackdropCount != len(want) {
			t.Fatalf("%s: BackdropCount = %d, want %d", step, st.BackdropCount, len(want))
		}
		for i, w := range want {
			got, _, err := client.GetArtistBackdrop(ctx, itemID, i)
			if err != nil {
				t.Fatalf("%s: reading backdrop %d: %v", step, i, err)
			}
			if hashOf(got) != hashOf(w) {
				t.Errorf("%s: backdrop %d does not hold the expected image", step, i)
			}
		}
	}

	// PRECONDITION: the seed landed exactly, so the prune has copies to remove.
	measure("before prune", seed)
	res, err := p.PrunePlatformBackdropDuplicates(ctx, PlatformBackdropPruneScope{ArtistID: a.ID})
	if err != nil || len(res.Failures) != 0 {
		t.Fatalf("prune: err=%v failures=%v", err, res.Failures)
	}
	if want := len(seed) - len(afterPrune); res.BackdropsRemoved != want {
		t.Fatalf("precondition: prune removed %d, want %d", res.BackdropsRemoved, want)
	}
	measure("after prune", afterPrune)
	for pass := 1; pass <= 2; pass++ {
		p.ReconcileArtworkToPlatforms(ctx)
		time.Sleep(500 * time.Millisecond)
		measure("after reconciler pass "+string(rune('0'+pass)), afterPass)
	}
}

func liveEmbyPrune(t *testing.T, local, seed, afterPrune, afterPass [][]byte) {
	env := loadLiveEmbyEnv(t)
	livePruneReconcile(t, connection.TypeEmby, env.url, env.apiKey, env.userID, env.itemID,
		emby.New(env.url, env.apiKey, env.userID, silentLogger()), local, seed, afterPrune, afterPass)
}

func liveJellyfinPrune(t *testing.T, local, seed, afterPrune, afterPass [][]byte) {
	env := loadLiveJellyfinEnv(t)
	livePruneReconcile(t, connection.TypeJellyfin, env.url, env.apiKey, env.userID, env.itemID,
		jellyfin.New(env.url, env.apiKey, env.userID, silentLogger()), local, seed, afterPrune, afterPass)
}

// Durability: local holds A,B,A; the platform holds A,B,A,B. The prune leaves
// A,B, below the local FILE count (3) but holding every distinct local image,
// and both reconciler passes must leave it there. Before #3144 pass 1 re-pushed
// the set and restored a removed copy.
func TestLivePruneSurvivesReconcile_Emby(t *testing.T) {
	A, B := bandJPEG(t, 0xA1), bandJPEG(t, 0xA2)
	liveEmbyPrune(t, [][]byte{A, B, A}, [][]byte{A, B, A, B}, [][]byte{A, B}, [][]byte{A, B})
}

func TestLivePruneSurvivesReconcile_Jellyfin(t *testing.T) {
	A, B := bandJPEG(t, 0xA1), bandJPEG(t, 0xA2)
	liveJellyfinPrune(t, [][]byte{A, B, A}, [][]byte{A, B, A, B}, [][]byte{A, B}, [][]byte{A, B})
}

// Repair (#1869 must survive): local holds A,B,A,C; the platform holds A,A.
// The prune leaves A, which is genuinely missing B and C, so pass 1 must push
// the local set and pass 2 must leave the repaired set alone.
func TestLivePruneThenGenuineDeficitStillRepaired_Emby(t *testing.T) {
	A, B, C := bandJPEG(t, 0xB1), bandJPEG(t, 0xB2), bandJPEG(t, 0xB3)
	liveEmbyPrune(t, [][]byte{A, B, A, C}, [][]byte{A, A}, [][]byte{A}, [][]byte{A, B, A, C})
}

func TestLivePruneThenGenuineDeficitStillRepaired_Jellyfin(t *testing.T) {
	A, B, C := bandJPEG(t, 0xB1), bandJPEG(t, 0xB2), bandJPEG(t, 0xB3)
	liveJellyfinPrune(t, [][]byte{A, B, A, C}, [][]byte{A, A}, [][]byte{A}, [][]byte{A, B, A, C})
}
