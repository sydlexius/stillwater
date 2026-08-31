//go:build integration

package publish

import (
	"context"
	"os"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
)

// loadLiveEmbyBystanderItemID reads the SECOND Emby item the scoped-prune
// test needs, alongside the four coordinates loadLiveEmbyEnv already reads.
//
// A separate variable rather than a widened liveEmbyEnv: every other live
// test in this package needs exactly one item, and only this one needs a
// bystander. Folding it into loadLiveEmbyEnv would make all of them skip
// whenever it is unset.
//
// SW_LIVE_EMBY_BYSTANDER_ITEM_ID must name a DIFFERENT item from
// SW_LIVE_EMBY_ITEM_ID; the test refuses to run if they are equal, since a
// bystander that is the target cannot demonstrate anything about scoping.
func loadLiveEmbyBystanderItemID(t *testing.T, targetItemID string) string {
	t.Helper()
	id := os.Getenv("SW_LIVE_EMBY_BYSTANDER_ITEM_ID")
	if id == "" {
		t.Skip("SW_LIVE_EMBY_BYSTANDER_ITEM_ID not set; skipping live scoped-prune test")
	}
	if id == targetItemID {
		t.Fatalf("SW_LIVE_EMBY_BYSTANDER_ITEM_ID (%s) must differ from SW_LIVE_EMBY_ITEM_ID; a bystander that IS the target proves nothing about scoping", id)
	}
	return id
}

// TestLiveEmby_ScopedPruneLeavesTheBystanderUntouched is #3139's
// live-server acceptance criterion: "a single-artist prune deletes only
// that artist's redundant backdrops, verified by measuring backdrop counts
// on a real peer for the pruned artist AND for an untouched bystander
// artist in the same run."
//
// WHY A FAKE CANNOT CLOSE THIS. The unit-level sibling
// (TestPruneScope_SingleArtistLeavesTheBystanderUntouched) asserts against
// a fake that records "delete was called". On the parent branch a fake that
// did not re-index on delete modeled a platform that does not exist, and
// that single inaccuracy survived 26 killed mutations and a full hostile
// review. Only a real peer decides whether a delete really happened, what
// the surviving count is, and how the remaining slots are numbered.
//
// Both artists are seeded with the SAME redundant shape -- three backdrops
// of which two are byte-identical. The bystander's duplicate pair is the
// load-bearing half: a bystander with nothing prunable would survive even a
// completely unscoped library-wide run, and the test would pass vacuously.
func TestLiveEmby_ScopedPruneLeavesTheBystanderUntouched(t *testing.T) {
	env := loadLiveEmbyEnv(t)
	bystanderItemID := loadLiveEmbyBystanderItemID(t, env.itemID)

	ctx, cancel := context.WithTimeout(context.Background(), liveBackdropTestTimeout)
	defer cancel()

	logger := silentLogger()
	client := emby.New(env.url, env.apiKey, env.userID, logger)

	// seedRedundant puts three backdrops on an item -- indices 0 and 1
	// byte-identical, index 2 distinct -- and asserts the peer really
	// accepted all three. Clearing first rather than trusting the item's
	// current state: a real UAT container is not guaranteed clean, and a
	// POST to an occupied index behaves differently per peer.
	seedRedundant := func(itemID string, seed int) {
		t.Helper()
		clearAllBackdrops(ctx, t, itemID, client)
		dup, distinct := bandJPEG(t, seed), bandJPEG(t, seed+1)
		for i, payload := range [][]byte{dup, dup, distinct} {
			if err := client.UploadImageAtIndex(ctx, itemID, "fanart", i, payload, "image/jpeg"); err != nil {
				t.Fatalf("item %s: seeding backdrop %d: %v", itemID, i, err)
			}
		}
	}
	countOf := func(itemID string) int {
		t.Helper()
		state, err := client.GetArtistDetail(ctx, itemID)
		if err != nil {
			t.Fatalf("item %s: reading backdrop count: %v", itemID, err)
		}
		return state.BackdropCount
	}

	// Registered BEFORE seeding so a failure anywhere below -- including in
	// the seeding itself -- still leaves both items clean for the next run.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), liveBackdropCleanupTimeout)
		defer cleanupCancel()
		clearAllBackdrops(cleanupCtx, t, env.itemID, client)
		clearAllBackdrops(cleanupCtx, t, bystanderItemID, client)
	})

	seedRedundant(env.itemID, 0xD1)
	seedRedundant(bystanderItemID, 0xD3)

	// PRECONDITIONS, asserted rather than assumed. Without these the
	// after-measurements below are worthless: "the bystander still has 3"
	// says nothing if it never had 3, and "the target dropped to 2" says
	// nothing if the peer only ever accepted 2.
	if got := countOf(env.itemID); got != 3 {
		t.Fatalf("precondition: target %s has %d backdrops after seeding, want 3", env.itemID, got)
	}
	if got := countOf(bystanderItemID); got != 3 {
		t.Fatalf("precondition: bystander %s has %d backdrops after seeding, want 3", bystanderItemID, got)
	}
	t.Logf("BEFORE: target %s = 3 backdrops, bystander %s = 3 backdrops", env.itemID, bystanderItemID)

	svc := &scopedArtistLister{
		fakePlatformLister: &fakePlatformLister{
			artists: []artist.Artist{{ID: "live-target", Name: "Live Target"}, {ID: "live-bystander", Name: "Live Bystander"}},
		},
		byArtist: map[string][]artist.PlatformID{
			"live-target":    {{ArtistID: "live-target", ConnectionID: "c-emby", PlatformArtistID: env.itemID}},
			"live-bystander": {{ArtistID: "live-bystander", ConnectionID: "c-emby", PlatformArtistID: bystanderItemID}},
		},
	}
	p := New(Deps{
		Logger:        logger,
		ArtistService: svc,
		ArtistLister:  svc,
		ArtistGetter:  svc,
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {
				ID: "c-emby", Name: "live-emby-uat", Type: connection.TypeEmby,
				URL: env.url, APIKey: env.apiKey, Enabled: true, Status: "ok",
				Emby: &connection.EmbyConfig{PlatformUserID: env.userID, FeatureImageWrite: true},
			},
		}},
	})

	// The real publisher path, against the real peer, scoped to ONE artist.
	res, err := p.PrunePlatformBackdropDuplicates(ctx, PlatformBackdropPruneScope{ArtistID: "live-target"})
	if err != nil {
		t.Fatalf("scoped prune: %v", err)
	}
	if res.BackdropsRemoved != 1 {
		t.Errorf("BackdropsRemoved = %d, want 1 (one of the two byte-identical copies)", res.BackdropsRemoved)
	}
	if len(res.Failures) != 0 {
		t.Errorf("prune reported failures: %+v", res.Failures)
	}

	// THE MEASUREMENT the AC asks for: counts read back from the peer, for
	// both artists, after the same run.
	targetAfter, bystanderAfter := countOf(env.itemID), countOf(bystanderItemID)
	t.Logf("AFTER: target %s = %d backdrops, bystander %s = %d backdrops", env.itemID, targetAfter, bystanderItemID, bystanderAfter)

	if targetAfter != 2 {
		t.Errorf("target %s: %d backdrops after the prune, want 2 (the duplicate removed, its survivor and the distinct image kept)", env.itemID, targetAfter)
	}
	if bystanderAfter != 3 {
		t.Errorf("BYSTANDER %s: %d backdrops after a prune scoped to another artist, want its original 3; the scope did not narrow anything on the real peer", bystanderItemID, bystanderAfter)
	}
}
