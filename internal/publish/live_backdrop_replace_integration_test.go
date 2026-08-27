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

// liveBackdropTestTimeout bounds an ENTIRE test's main context -- every
// test in this file builds ONE ctx with this timeout via
// context.WithTimeout and reuses it across its whole body, so this is a
// whole-test budget, not a per-call one, despite an earlier version of
// this comment claiming otherwise (CR review round). The busiest test here
// (TestLiveEmby_F3_BystanderSurvivesAfterPlatformSideDelete) spends this
// single budget on roughly twenty sequential round trips plus a 500ms
// settle sleep; 60s leaves real headroom for that on a loaded UAT
// container without making a genuine hang take unreasonably long to
// surface. A stalled/unreachable server still fails fast: the FIRST call in
// each test (clearAllBackdrops or the initial upload) hits the platform
// immediately, so a truly dead peer is caught well before the budget
// expires -- this timeout exists to bound total elapsed time across many
// calls, not to detect an immediately-refused connection.
const liveBackdropTestTimeout = 60 * time.Second

// liveBackdropCleanupTimeout bounds each test's t.Cleanup teardown
// (clearAllBackdrops again, against a fresh context since the main ctx may
// already be near its own deadline by cleanup time). Cleanup issues only a
// handful of calls -- far fewer than a test body -- so it gets its own
// short budget rather than sharing liveBackdropTestTimeout's larger one.
const liveBackdropCleanupTimeout = 15 * time.Second

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
//
// t.Errorf, NEVER t.Fatalf (round-2 S1 fix): this is called from inside a
// t.Cleanup closure at four call sites, and Fatalf calls runtime.Goexit,
// which stops the CLEANUP itself mid-run -- exactly the failure this
// function exists to prevent, since it would leave the item dirty for the
// next test/run. Errorf records the failure and lets execution continue, so
// a partial clear still attempts every remaining delete rather than
// abandoning the item after the first one that fails.
func clearAllBackdrops(ctx context.Context, t *testing.T, itemID string, client backdropClearer) {
	t.Helper()
	state, err := client.GetArtistDetail(ctx, itemID)
	if err != nil {
		t.Errorf("clearAllBackdrops: reading current state: %v", err)
		return
	}
	for i := state.BackdropCount - 1; i >= 0; i-- {
		if err := client.DeleteImageAtIndex(ctx, itemID, "fanart", i); err != nil {
			t.Errorf("clearAllBackdrops: deleting backdrop %d: %v", i, err)
		}
	}
	// Re-verify rather than trust the loop: the count read above could
	// itself be stale, or a delete could silently no-op on some peer.
	after, err := client.GetArtistDetail(ctx, itemID)
	if err != nil {
		t.Errorf("clearAllBackdrops: reading state after clear: %v", err)
		return
	}
	if after.BackdropCount != 0 {
		t.Errorf("clearAllBackdrops: BackdropCount = %d after clearing, want 0 (item was not fully cleared)", after.BackdropCount)
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
	clearAllBackdrops(ctx, t, env.itemID, client)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), liveBackdropCleanupTimeout)
		defer cleanupCancel()
		clearAllBackdrops(cleanupCtx, t, env.itemID, client)
	})

	// Seed two distinct backdrops directly via the indexed uploader (the
	// same call uploadFanartSet already uses, proven correct by the issue's
	// "SyncAllFanartToPlatforms produces zero duplicates" measurement) so
	// the test does not depend on the very code path it is about to test
	// for its setup. bandJPEG (phash_platform_test.go, same package), for
	// consistency with the other Emby tests below that also route through
	// SyncImageToPlatforms and image.BackupSlot -- NOT because solidJPEG
	// (defined above in this file) could not serve: solidJPEG(fill) is
	// ALSO byte-distinct per its fill argument, and would satisfy hashOf's
	// need to tell slots apart just as well. bandJPEG's actual difference
	// from solidJPEG is that it is a genuine, decodable raster (verified:
	// image/jpeg's own Decode succeeds on bandJPEG's output and fails with
	// "missing SOS marker" on solidJPEG's; Emby's own resize endpoint
	// actually reprocesses a bandJPEG image but returns solidJPEG's bytes
	// unchanged) -- a more faithful stand-in for real on-disk operator
	// artwork on the tests that simulate the actual save-then-sync
	// sequence. (CR review round: an earlier version of this comment
	// wrongly claimed solidJPEG was not byte-distinguishable -- checked
	// directly against solidJPEG's source below and that claim does not
	// hold; this restates only what is actually true of each fixture.)
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

	// #3125 C1: the resolver's previous-primary identity now comes from the
	// ON-DISK BACKUP (image.ReadSlotBackup), never from the database -- a
	// round-1 review found the DB-hash design inert in production, because
	// the DB row is stamped from the NEW file before sync ever runs. So
	// this test simulates the REAL save-then-sync ordering: write seedA as
	// the current primary, take its backup (BackupSlot, exactly what
	// SaveSlotProtected does before a destructive fanart overwrite), THEN
	// overwrite fanart.jpg with the replacement -- the same sequence
	// finalizeImageSave's caller performs before ever reaching
	// SyncImageToPlatforms.
	if err := os.WriteFile(dir+"/fanart.jpg", seedA, 0o600); err != nil {
		t.Fatalf("writing current primary before backup: %v", err)
	}
	if err := image.BackupSlot(ctx, dir, "fanart", "fanart.jpg"); err != nil {
		t.Fatalf("backing up current primary: %v", err)
	}
	replacement := bandJPEG(t, 0xC3)
	if err := os.WriteFile(dir+"/fanart.jpg", replacement, 0o600); err != nil {
		t.Fatalf("writing local replacement fanart: %v", err)
	}

	p := New(Deps{
		Logger: logger,
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "live-a1", ConnectionID: "c-emby", PlatformArtistID: env.itemID},
		}},
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
	clearAllBackdrops(ctx, t, env.itemID, client)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), liveBackdropCleanupTimeout)
		defer cleanupCancel()
		clearAllBackdrops(cleanupCtx, t, env.itemID, client)
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
	clearAllBackdrops(ctx, t, env.itemID, client)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), liveBackdropCleanupTimeout)
		defer cleanupCancel()
		clearAllBackdrops(cleanupCtx, t, env.itemID, client)
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
// previous primary's ON-DISK BACKUP is populated exactly as
// SaveSlotProtected/BackupSlot would leave it, and previousFanartPrimaryData
// reads it from there (#3125 C1) -- never from artist_images, which round 1
// found stamped with the NEW file's hash before sync ever runs.
func TestLiveEmby_F3_BystanderSurvivesAfterPlatformSideDelete(t *testing.T) {
	env := loadLiveEmbyEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveBackdropTestTimeout)
	defer cancel()

	logger := silentLogger()
	client := emby.New(env.url, env.apiKey, env.userID, logger)

	// FIRST ACT: clear whatever the item already holds; see the identical
	// comment on the other live tests in this file.
	clearAllBackdrops(ctx, t, env.itemID, client)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), liveBackdropCleanupTimeout)
		defer cleanupCancel()
		clearAllBackdrops(cleanupCtx, t, env.itemID, client)
	})

	// Seed idx0=oldPrimary, idx1=bystander, idx2=third -- three BYTE-DISTINCT
	// images (bandJPEG from phash_platform_test.go, same package). Both
	// bandJPEG and solidJPEG (this file) produce byte-distinct output per
	// their seed/fill argument, so either would satisfy hashOf's need to
	// tell these three apart. bandJPEG is used here because this test
	// exercises image.BackupSlot on a local file exactly as
	// SaveSlotProtected would (see the comment further down): BackupSlot's
	// only requirement on the bytes it backs up is that FindExistingImage
	// finds the file, which neither fixture's byte content affects, but
	// bandJPEG produces a genuinely decodable raster where solidJPEG's
	// minimal SOI+EOI marker does not (verified: image/jpeg.Decode
	// succeeds on bandJPEG's output, and fails with "missing SOS marker" on
	// solidJPEG's) -- a closer stand-in for real on-disk operator artwork
	// on a test that models the real save path. (CR review round: an
	// earlier version of this comment invented a decode requirement on the
	// resolveFanartReplaceTarget side, which compares raw bytes via
	// image.ContentHash and never decodes; that claim was wrong and is
	// removed rather than replaced with another guess.)
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

	// Now run the REAL fanart sync: a new local primary, with the previous
	// primary's ON-DISK BACKUP present exactly as SaveSlotProtected would
	// have left it (#3125 C1: the resolver reads image.ReadSlotBackup, never
	// the database, since the DB row is stamped from the NEW file by the
	// same request before sync ever runs).
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/fanart.jpg", oldPrimary, 0o600); err != nil {
		t.Fatalf("writing current primary before backup: %v", err)
	}
	if err := image.BackupSlot(ctx, dir, "fanart", "fanart.jpg"); err != nil {
		t.Fatalf("backing up current primary: %v", err)
	}
	newPrimary := bandJPEG(t, 0xC4)
	if err := os.WriteFile(dir+"/fanart.jpg", newPrimary, 0o600); err != nil {
		t.Fatalf("writing local replacement fanart: %v", err)
	}

	p := New(Deps{
		Logger: logger,
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "live-f3", ConnectionID: "c-emby", PlatformArtistID: env.itemID},
		}},
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

	// The new primary must have landed SOMEWHERE -- via APPEND, not an
	// indexed replace. CR review round: an earlier version of this comment
	// claimed the new primary lands "at index 1, where the old primary was
	// resolved to after the reindex" -- that is wrong. Tracing the actual
	// state: DeleteImageAtIndex(0) removes oldPrimary OUTRIGHT (this test
	// deletes the PRIMARY itself, not a lower bystander), so after the
	// reindex oldPrimary is not present at ANY index -- only the bystander
	// (now at 0) and third (now at 1) remain. previousFanartPrimaryData
	// still returns oldPrimary's bytes from the backup, but writeTarget
	// finds no match for them anywhere on the platform, so
	// resolveFanartReplaceTarget falls through to fanartTargetAppend and
	// newPrimary lands at index 2 by append -- no indexed replace occurs
	// in this scenario. The bystander-survival assertion above is still
	// exactly right (an unconditional index-0 write would have destroyed
	// it); this comment only corrects WHICH outcome produced that survival.
	// See TestLiveEmby_IndexedReplace_FindsPrimaryShiftedByLowerBystanderDelete
	// below for live coverage of the positive-identification (outcome 2b)
	// case this test does not exercise.
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

// TestLiveEmby_IndexedReplace_FindsPrimaryShiftedByLowerBystanderDelete is
// the CR review round's finding 4 addition: real live coverage of
// resolveFanartReplaceTarget's outcome 2b (POSITIVE identification of the
// previous primary at a platform-shifted index), which
// TestLiveEmby_F3_BystanderSurvivesAfterPlatformSideDelete does NOT
// exercise -- that test deletes the primary itself, so its previous-primary
// bytes are gone from the platform entirely and the resolver falls through
// to APPEND. This is the branch's central claim (an INDEXED, in-place
// replace happens when the previous primary can be positively located,
// even after its index has shifted) and until this test it had no live
// evidence.
//
// Seeds FOUR distinct backdrops: idx0=bystanderKept (survives untouched),
// idx1=lowerVictim (deleted, to shift everything after it down by one),
// idx2=oldPrimary, idx3=third. Deleting idx1 -- a LOWER index than
// oldPrimary's original slot -- shifts oldPrimary from index 2 down to
// index 1 and third from 3 down to 2, while bystanderKept at index 0 is
// completely unaffected (nothing lower than it was touched). This is
// exactly the "index shifted but the previous primary is still present"
// shape outcome 2b exists to handle, distinct from F3's "index shifted
// because the previous primary itself is gone" shape.
func TestLiveEmby_IndexedReplace_FindsPrimaryShiftedByLowerBystanderDelete(t *testing.T) {
	env := loadLiveEmbyEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveBackdropTestTimeout)
	defer cancel()

	logger := silentLogger()
	client := emby.New(env.url, env.apiKey, env.userID, logger)

	clearAllBackdrops(ctx, t, env.itemID, client)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), liveBackdropCleanupTimeout)
		defer cleanupCancel()
		clearAllBackdrops(cleanupCtx, t, env.itemID, client)
	})

	bystanderKept := bandJPEG(t, 0xD1)
	lowerVictim := bandJPEG(t, 0xD2)
	oldPrimary := bandJPEG(t, 0xD3)
	third := bandJPEG(t, 0xD4)
	if err := client.UploadImageAtIndex(ctx, env.itemID, "fanart", 0, bystanderKept, "image/jpeg"); err != nil {
		t.Fatalf("seeding backdrop 0: %v", err)
	}
	if err := client.UploadImageAtIndex(ctx, env.itemID, "fanart", 1, lowerVictim, "image/jpeg"); err != nil {
		t.Fatalf("seeding backdrop 1: %v", err)
	}
	if err := client.UploadImageAtIndex(ctx, env.itemID, "fanart", 2, oldPrimary, "image/jpeg"); err != nil {
		t.Fatalf("seeding backdrop 2: %v", err)
	}
	if err := client.UploadImageAtIndex(ctx, env.itemID, "fanart", 3, third, "image/jpeg"); err != nil {
		t.Fatalf("seeding backdrop 3: %v", err)
	}

	seeded, err := client.GetArtistDetail(ctx, env.itemID)
	if err != nil {
		t.Fatalf("reading state after seed: %v", err)
	}
	if seeded.BackdropCount != 4 {
		t.Fatalf("precondition failed: BackdropCount after seed = %d, want 4", seeded.BackdropCount)
	}

	// Delete index 1 (lowerVictim) -- BELOW oldPrimary's original index 2 --
	// so the peer reindexes oldPrimary down to 1 and third down to 2, while
	// bystanderKept at index 0 is untouched.
	if err := client.DeleteImageAtIndex(ctx, env.itemID, "fanart", 1); err != nil {
		t.Fatalf("deleting backdrop 1: %v", err)
	}

	// PRECONDITION: the peer reindexed exactly as expected before trusting
	// the rest of the test.
	afterDelete, err := client.GetArtistDetail(ctx, env.itemID)
	if err != nil {
		t.Fatalf("reading state after delete: %v", err)
	}
	if afterDelete.BackdropCount != 3 {
		t.Fatalf("precondition failed: BackdropCount after delete = %d, want 3", afterDelete.BackdropCount)
	}
	at0, _, err := client.GetArtistBackdrop(ctx, env.itemID, 0)
	if err != nil {
		t.Fatalf("reading backdrop 0 after delete: %v", err)
	}
	if hashOf(at0) != hashOf(bystanderKept) {
		t.Fatalf("precondition failed: index 0 does not hold bystanderKept after the delete+reindex")
	}
	oldPrimaryShiftedTo1, _, err := client.GetArtistBackdrop(ctx, env.itemID, 1)
	if err != nil {
		t.Fatalf("reading backdrop 1 after delete: %v", err)
	}
	if hashOf(oldPrimaryShiftedTo1) != hashOf(oldPrimary) {
		t.Fatalf("precondition failed: index 1 does not hold oldPrimary after the delete+reindex -- the setup does not model a shifted-but-present previous primary")
	}
	at2, _, err := client.GetArtistBackdrop(ctx, env.itemID, 2)
	if err != nil {
		t.Fatalf("reading backdrop 2 after delete: %v", err)
	}
	if hashOf(at2) != hashOf(third) {
		t.Fatalf("precondition failed: index 2 does not hold third after the delete+reindex")
	}

	// Run the REAL fanart sync: new local primary, previous primary's
	// on-disk backup present exactly as SaveSlotProtected would leave it.
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/fanart.jpg", oldPrimary, 0o600); err != nil {
		t.Fatalf("writing current primary before backup: %v", err)
	}
	if err := image.BackupSlot(ctx, dir, "fanart", "fanart.jpg"); err != nil {
		t.Fatalf("backing up current primary: %v", err)
	}
	newPrimary := bandJPEG(t, 0xD5)
	if err := os.WriteFile(dir+"/fanart.jpg", newPrimary, 0o600); err != nil {
		t.Fatalf("writing local replacement fanart: %v", err)
	}

	p := New(Deps{
		Logger: logger,
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "live-2b", ConnectionID: "c-emby", PlatformArtistID: env.itemID},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {ID: "c-emby", Name: "live-emby-uat", Type: connection.TypeEmby, URL: env.url, APIKey: env.apiKey, Enabled: true, Status: "ok", Emby: &connection.EmbyConfig{PlatformUserID: env.userID, FeatureImageWrite: true}},
		}},
	})

	art := &artist.Artist{ID: "live-2b", Name: "Live UAT Artist", Path: dir}
	warnings := p.SyncImageToPlatforms(ctx, art, "fanart")
	if len(warnings) != 0 {
		t.Fatalf("SyncImageToPlatforms returned warnings: %v", warnings)
	}

	time.Sleep(500 * time.Millisecond)

	// THE OUTCOME-2B ASSERTION: this must be an INDEXED, in-place replace,
	// not an append -- the count must stay at 3, never grow to 4. This is
	// the strongest possible proof that writeTarget positively identified
	// oldPrimary at its SHIFTED index (1) rather than falling back to
	// append: an append would show up as a fourth backdrop.
	final, err := client.GetArtistDetail(ctx, env.itemID)
	if err != nil {
		t.Fatalf("reading final state: %v", err)
	}
	if final.BackdropCount != 3 {
		t.Fatalf("BackdropCount after sync = %d, want 3 (an indexed replace, not an append) -- outcome 2b did not fire", final.BackdropCount)
	}

	// bystanderKept at index 0 must be completely untouched.
	finalAt0, _, err := client.GetArtistBackdrop(ctx, env.itemID, 0)
	if err != nil {
		t.Fatalf("reading backdrop 0 after sync: %v", err)
	}
	if hashOf(finalAt0) != hashOf(bystanderKept) {
		t.Error("index 0 (bystanderKept) was altered by the sync -- it must never be touched")
	}

	// Index 1 -- where oldPrimary was shifted to -- must now hold the NEW
	// primary: the whole point of outcome 2b is that this write landed
	// exactly there, positively identified, not guessed.
	finalAt1, _, err := client.GetArtistBackdrop(ctx, env.itemID, 1)
	if err != nil {
		t.Fatalf("reading backdrop 1 after sync: %v", err)
	}
	if hashOf(finalAt1) != hashOf(newPrimary) {
		t.Error("index 1 (where oldPrimary was shifted to) does not hold the new primary -- outcome 2b did not correctly resolve the shifted slot")
	}

	// third at index 2 must be completely untouched.
	finalAt2, _, err := client.GetArtistBackdrop(ctx, env.itemID, 2)
	if err != nil {
		t.Fatalf("reading backdrop 2 after sync: %v", err)
	}
	if hashOf(finalAt2) != hashOf(third) {
		t.Error("index 2 (third) was altered by the sync -- it must never be touched")
	}
}
