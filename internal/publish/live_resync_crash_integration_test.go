//go:build integration

package publish

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
)

// liveResyncCrash is the #3147 acceptance measurement against a REAL peer: seed
// the item, run an operator full-set push whose resync is interrupted after
// `landed` uploads (crashResyncAfter: every delete issued, then the client
// fails), then run the real reconciler twice, logging the backdrop count at
// each step and asserting the exact platform set.
//
// It CLEARS every backdrop on the target item first and again on cleanup, so
// the item must be a scratch item.
func liveResyncCrash(t *testing.T, connType, url, apiKey, userID, itemID string, client livePruneClient, local, seed [][]byte, landed int, crashed [][]byte) {
	ctx, cancel := context.WithTimeout(context.Background(), liveBackdropTestTimeout)
	defer cancel()
	clearAllBackdrops(ctx, t, itemID, client)
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), liveBackdropCleanupTimeout)
		defer ccancel()
		clearAllBackdrops(cctx, t, itemID, client)
	})
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

	measure("before the interrupted push", seed)
	crashResyncAfter(t, landed, func() { _ = p.SyncAllFanartToPlatforms(ctx, a) })
	measure("after the interrupted push", crashed)
	for pass := 1; pass <= 2; pass++ {
		p.ReconcileArtworkToPlatforms(ctx)
		time.Sleep(500 * time.Millisecond)
		measure("after reconciler pass "+string(rune('0'+pass)), local)
	}
}

func liveJellyfinCrash(t *testing.T, local, seed [][]byte, landed int, crashed [][]byte) {
	// This suite takes its own scratch item, falling back to the shared one.
	// The internal/api live handler suite also clears and rewrites
	// SW_LIVE_JELLYFIN_ITEM_ID, and `go test -tags integration ./...` may run
	// both package binaries at once, so running both packages together needs
	// distinct items or `-p 1`.
	if id := os.Getenv("SW_LIVE_JELLYFIN_CRASH_ITEM_ID"); id != "" {
		t.Setenv("SW_LIVE_JELLYFIN_ITEM_ID", id)
	}
	env := loadLiveJellyfinEnv(t)
	liveResyncCrash(t, connection.TypeJellyfin, env.url, env.apiKey, env.userID, env.itemID,
		jellyfin.New(env.url, env.apiKey, env.userID, silentLogger()), local, seed, landed, crashed)
}

// Zero uploads landed: the platform is left EMPTY. The reconciler must rebuild
// the exact local set, and a second pass must leave it alone.
func TestLiveResyncCrash_ZeroLanded_Jellyfin(t *testing.T) {
	A, B, C := bandJPEG(t, 0xC1), bandJPEG(t, 0xC2), bandJPEG(t, 0xC3)
	liveJellyfinCrash(t, [][]byte{A, B, C}, [][]byte{C, B, A}, 0, nil)
}

// A prefix landed: the platform holds A,B of local A,B,C.
func TestLiveResyncCrash_PrefixLanded_Jellyfin(t *testing.T) {
	A, B, C := bandJPEG(t, 0xC4), bandJPEG(t, 0xC5), bandJPEG(t, 0xC6)
	liveJellyfinCrash(t, [][]byte{A, B, C}, [][]byte{C, B, A}, 2, [][]byte{A, B})
}

// The count-ambiguous prefix: local A,A,B, crash after two uploads leaves A,A,
// whose count equals the distinct-image count. Before #3147 the reconciler
// read that as converged and B never came back.
func TestLiveResyncCrash_DuplicatePrefixLanded_Jellyfin(t *testing.T) {
	A, B := bandJPEG(t, 0xC7), bandJPEG(t, 0xC8)
	liveJellyfinCrash(t, [][]byte{A, A, B}, [][]byte{B, A, A}, 2, [][]byte{A, A})
}

// Emby contrast: the same push with the same crash hook armed never builds a
// resync client (Emby takes the indexed per-file replace), so nothing is
// deleted and the push itself lands the local set in place.
func TestLiveResyncCrash_EmbyUnaffected(t *testing.T) {
	env := loadLiveEmbyEnv(t)
	A, B, C := bandJPEG(t, 0xC9), bandJPEG(t, 0xCA), bandJPEG(t, 0xCB)
	built := 0
	orig := newFanartResyncClient
	newFanartResyncClient = func(conn *connection.Connection, logger *slog.Logger) fanartResyncClient {
		built++
		return orig(conn, logger)
	}
	t.Cleanup(func() { newFanartResyncClient = orig })
	liveResyncCrash(t, connection.TypeEmby, env.url, env.apiKey, env.userID, env.itemID,
		emby.New(env.url, env.apiKey, env.userID, silentLogger()),
		[][]byte{A, B, C}, [][]byte{C, B, A}, 0, [][]byte{A, B, C})
	if built != 0 {
		t.Errorf("an Emby push and reconcile built %d resync clients, want 0", built)
	}
}
