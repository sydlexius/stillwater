//go:build integration

package publish

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
)

// Live proof for #3200, full path: SyncAllFanartToPlatforms against a REAL
// Emby with a REAL unreadable local fanart file (chmod 0). It uses only API
// that also exists before the #3311 guard, so scripts/live-emby-proof.sh can
// run it against the pre-fix tree and require the LIVE-PROOF-DEFECT marker.
// Serial on purpose: every test shares one Emby item.

// nilSlotSeeds are the five distinct fixture images, local slots 1..5.
var nilSlotSeeds = []int{0xA1, 0xA2, 0xA3, 0xA4, 0xA5}

// nilSlotRig is one test's live peer plus its local fixture directory.
type nilSlotRig struct {
	env    liveEmbyEnv
	client *emby.Client
	p      *Publisher
	art    *artist.Artist
	dir    string
	imgs   [][]byte
}

func newNilSlotRig(ctx context.Context, t *testing.T) *nilSlotRig {
	t.Helper()
	env := loadLiveEmbyEnv(t)
	logger := silentLogger()
	client := emby.New(env.url, env.apiKey, env.userID, logger)
	resetNilSlotFullPathItem(ctx, t, env.itemID, client, true)
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), liveBackdropCleanupTimeout)
		defer cancel()
		resetNilSlotFullPathItem(cctx, t, env.itemID, client, false)
	})
	r := &nilSlotRig{env: env, client: client, dir: t.TempDir()}
	for i, seed := range nilSlotSeeds {
		r.imgs = append(r.imgs, bandJPEG(t, seed))
		for j := 0; j < i; j++ {
			if hashOf(r.imgs[j]) == hashOf(r.imgs[i]) {
				t.Fatalf("LIVE-PROOF-SETUP: fixture images %d and %d are byte-identical", j+1, i+1)
			}
		}
		if err := os.WriteFile(r.file(i), r.imgs[i], 0o600); err != nil {
			t.Fatalf("writing %s: %v", r.file(i), err)
		}
	}
	// Registered after TempDir, so it runs first and the directory can be removed.
	t.Cleanup(func() { _ = os.Chmod(r.file(2), 0o600) })
	r.p = New(Deps{
		Logger: logger,
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "live-nil-slot", ConnectionID: "c-emby", PlatformArtistID: env.itemID},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {ID: "c-emby", Name: "live-emby-uat", Type: connection.TypeEmby, URL: env.url, APIKey: env.apiKey, Enabled: true, Status: "ok", Emby: &connection.EmbyConfig{PlatformUserID: env.userID, FeatureImageWrite: true}},
		}},
	})
	r.art = &artist.Artist{ID: "live-nil-slot", Name: "Live UAT Artist", Path: r.dir}
	return r
}

// Duplicated on purpose: each live test file is copied ALONE into an old-ref
// extract, so it cannot share a helper with the other file.
// resetNilSlotFullPathItem empties the item and CONVERGES: Emby 4.10.1.0 answers a delete (and a
// non-indexed upload) with a 500 SQLiteException yet applies it, so a delete
// error is tolerated and only the polled state counts. It re-deletes whatever
// remains until the count reads zero (bounded 30s) and fails loudly otherwise.
// fatal is false in t.Cleanup, where Fatal would abort the teardown.
func resetNilSlotFullPathItem(ctx context.Context, t *testing.T, itemID string, client *emby.Client, fatal bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for ctx.Err() == nil {
		state, err := client.GetArtistDetail(ctx, itemID)
		if err == nil && state.BackdropCount == 0 {
			return
		}
		if err == nil {
			for i := state.BackdropCount - 1; i >= 0; i-- {
				_ = client.DeleteImageAtIndex(ctx, itemID, "fanart", i)
			}
		}
		if time.Now().After(deadline) {
			msg := fmt.Sprintf("LIVE-PROOF-SETUP: item %s did not reach 0 backdrops within 30s (last read err=%v)", itemID, err)
			if fatal {
				t.Fatal(msg)
			}
			t.Error(msg)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	if fatal {
		t.Fatalf("LIVE-PROOF-SETUP: context ended before item %s reached 0 backdrops: %v", itemID, ctx.Err())
	}
	t.Errorf("LIVE-PROOF-SETUP: context ended before item %s reached 0 backdrops: %v", itemID, ctx.Err())
}

// file is local slot i's path: fanart.jpg, then fanart2.jpg ... fanart5.jpg.
func (r *nilSlotRig) file(i int) string {
	if i == 0 {
		return filepath.Join(r.dir, "fanart.jpg")
	}
	return filepath.Join(r.dir, "fanart"+string(rune('1'+i))+".jpg")
}

// makeThirdUnreadable chmods local slot 3 to 0 and Fatals if that did not make
// it unreadable (root, or a filesystem that ignores modes): never a Skip.
func (r *nilSlotRig) makeThirdUnreadable(t *testing.T) {
	t.Helper()
	if err := os.Chmod(r.file(2), 0); err != nil {
		t.Fatalf("LIVE-PROOF-SETUP: chmod 0: %v", err)
	}
	if _, err := os.ReadFile(r.file(2)); err == nil {
		t.Fatalf("LIVE-PROOF-SETUP: precondition failed: %s is still readable after chmod 0 (running as root, or the filesystem ignores mode bits)", r.file(2))
	}
}

// peerHashes settles on want backdrops and returns the settled count plus the
// content hash of every slot.
func (r *nilSlotRig) peerHashes(ctx context.Context, t *testing.T, want int) (int, []string) {
	t.Helper()
	n := pollBackdropDetail(ctx, t, r.client, r.env.itemID, want).BackdropCount
	hs := make([]string, 0, n)
	for i := 0; i < n; i++ {
		data, _, err := r.client.GetArtistBackdrop(ctx, r.env.itemID, i)
		if err != nil {
			t.Fatalf("reading peer slot %d: %v", i, err)
		}
		hs = append(hs, hashOf(data))
	}
	return n, hs
}

// check logs a LIVE-PROOF-COUNT line and reports a LIVE-PROOF-DEFECT when the
// peer is not exactly the wanted local slots, by count, uniqueness, and order.
func (r *nilSlotRig) check(ctx context.Context, t *testing.T, scenario string, run int, warnings []string, wantSlots []int) {
	t.Helper()
	r.verify(ctx, t, "LIVE-PROOF-DEFECT", false, scenario, run, warnings, wantSlots)
}

// checkSetup is check for a SEED step: a failure there is the harness failing
// to build its fixture, never evidence of the defect, so it is a Fatal
// LIVE-PROOF-SETUP and can never satisfy the runner's defect rule.
func (r *nilSlotRig) checkSetup(ctx context.Context, t *testing.T, scenario string, warnings []string, wantSlots []int) {
	t.Helper()
	r.verify(ctx, t, "LIVE-PROOF-SETUP", true, scenario, 0, warnings, wantSlots)
}

func (r *nilSlotRig) verify(ctx context.Context, t *testing.T, marker string, fatal bool, scenario string, run int, warnings []string, wantSlots []int) {
	t.Helper()
	report := t.Errorf
	if fatal {
		report = t.Fatalf
	}
	t.Logf("run %d warnings (informational): %v", run, warnings)
	n, hs := r.peerHashes(ctx, t, len(wantSlots))
	t.Logf("LIVE-PROOF-COUNT scenario=%s run=%d count=%d", scenario, run, n)
	seen := map[string]bool{}
	for _, h := range hs {
		if seen[h] {
			report("%s: scenario=%s run=%d peer holds a duplicate backdrop (count=%d)", marker, scenario, run, n)
			break
		}
		seen[h] = true
	}
	if n != len(wantSlots) {
		report("%s: scenario=%s run=%d BackdropCount=%d, want %d", marker, scenario, run, n, len(wantSlots))
		return
	}
	for i, slot := range wantSlots {
		if hs[i] != hashOf(r.imgs[slot]) {
			report("%s: scenario=%s run=%d peer slot %d is not local image %d", marker, scenario, run, i, slot+1)
		}
	}
}

// A1-short: an empty peer and a permanently unreadable local slot 3. Survivors
// are compacted in local order and repeat runs neither grow nor duplicate; once
// the file is readable again one run converges to [1..5].
func TestLiveEmby_NilSlotFullPath_ShortPeerCompactsWithoutDuplicates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*liveBackdropTestTimeout)
	defer cancel()
	r := newNilSlotRig(ctx, t)
	r.makeThirdUnreadable(t)
	for run := 1; run <= 3; run++ {
		r.check(ctx, t, "short", run, r.p.SyncAllFanartToPlatforms(ctx, r.art), []int{0, 1, 3, 4})
	}
	if err := os.Chmod(r.file(2), 0o600); err != nil {
		t.Fatalf("LIVE-PROOF-SETUP: restoring mode: %v", err)
	}
	r.check(ctx, t, "short-restored", 4, r.p.SyncAllFanartToPlatforms(ctx, r.art), []int{0, 1, 2, 3, 4})
}

// A1-full: a peer already holding all five. The unreadable slot 3 must leave
// every peer slot in place (no shift, no growth).
func TestLiveEmby_NilSlotFullPath_FullPeerStaysInPlace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*liveBackdropTestTimeout)
	defer cancel()
	r := newNilSlotRig(ctx, t)
	// Emby 4.10.x can 500 a write it applied, so the seed is judged by the
	// peer's settled state in check below, not by warnings.
	t.Logf("seeding sync warnings (informational): %s", strings.Join(r.p.SyncAllFanartToPlatforms(ctx, r.art), "; "))
	r.checkSetup(ctx, t, "full-seed", nil, []int{0, 1, 2, 3, 4})
	r.makeThirdUnreadable(t)
	for run := 1; run <= 2; run++ {
		r.check(ctx, t, "full", run, r.p.SyncAllFanartToPlatforms(ctx, r.art), []int{0, 1, 2, 3, 4})
	}
}
