//go:build integration

package publish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/image"
)

// liveBackdropTestTimeout bounds each live-server call so a stalled UAT
// container fails the test quickly instead of hanging the run.
const liveBackdropTestTimeout = 30 * time.Second

// solidJPEG returns a tiny, valid, distinguishable JPEG. Three different
// "colors" (arbitrary byte fillers, not real pixel colors) give three
// byte-distinct payloads so a test can prove WHICH backdrop slot changed
// by hashing the bytes, not just by counting slots.
func solidJPEG(fill byte) []byte {
	// A minimal but real JPEG: SOI, a tiny APP0 JFIF header, a payload block
	// (the "color"), EOI. Real media servers only need a decodable image to
	// accept the upload and report Width/Height back; this is intentionally
	// not a full raster, matching the minimal-JPEG convention already used
	// by seedJPG in helpers_test.go for the same reason (server acceptance,
	// not pixel-perfect rendering).
	var buf bytes.Buffer
	buf.Write([]byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00})
	for i := 0; i < 64; i++ {
		buf.WriteByte(fill)
	}
	buf.Write([]byte{0xff, 0xd9})
	return buf.Bytes()
}

func hashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}

// backdropClearer is the minimal surface clearAllBackdrops needs: read the
// current count and delete a specific index. Both emby.Client and
// jellyfin.Client satisfy this via connection.IndexedImageDeleter +
// connection.ArtistStateGetter.
type backdropClearer interface {
	GetArtistDetail(ctx context.Context, platformArtistID string) (*connection.ArtistPlatformState, error)
	DeleteImageAtIndex(ctx context.Context, platformArtistID string, imageType string, index int) error
}

// clearAllBackdrops deletes every existing backdrop on the given item,
// HIGH-INDEX-FIRST, and asserts the item ends at zero. Round-1 fix (#3125
// review): the live tests previously seeded by POSTing straight to indices
// 0/1 and asserted the resulting count as a precondition, but that only
// holds when the item started with ZERO backdrops -- a real UAT server is
// not guaranteed to be in that state (a prior run, or another test, can
// leave backdrops behind). POSTing to an occupied index REPLACES on Emby
// but APPENDS on Jellyfin (the #3125 finding this whole file exists to
// measure), so a dirty item makes the "seed two, expect count==2"
// precondition silently wrong on Jellyfin and merely lucky on Emby.
//
// High-index-first matters because DeleteImageAtIndexRaw's own doc comment
// records that the peer RE-INDEXES remaining backdrops after each delete:
// deleting index 0 first would shift what was index 1 down to index 0,
// so a naive ascending loop skips every other slot on a peer with an odd
// habit of re-indexing mid-loop. Descending avoids that entirely -- deleting
// the highest index first never disturbs any index this loop has not
// visited yet.
func clearAllBackdrops(t *testing.T, ctx context.Context, itemID string, client backdropClearer) {
	t.Helper()
	state, err := client.GetArtistDetail(ctx, itemID)
	if err != nil {
		t.Fatalf("clearAllBackdrops: reading current state: %v", err)
	}
	for i := state.BackdropCount - 1; i >= 0; i-- {
		if err := client.DeleteImageAtIndex(ctx, itemID, "fanart", i); err != nil {
			t.Fatalf("clearAllBackdrops: deleting backdrop %d: %v", i, err)
		}
	}
	// Re-verify rather than trust the loop: the count read above could
	// itself be stale, or a delete could silently no-op on some peer.
	after, err := client.GetArtistDetail(ctx, itemID)
	if err != nil {
		t.Fatalf("clearAllBackdrops: reading state after clear: %v", err)
	}
	if after.BackdropCount != 0 {
		t.Fatalf("clearAllBackdrops: BackdropCount = %d after clearing, want 0 (item was not fully cleared)", after.BackdropCount)
	}
}

// liveEmbyEnv reads the UAT Emby coordinates the test needs and skips when
// any are unset, per the repo's integration-test convention (see
// internal/provider/integration_test.go). SW_LIVE_EMBY_ITEM_ID is deliberately
// a generic library item, not necessarily a MusicArtist -- see the #3125
// issue's stated verification limit: the UAT Emby instance has no
// music-artist items, and Emby's image endpoints are item-type-generic (same
// controller, same routes) so a MusicAlbum item exercises the identical code
// path.
type liveEmbyEnv struct {
	url    string
	apiKey string
	userID string
	itemID string
}

func loadLiveEmbyEnv(t *testing.T) liveEmbyEnv {
	t.Helper()
	e := liveEmbyEnv{
		url:    os.Getenv("SW_LIVE_EMBY_URL"),
		apiKey: os.Getenv("SW_LIVE_EMBY_API_KEY"),
		userID: os.Getenv("SW_LIVE_EMBY_USER_ID"),
		itemID: os.Getenv("SW_LIVE_EMBY_ITEM_ID"),
	}
	if e.url == "" || e.apiKey == "" || e.userID == "" || e.itemID == "" {
		t.Skip("SW_LIVE_EMBY_URL / SW_LIVE_EMBY_API_KEY / SW_LIVE_EMBY_USER_ID / SW_LIVE_EMBY_ITEM_ID not all set; skipping live Emby backdrop-replace test")
	}
	return e
}

// TestLiveEmby_FanartSyncReplacesInPlace_DoesNotAppend is the AC-required
// live-server proof (#3125): it drives the REAL publisher code path
// (syncImageToPlatforms via SyncImageToPlatforms) against a real Emby
// server and asserts the backdrop COUNT is unchanged after a "replace",
// which a fake client cannot demonstrate -- the whole defect is a
// difference between two URL shapes that only a real server's semantics
// distinguish.
//
// Reproduces the issue's measured fingerprint in reverse: seeds two
// distinct backdrops (proving the starting count first, per the
// vacuous-precondition rule), then runs a fanart "replace" through
// SyncImageToPlatforms pointed at a local file holding a THIRD distinct
// image, and asserts:
//  1. BackdropCount is unchanged (2, not 3) -- this is the count assertion
//     the issue's AC requires against a real Emby.
//  2. Slot 0's content actually changed to the new bytes -- proving this
//     was a REPLACE, not a same-content no-op that would pass (1) for the
//     wrong reason.
//  3. Slot 1's content is untouched -- proving the replace targeted only
//     the primary slot, not a wholesale re-push.
func TestLiveEmby_FanartSyncReplacesInPlace_DoesNotAppend(t *testing.T) {
	env := loadLiveEmbyEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveBackdropTestTimeout)
	defer cancel()

	logger := silentLogger()
	client := emby.New(env.url, env.apiKey, env.userID, logger)

	// FIRST ACT: clear whatever the item already holds. The harness's UAT
	// server is not guaranteed to start this test at zero backdrops (a
	// prior run, another test, or manual poking can leave some behind), and
	// this test's precondition assertion below only means what it claims
	// when the item started clean. Registered as a t.Cleanup too, so the
	// test leaves the item exactly as it found it (empty) regardless of
	// pass/fail.
	clearAllBackdrops(t, ctx, env.itemID, client)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), liveBackdropTestTimeout)
		defer cleanupCancel()
		clearAllBackdrops(t, cleanupCtx, env.itemID, client)
	})

	// Seed two distinct backdrops directly via the indexed uploader (the
	// same call uploadFanartSet already uses, proven correct by the issue's
	// "SyncAllFanartToPlatforms produces zero duplicates" measurement) so
	// the test does not depend on the very code path it is about to test
	// for its setup. DECODABLE images (bandJPEG, phash_platform_test.go,
	// same package): #3125 F3 added a perceptual-hash resolution step to the
	// fanart sync, which now reads back and decodes every EXISTING backdrop
	// too (via matchingBackdropIndices), not just the new one being
	// uploaded. solidJPEG's minimal SOI+EOI marker is fine for upload
	// acceptance but is not decodable.
	seedA, seedB := bandJPEG(t, 0xA1), bandJPEG(t, 0xB2)
	if err := client.UploadImageAtIndex(ctx, env.itemID, "fanart", 0, seedA, "image/jpeg"); err != nil {
		t.Fatalf("seeding backdrop 0: %v", err)
	}
	if err := client.UploadImageAtIndex(ctx, env.itemID, "fanart", 1, seedB, "image/jpeg"); err != nil {
		t.Fatalf("seeding backdrop 1: %v", err)
	}

	// PRECONDITION: assert the starting count is exactly 2 before doing
	// anything else. A test that skipped this and only checked the count
	// after the replace would pass vacuously if the seed step silently
	// failed to actually add two backdrops.
	before, err := client.GetArtistDetail(ctx, env.itemID)
	if err != nil {
		t.Fatalf("reading state after seed: %v", err)
	}
	if before.BackdropCount != 2 {
		t.Fatalf("precondition failed: BackdropCount after seed = %d, want 2 (seed step did not establish the expected starting state)", before.BackdropCount)
	}
	beforeSlot0, _, err := client.GetArtistBackdrop(ctx, env.itemID, 0)
	if err != nil {
		t.Fatalf("reading seeded slot 0: %v", err)
	}
	if hashOf(beforeSlot0) != hashOf(seedA) {
		t.Fatalf("precondition failed: slot 0 does not hold the seeded content")
	}

	// Now drive the REAL production code path: write a new, third, distinct
	// image to a local temp dir and call SyncImageToPlatforms exactly as
	// the upload/fetch/crop handlers do for a fanart "replace". This is the
	// function under test for #3125.
	dir := t.TempDir()
	replacement := bandJPEG(t, 0xC3)
	if err := os.WriteFile(dir+"/fanart.jpg", replacement, 0o600); err != nil {
		t.Fatalf("writing local replacement fanart: %v", err)
	}

	p := New(Deps{
		Logger: logger,
		ArtistService: &fakePlatformLister{
			ids: []artist.PlatformID{
				{ArtistID: "live-a1", ConnectionID: "c-emby", PlatformArtistID: env.itemID},
			},
			// #3125 F3: resolveFanartReplaceTarget now needs the PREVIOUS
			// primary's stored phash to identify which platform index to
			// overwrite; without it (an artist whose provenance was never
			// recorded) it correctly refuses to guess and falls back to
			// append. This test is specifically about the REPLACE-IN-PLACE
			// behavior, so it must supply seedA's hash the same way
			// finalizeImageSave/recordImageProvenance would have stamped it
			// when seedA was originally saved as the primary.
			images: []artist.ArtistImage{
				{ImageType: "fanart", SlotIndex: 0, Exists: true, PHash: image.HashHex(phashOf(t, seedA))},
			},
		},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {ID: "c-emby", Name: "live-emby-uat", Type: connection.TypeEmby, URL: env.url, APIKey: env.apiKey, Enabled: true, Status: "ok", Emby: &connection.EmbyConfig{PlatformUserID: env.userID, FeatureImageWrite: true}},
		}},
	})

	art := &artist.Artist{ID: "live-a1", Name: "Live UAT Artist", Path: dir}
	warnings := p.SyncImageToPlatforms(ctx, art, "fanart")
	if len(warnings) != 0 {
		// Cross-check with a direct call to isolate a genuine upload failure
		// from a Publisher-plumbing issue (e.g. file discovery).
		directErr := client.UploadImageAtIndex(ctx, env.itemID, "fanart", 0, replacement, "image/jpeg")
		t.Fatalf("SyncImageToPlatforms returned warnings: %v (direct UploadImageAtIndex err=%v)", warnings, directErr)
	}

	// Give Emby a moment to settle the write before reading it back; the
	// UAT container has been observed to need a short beat after a POST
	// before GET reflects it (same settle rationale as reassertSettleDelay
	// elsewhere in this package, just applied to a read instead of a repair).
	time.Sleep(500 * time.Millisecond)

	after, err := client.GetArtistDetail(ctx, env.itemID)
	if err != nil {
		t.Fatalf("reading state after sync: %v", err)
	}

	// THE AC ASSERTION: count unchanged. Before the #3125 fix this fails --
	// the non-indexed POST appends a third backdrop, making the count 3.
	if after.BackdropCount != 2 {
		t.Errorf("BackdropCount after fanart sync = %d, want 2 (unchanged) -- a non-indexed upload APPENDS instead of replacing (#3125)", after.BackdropCount)
	}

	afterSlot0, _, err := client.GetArtistBackdrop(ctx, env.itemID, 0)
	if err != nil {
		t.Fatalf("reading slot 0 after sync: %v", err)
	}
	if hashOf(afterSlot0) != hashOf(replacement) {
		t.Errorf("slot 0 content did not change to the replacement bytes -- sync did not actually replace the primary backdrop")
	}

	afterSlot1, _, err := client.GetArtistBackdrop(ctx, env.itemID, 1)
	if err != nil {
		t.Fatalf("reading slot 1 after sync: %v", err)
	}
	if hashOf(afterSlot1) != hashOf(seedB) {
		t.Errorf("slot 1 content changed -- the fanart sync must touch only the primary slot (index 0)")
	}
}

// liveJellyfinEnv mirrors liveEmbyEnv. Jellyfin's non-indexed-POST append
// semantics were never separately measured on the wire (see the issue's
// verification-limits section); this test closes that gap when its env is
// set, reusing the identical assertions as the Emby test above since both
// platforms share the same UploadImageRaw/UploadImageAtIndexRaw client code
// (internal/connection/mediabrowser/image_writers.go).
type liveJellyfinEnv struct {
	url    string
	apiKey string
	userID string
	itemID string
}

func loadLiveJellyfinEnv(t *testing.T) liveJellyfinEnv {
	t.Helper()
	e := liveJellyfinEnv{
		url:    os.Getenv("SW_LIVE_JELLYFIN_URL"),
		apiKey: os.Getenv("SW_LIVE_JELLYFIN_API_KEY"),
		userID: os.Getenv("SW_LIVE_JELLYFIN_USER_ID"),
		itemID: os.Getenv("SW_LIVE_JELLYFIN_ITEM_ID"),
	}
	if e.url == "" || e.apiKey == "" || e.userID == "" || e.itemID == "" {
		t.Skip("SW_LIVE_JELLYFIN_URL / SW_LIVE_JELLYFIN_API_KEY / SW_LIVE_JELLYFIN_USER_ID / SW_LIVE_JELLYFIN_ITEM_ID not all set; skipping live Jellyfin backdrop-replace test")
	}
	return e
}

// TestLiveJellyfin_NonIndexedUploadAppends closes the issue's stated
// verification gap: "Jellyfin's non-indexed POST was not separately tested
// for append" (assumed identical to Emby because both share
// UploadImageRaw). MEASURED, not assumed: on a real Jellyfin 10.11.10, the
// non-indexed POST /Items/{id}/Images/Backdrop does append, matching Emby.
func TestLiveJellyfin_NonIndexedUploadAppends(t *testing.T) {
	env := loadLiveJellyfinEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveBackdropTestTimeout)
	defer cancel()

	client := jellyfin.New(env.url, env.apiKey, env.userID, silentLogger())

	// FIRST ACT: clear whatever the item already holds; see the identical
	// comment on the Emby test above for why this is required rather than
	// assuming the item starts at zero. Doubly necessary here: Jellyfin
	// APPENDS on this test's own upload calls, so a second run against an
	// uncleared item compounds instead of merely being lucky.
	clearAllBackdrops(t, ctx, env.itemID, client)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), liveBackdropTestTimeout)
		defer cleanupCancel()
		clearAllBackdrops(t, cleanupCtx, env.itemID, client)
	})

	seed := solidJPEG(0xD4)
	if err := client.UploadImageAtIndex(ctx, env.itemID, "fanart", 0, seed, "image/jpeg"); err != nil {
		t.Fatalf("seeding backdrop 0: %v", err)
	}
	before, err := client.GetArtistDetail(ctx, env.itemID)
	if err != nil {
		t.Fatalf("reading state after seed: %v", err)
	}
	if before.BackdropCount != 1 {
		t.Fatalf("precondition failed: BackdropCount after seed = %d, want 1", before.BackdropCount)
	}

	// The raw client method, not routed through SyncImageToPlatforms: this
	// test measures PLATFORM behavior for the non-indexed upload shape
	// specifically, independent of which shape this repo's code currently
	// issues (that question is answered by the unit-level path tests).
	if err := client.UploadImage(ctx, env.itemID, "fanart", solidJPEG(0xE5), "image/jpeg"); err != nil {
		t.Fatalf("non-indexed upload: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	after, err := client.GetArtistDetail(ctx, env.itemID)
	if err != nil {
		t.Fatalf("reading state after non-indexed upload: %v", err)
	}
	if after.BackdropCount != 2 {
		t.Errorf("BackdropCount after non-indexed upload = %d, want 2 (APPENDED) -- if this ever reads 1, Jellyfin's non-indexed semantics changed and the #3125 analysis needs revisiting", after.BackdropCount)
	}
}

// TestLiveJellyfin_IndexedUploadAlsoAppends_KnownGap is a NEW finding made
// while implementing #3125, not something the issue anticipated: on a real
// Jellyfin 10.11.10, POST /Items/{id}/Images/Backdrop/{index} does NOT honor
// the URL index for placement the way Emby's identical-looking endpoint
// does. Measured directly: seeding one backdrop at index 0, then POSTing
// AGAIN to index 0 (an in-range, already-occupied index) leaves the
// original content at index 0 untouched and adds a SECOND backdrop at index
// 1 -- i.e. Jellyfin's indexed endpoint appends exactly like its non-indexed
// one, ignoring the index entirely (confirmed further: POSTing to an
// out-of-range index like 99 against zero existing backdrops still lands
// the upload at index 0, not 99).
//
// CONSEQUENCE FOR #3125's FIX: routing the fanart sync through
// UploadImageAtIndex(..., 0, ...) (this PR's change) is a NO-OP on Jellyfin
// specifically -- both the old non-indexed call and the new indexed call
// append there, so Jellyfin's duplication is UNCHANGED by this PR (not
// fixed, but not made worse either; see the publisher.go comment at the
// #3125 branch for the placement-recovery analysis). This test documents
// that pre-existing, still-open gap so it is not silently rediscovered:
// TRACKED FOR A FOLLOW-UP, not fixed here (the only correct fix -- delete
// every backdrop and re-upload the full ordered set -- is the
// syncAllFanart-routing alternative the #3125 issue explicitly scoped OUT
// of this branch). A red failure here is GOOD NEWS: it means Jellyfin's
// indexed endpoint started honoring the index, and this test (and the
// #3125 follow-up) should be updated/closed accordingly.
func TestLiveJellyfin_IndexedUploadAlsoAppends_KnownGap(t *testing.T) {
	env := loadLiveJellyfinEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveBackdropTestTimeout)
	defer cancel()

	client := jellyfin.New(env.url, env.apiKey, env.userID, silentLogger())

	// FIRST ACT: clear whatever the item already holds; see the identical
	// comment on TestLiveEmby_FanartSyncReplacesInPlace_DoesNotAppend above.
	// This test in particular used to fail permanently after its own first
	// run: it never removed what it seeded, and this platform appends, so
	// every subsequent run started from an already-dirty count.
	clearAllBackdrops(t, ctx, env.itemID, client)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), liveBackdropTestTimeout)
		defer cleanupCancel()
		clearAllBackdrops(t, cleanupCtx, env.itemID, client)
	})

	seed := solidJPEG(0xA1)
	if err := client.UploadImageAtIndex(ctx, env.itemID, "fanart", 0, seed, "image/jpeg"); err != nil {
		t.Fatalf("seeding backdrop 0: %v", err)
	}
	before, err := client.GetArtistDetail(ctx, env.itemID)
	if err != nil {
		t.Fatalf("reading state after seed: %v", err)
	}
	if before.BackdropCount != 1 {
		t.Fatalf("precondition failed: BackdropCount after seed = %d, want 1", before.BackdropCount)
	}

	// Re-upload to the SAME index the seed just occupied -- the in-place
	// replace this PR's fix relies on for Emby.
	replacement := solidJPEG(0xB2)
	if err := client.UploadImageAtIndex(ctx, env.itemID, "fanart", 0, replacement, "image/jpeg"); err != nil {
		t.Fatalf("indexed re-upload to index 0: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	after, err := client.GetArtistDetail(ctx, env.itemID)
	if err != nil {
		t.Fatalf("reading state after indexed re-upload: %v", err)
	}
	if after.BackdropCount != 2 {
		t.Errorf("KNOWN GAP no longer reproduces (BackdropCount = %d, want the documented-append value of 2) -- if Jellyfin now replaces in place, update the #3125 fix to stop treating Jellyfin as unfixed and close #3135", after.BackdropCount)
	}
}

// TestLiveEmby_F3_BystanderSurvivesAfterPlatformSideDelete reproduces the
// EXACT sequence the #3125 review round 1 F3 finding describes, against a
// real Emby 4.9.5.0: seed three distinct backdrops, delete index 0 (as
// deletePollutedBackdrops / PrunePlatformBackdropDuplicates / an operator
// deleting in the Emby UI all can), let the peer re-index the survivors,
// then run a REAL fanart sync (SyncImageToPlatforms, the exact code path
// #3125 patches) and assert the SECOND image's backdrop survives untouched
// -- rather than a naive unconditional "write index 0" destroying it.
//
// This drives the full production stack, not just the resolver: the
// Publisher is wired with a fakePlatformLister whose GetImagesForArtist
// returns the previous primary's stored phash, exactly as
// previousFanartPrimaryPHash reads it from real artist_images provenance.
func TestLiveEmby_F3_BystanderSurvivesAfterPlatformSideDelete(t *testing.T) {
	env := loadLiveEmbyEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveBackdropTestTimeout)
	defer cancel()

	logger := silentLogger()
	client := emby.New(env.url, env.apiKey, env.userID, logger)

	// FIRST ACT: clear whatever the item already holds; see the identical
	// comment on the other live tests in this file.
	clearAllBackdrops(t, ctx, env.itemID, client)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), liveBackdropTestTimeout)
		defer cleanupCancel()
		clearAllBackdrops(t, cleanupCtx, env.itemID, client)
	})

	// Seed idx0=oldPrimary, idx1=bystander, idx2=third -- three DISTINCT,
	// fully DECODABLE images (bandJPEG from phash_platform_test.go, same
	// package): this test needs an actual perceptual hash for the
	// provenance stamp below, which solidJPEG's minimal SOI+EOI marker
	// (fine for upload, not decodable) cannot provide.
	oldPrimary := bandJPEG(t, 0xC1)
	bystander := bandJPEG(t, 0xC2)
	third := bandJPEG(t, 0xC3)
	if err := client.UploadImageAtIndex(ctx, env.itemID, "fanart", 0, oldPrimary, "image/jpeg"); err != nil {
		t.Fatalf("seeding backdrop 0: %v", err)
	}
	if err := client.UploadImageAtIndex(ctx, env.itemID, "fanart", 1, bystander, "image/jpeg"); err != nil {
		t.Fatalf("seeding backdrop 1: %v", err)
	}
	if err := client.UploadImageAtIndex(ctx, env.itemID, "fanart", 2, third, "image/jpeg"); err != nil {
		t.Fatalf("seeding backdrop 2: %v", err)
	}

	// PRECONDITION: exactly 3 backdrops before the delete.
	seeded, err := client.GetArtistDetail(ctx, env.itemID)
	if err != nil {
		t.Fatalf("reading state after seed: %v", err)
	}
	if seeded.BackdropCount != 3 {
		t.Fatalf("precondition failed: BackdropCount after seed = %d, want 3", seeded.BackdropCount)
	}

	// Something deletes index 0 (phash back-out, remote prune, or an
	// operator) -- the peer re-indexes survivors DOWN BY ONE, so the
	// bystander that was at index 1 is now at index 0.
	if err := client.DeleteImageAtIndex(ctx, env.itemID, "fanart", 0); err != nil {
		t.Fatalf("deleting backdrop 0: %v", err)
	}

	// PRECONDITION: the peer re-indexed as expected. Assert this BEFORE
	// trusting the rest of the test, or a peer that behaved differently
	// would make the final assertion pass for the wrong reason.
	afterDelete, err := client.GetArtistDetail(ctx, env.itemID)
	if err != nil {
		t.Fatalf("reading state after delete: %v", err)
	}
	if afterDelete.BackdropCount != 2 {
		t.Fatalf("precondition failed: BackdropCount after delete = %d, want 2", afterDelete.BackdropCount)
	}
	bystanderNowAt0, _, err := client.GetArtistBackdrop(ctx, env.itemID, 0)
	if err != nil {
		t.Fatalf("reading backdrop 0 after delete: %v", err)
	}
	if hashOf(bystanderNowAt0) != hashOf(bystander) {
		t.Fatalf("precondition failed: index 0 does not hold the bystander after the delete+reindex -- the setup does not model the #3125 F3 scenario")
	}

	// Now run the REAL fanart sync: a new local primary, with the artist's
	// PREVIOUS primary phash recorded in provenance exactly as
	// finalizeImageSave/recordImageProvenance would have stamped it at the
	// time oldPrimary was saved.
	dir := t.TempDir()
	newPrimary := bandJPEG(t, 0xC4)
	if err := os.WriteFile(dir+"/fanart.jpg", newPrimary, 0o600); err != nil {
		t.Fatalf("writing local replacement fanart: %v", err)
	}
	previousHash := image.HashHex(phashOf(t, oldPrimary))

	p := New(Deps{
		Logger: logger,
		ArtistService: &fakePlatformLister{
			ids: []artist.PlatformID{
				{ArtistID: "live-f3", ConnectionID: "c-emby", PlatformArtistID: env.itemID},
			},
			images: []artist.ArtistImage{
				{ImageType: "fanart", SlotIndex: 0, Exists: true, PHash: previousHash},
			},
		},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {ID: "c-emby", Name: "live-emby-uat", Type: connection.TypeEmby, URL: env.url, APIKey: env.apiKey, Enabled: true, Status: "ok", Emby: &connection.EmbyConfig{PlatformUserID: env.userID, FeatureImageWrite: true}},
		}},
	})

	art := &artist.Artist{ID: "live-f3", Name: "Live UAT Artist", Path: dir}
	warnings := p.SyncImageToPlatforms(ctx, art, "fanart")
	if len(warnings) != 0 {
		t.Fatalf("SyncImageToPlatforms returned warnings: %v", warnings)
	}

	time.Sleep(500 * time.Millisecond)

	// THE F3 ASSERTION: the bystander (now at index 0 before the sync) must
	// SURVIVE. A blind "write index 0" would have destroyed it here.
	afterSync0, _, err := client.GetArtistBackdrop(ctx, env.itemID, 0)
	if err != nil {
		t.Fatalf("reading backdrop 0 after sync: %v", err)
	}
	if hashOf(afterSync0) == hashOf(newPrimary) {
		t.Fatal("the bystander at index 0 was DESTROYED by the fanart sync -- F3 guard did not fire")
	}
	if hashOf(afterSync0) != hashOf(bystander) {
		t.Errorf("index 0 content changed to something other than the bystander or the new primary; got an unexpected byte pattern")
	}

	// The new primary must have landed SOMEWHERE (index 1, where the old
	// primary was resolved to after the reindex).
	final, err := client.GetArtistDetail(ctx, env.itemID)
	if err != nil {
		t.Fatalf("reading final state: %v", err)
	}
	foundNewPrimary := false
	for i := 0; i < final.BackdropCount; i++ {
		data, _, gErr := client.GetArtistBackdrop(ctx, env.itemID, i)
		if gErr != nil {
			continue
		}
		if hashOf(data) == hashOf(newPrimary) {
			foundNewPrimary = true
			break
		}
	}
	if !foundNewPrimary {
		t.Error("the new primary was not found at any backdrop index after the sync")
	}
}
