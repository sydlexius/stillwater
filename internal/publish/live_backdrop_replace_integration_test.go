//go:build integration

package publish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
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

// TestLiveJellyfin_IndexedUploadStillIgnoresIndex_WorkedAroundNotFixed is a
// NEW finding made while implementing #3125, not something the issue
// anticipated: on a real Jellyfin 10.11.10, POST
// /Items/{id}/Images/Backdrop/{index} does NOT honor the URL index for
// placement the way Emby's identical-looking endpoint does. Measured
// directly: seeding one backdrop at index 0, then POSTing AGAIN to index 0
// (an in-range, already-occupied index) leaves the original content at
// index 0 untouched and adds a SECOND backdrop at index 1 -- i.e.
// Jellyfin's indexed endpoint appends exactly like its non-indexed one,
// ignoring the index entirely (confirmed further: POSTing to an
// out-of-range index like 99 against zero existing backdrops still lands
// the upload at index 0, not 99).
//
// #3135 UPDATE: this raw wire behavior is UNCHANGED and is not expected to
// change -- it is Jellyfin's own server bug, not something Stillwater's
// client code can fix. What #3135 fixed is that
// uploadFanartForSync/uploadFanartFullResyncForSync no longer RELIES on this
// endpoint honoring the index for a Jellyfin connection:
// connection.SupportsIndexedBackdropReplace(TypeJellyfin) reports false, so
// the publisher routes a Jellyfin fanart replace through delete-every-
// backdrop-then-reupload-the-full-set instead of a single call to this
// endpoint. This test is KEPT, RENAMED, AND REPOINTED rather than removed:
// it still directly measures the platform quirk that motivates the
// #3135 routing decision, and remains a useful live canary -- if a future
// Jellyfin release starts honoring the index (this test would then fail,
// which is GOOD NEWS), connection.SupportsIndexedBackdropReplace could be
// flipped to true for Jellyfin and the resync workaround retired as
// unnecessary. See TestLiveJellyfin_FanartSyncFullResync_ReplacesWithoutDuplicating
// below for live proof that the HIGHER-LEVEL defect (a fanart replace
// duplicating backdrops on Jellyfin) is actually fixed despite this
// lower-level quirk persisting.
func TestLiveJellyfin_IndexedUploadStillIgnoresIndex_WorkedAroundNotFixed(t *testing.T) {
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
		t.Errorf("Jellyfin's raw indexed-upload quirk no longer reproduces (BackdropCount = %d, want the documented-append value of 2) -- if Jellyfin now replaces in place at the wire level, connection.SupportsIndexedBackdropReplace could report true for TypeJellyfin and the #3135 resync workaround could be retired as unnecessary", after.BackdropCount)
	}
}

// TestLiveJellyfin_FanartSyncFullResync_ReplacesWithoutDuplicating is #3135's
// AC-required live-server proof: it drives the REAL publisher code path
// (syncImageToPlatforms via SyncImageToPlatforms, exactly as an operator's
// "replace this artist's fanart" action does) against a real Jellyfin server
// and asserts the backdrop COUNT is unchanged after the replace, and that
// the primary slot's CONTENT actually changed -- the same two-part proof
// TestLiveEmby_FanartSyncReplacesInPlace_DoesNotAppend uses for Emby. A fake
// client cannot demonstrate this: the whole #3135 defect is that Jellyfin's
// indexed-upload endpoint (proven still broken by the sibling KnownGap-style
// test above) silently ignores the index, so only a real server's actual
// response to the DELETE+POST sequence proves the workaround functions.
//
// Seeds TWO distinct local fanart files (fanart.jpg index 0, fanart2.jpg
// index 1) and syncs them via SyncAllFanartToPlatforms first, establishing a
// real starting platform state exactly the way an operator's library would
// arrive at one (not a synthetic UploadImageAtIndex seed, since #3135's own
// fix no longer trusts that endpoint for Jellyfin). Then overwrites the
// local primary with a THIRD distinct image and runs SyncImageToPlatforms
// (a single-image "replace" sync, the exact operation #3135 is about) and
// asserts:
//  1. BackdropCount is unchanged (2, not 3) -- the resync cleared and
//     rebuilt the set rather than appending a duplicate.
//  2. Slot 0 holds the new primary's bytes -- proving the replace actually
//     landed, not merely that the count happened to come out right.
//  3. Slot 1 still holds the untouched second backdrop -- proving the full
//     local set was faithfully rebuilt, not just the primary.
func TestLiveJellyfin_FanartSyncFullResync_ReplacesWithoutDuplicating(t *testing.T) {
	env := loadLiveJellyfinEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveBackdropTestTimeout)
	defer cancel()

	logger := silentLogger()
	client := jellyfin.New(env.url, env.apiKey, env.userID, logger)

	clearAllBackdrops(ctx, t, env.itemID, client)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), liveBackdropCleanupTimeout)
		defer cleanupCancel()
		clearAllBackdrops(cleanupCtx, t, env.itemID, client)
	})

	dir := t.TempDir()
	seedA, seedB := bandJPEG(t, 0xE1), bandJPEG(t, 0xE2)
	if err := os.WriteFile(dir+"/fanart.jpg", seedA, 0o600); err != nil {
		t.Fatalf("writing fanart.jpg: %v", err)
	}
	if err := os.WriteFile(dir+"/fanart2.jpg", seedB, 0o600); err != nil {
		t.Fatalf("writing fanart2.jpg: %v", err)
	}

	p := New(Deps{
		Logger: logger,
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "live-jf-resync", ConnectionID: "c-jf", PlatformArtistID: env.itemID},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-jf": {ID: "c-jf", Name: "live-jellyfin-uat", Type: connection.TypeJellyfin, URL: env.url, APIKey: env.apiKey, Enabled: true, Status: "ok", Jellyfin: &connection.JellyfinConfig{PlatformUserID: env.userID, FeatureImageWrite: true}},
		}},
	})
	art := &artist.Artist{ID: "live-jf-resync", Name: "Live UAT Artist", Path: dir}

	// Establish the starting platform state through the real full-set sync,
	// not a synthetic seed -- the SAME production path an operator's initial
	// library scan uses.
	seedWarnings := p.SyncAllFanartToPlatforms(ctx, art)
	if len(seedWarnings) != 0 {
		t.Fatalf("SyncAllFanartToPlatforms (seed) returned warnings: %v", seedWarnings)
	}
	time.Sleep(500 * time.Millisecond)

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

	// #3125 C1 ordering: simulate the real save-then-sync sequence so
	// previousFanartPrimaryData's on-disk-backup read has something to find.
	if err := image.BackupSlot(ctx, dir, "fanart", "fanart.jpg"); err != nil {
		t.Fatalf("backing up current primary: %v", err)
	}
	replacement := bandJPEG(t, 0xE3)
	if err := os.WriteFile(dir+"/fanart.jpg", replacement, 0o600); err != nil {
		t.Fatalf("writing local replacement fanart: %v", err)
	}

	// THE OPERATION UNDER TEST: a single-image fanart "replace" sync, the
	// exact call every upload/fetch/crop handler issues for a fanart save.
	warnings := p.SyncImageToPlatforms(ctx, art, "fanart")
	if len(warnings) != 0 {
		t.Fatalf("SyncImageToPlatforms returned warnings: %v", warnings)
	}
	time.Sleep(500 * time.Millisecond)

	after, err := client.GetArtistDetail(ctx, env.itemID)
	if err != nil {
		t.Fatalf("reading state after sync: %v", err)
	}
	// THE #3135 AC ASSERTION: count unchanged. Before the #3135 fix, routing
	// a Jellyfin fanart replace through the plain indexed call (#3125's
	// shape) leaves this at 3 -- Jellyfin appends regardless of the index.
	if after.BackdropCount != 2 {
		t.Errorf("BackdropCount after fanart sync = %d, want 2 (unchanged) -- a Jellyfin fanart replace duplicated a backdrop instead of resyncing the set (#3135)", after.BackdropCount)
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
		t.Errorf("slot 1 content changed -- the fanart resync must faithfully rebuild every local slot, not just the primary")
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

// TestLiveJellyfin_SyncAllFanartToPlatforms_RepeatedRunsInflate_KnownGap is a
// NEW, WIDER finding surfaced during #3135's hostile review, deliberately
// OUT OF SCOPE for this branch's fix: uploadFanartSet (the per-file upload
// SyncAllFanartToPlatforms/syncAllFanartToPlatforms uses) issues
// UploadImageAtIndex for each local file with NO preceding platform-state
// read and NO delete step -- unlike this branch's uploadFanartFullResyncForSync
// contribution, which added the missing delete step for the SINGLE-image
// replace path only. Since Jellyfin's indexed endpoint ignores the index and
// always appends (established elsewhere in this file), a REPEATED full-set
// resync through SyncAllFanartToPlatforms against Jellyfin grows the
// backdrop count WITHOUT BOUND: measured live, 0 -> 3 -> 6 -> 9 across three
// runs of the same 3-file local set (see the PR report for the exact
// numbers). TestLiveEmby_SyncAllFanartToPlatforms_RepeatedRunsDoNotInflate
// below proves Emby, by contrast, stays flat at 3 across the same sequence
// -- the SAME uploadFanartSet code issues the SAME UploadImageAtIndex calls
// to both platforms, so this is a platform-behavior gap, not a client
// request-shape defect.
//
// THIS AFFECTS THE ENTIRE BACKDROPS TAB, NOT JUST THE SINGLE-IMAGE REPLACE
// #3135's issue describes: handleFanartSlotDelete, handleFanartReorder,
// handleFanartBatchDelete, and handleFanartSlotAssign
// (internal/api/handlers_backdrop.go) all route through
// SyncAllFanartToPlatforms -> syncAllFanartToPlatforms -> uploadFanartSet,
// so every one of those operator actions against a Jellyfin connection
// inflates the backdrop count on repetition. #3125's "zero duplicates on
// both platforms" full-indexed-sync measurement did not separately verify
// Jellyfin through this exact call (no live Jellyfin test in this file
// exercised SyncAllFanartToPlatforms before this one), so that claim reads
// as Emby-verified and Jellyfin-assumed, not Jellyfin-measured.
//
// NOT FIXED HERE: fixing uploadFanartSet for Jellyfin needs the SAME
// delete-every-backdrop-then-reupload shape this branch's
// uploadFanartFullResyncForSync already implements for the single-image
// path, but applying it to the full-set sync is a materially different
// change (a different call site, a different warning contract, and its own
// review) than this branch's stated scope. Tracked as #3145; this
// test PINS the gap with a hard assertion so a future fix (or accidental
// regression) is caught by a red/green flip rather than silently
// rediscovered. A RED failure here is GOOD NEWS: it means the count no
// longer grows and #3145 landed (or Jellyfin itself changed).
//
// Seeds THREE distinct local fanart files, then calls
// SyncAllFanartToPlatforms three times in a row against the SAME clean
// starting platform state and asserts the count strictly INCREASES each
// run -- not merely "differs", since a bounded/flat count would be the
// fixed behavior this test exists to flag as still-missing.
func TestLiveJellyfin_SyncAllFanartToPlatforms_RepeatedRunsInflate_KnownGap(t *testing.T) {
	env := loadLiveJellyfinEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveBackdropTestTimeout)
	defer cancel()

	logger := silentLogger()
	client := jellyfin.New(env.url, env.apiKey, env.userID, logger)

	clearAllBackdrops(ctx, t, env.itemID, client)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), liveBackdropCleanupTimeout)
		defer cleanupCancel()
		clearAllBackdrops(cleanupCtx, t, env.itemID, client)
	})

	dir := t.TempDir()
	if err := os.WriteFile(dir+"/fanart.jpg", bandJPEG(t, 0xF1), 0o600); err != nil {
		t.Fatalf("writing fanart.jpg: %v", err)
	}
	if err := os.WriteFile(dir+"/fanart2.jpg", bandJPEG(t, 0xF2), 0o600); err != nil {
		t.Fatalf("writing fanart2.jpg: %v", err)
	}
	if err := os.WriteFile(dir+"/fanart3.jpg", bandJPEG(t, 0xF3), 0o600); err != nil {
		t.Fatalf("writing fanart3.jpg: %v", err)
	}

	p := New(Deps{
		Logger: logger,
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "live-jf-inflate", ConnectionID: "c-jf", PlatformArtistID: env.itemID},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-jf": {ID: "c-jf", Name: "live-jellyfin-uat", Type: connection.TypeJellyfin, URL: env.url, APIKey: env.apiKey, Enabled: true, Status: "ok", Jellyfin: &connection.JellyfinConfig{PlatformUserID: env.userID, FeatureImageWrite: true}},
		}},
	})
	art := &artist.Artist{ID: "live-jf-inflate", Name: "Live UAT Artist", Path: dir}

	// PRECONDITION: exactly zero backdrops before the first run.
	before, err := client.GetArtistDetail(ctx, env.itemID)
	if err != nil {
		t.Fatalf("reading state before any sync: %v", err)
	}
	if before.BackdropCount != 0 {
		t.Fatalf("precondition failed: BackdropCount before run 1 = %d, want 0", before.BackdropCount)
	}

	prevCount := before.BackdropCount
	for run := 1; run <= 3; run++ {
		warnings := p.SyncAllFanartToPlatforms(ctx, art)
		if len(warnings) != 0 {
			t.Fatalf("run %d: SyncAllFanartToPlatforms returned warnings: %v", run, warnings)
		}
		time.Sleep(500 * time.Millisecond)
		after, err := client.GetArtistDetail(ctx, env.itemID)
		if err != nil {
			t.Fatalf("run %d: reading state: %v", run, err)
		}
		t.Logf("KNOWN GAP: BackdropCount after run %d = %d", run, after.BackdropCount)
		wantMin := prevCount + 3 // each run uploads 3 local files
		if after.BackdropCount < wantMin {
			t.Errorf("run %d: BackdropCount = %d, want >= %d -- the known-gap growth did not reproduce; if this is because the count is now STABLE instead, #3145 may have landed (or Jellyfin itself changed) -- update this test accordingly and close #3145", run, after.BackdropCount, wantMin)
		}
		prevCount = after.BackdropCount
	}
}

// TestLiveEmby_SyncAllFanartToPlatforms_RepeatedRunsDoNotInflate is the Emby CONTRAST
// case for TestLiveJellyfin_SyncAllFanartToPlatforms_RepeatedRunsInflate_KnownGap: proves
// the same repeated-SyncAllFanartToPlatforms sequence does NOT inflate
// Emby's backdrop count, because Emby's indexed endpoint genuinely replaces
// in place (unlike Jellyfin's, which appends regardless of index -- see the
// sibling test's finding). Establishes that the inflation is a Jellyfin-only
// server-behavior gap, not a defect in uploadFanartSet's request shape
// itself (the same code issues the same UploadImageAtIndex calls to both
// platforms).
func TestLiveEmby_SyncAllFanartToPlatforms_RepeatedRunsDoNotInflate(t *testing.T) {
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

	dir := t.TempDir()
	if err := os.WriteFile(dir+"/fanart.jpg", bandJPEG(t, 0xF4), 0o600); err != nil {
		t.Fatalf("writing fanart.jpg: %v", err)
	}
	if err := os.WriteFile(dir+"/fanart2.jpg", bandJPEG(t, 0xF5), 0o600); err != nil {
		t.Fatalf("writing fanart2.jpg: %v", err)
	}
	if err := os.WriteFile(dir+"/fanart3.jpg", bandJPEG(t, 0xF6), 0o600); err != nil {
		t.Fatalf("writing fanart3.jpg: %v", err)
	}

	p := New(Deps{
		Logger: logger,
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "live-emby-inflate", ConnectionID: "c-emby", PlatformArtistID: env.itemID},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {ID: "c-emby", Name: "live-emby-uat", Type: connection.TypeEmby, URL: env.url, APIKey: env.apiKey, Enabled: true, Status: "ok", Emby: &connection.EmbyConfig{PlatformUserID: env.userID, FeatureImageWrite: true}},
		}},
	})
	art := &artist.Artist{ID: "live-emby-inflate", Name: "Live UAT Artist", Path: dir}

	for run := 1; run <= 3; run++ {
		warnings := p.SyncAllFanartToPlatforms(ctx, art)
		if len(warnings) != 0 {
			t.Fatalf("run %d: SyncAllFanartToPlatforms returned warnings: %v", run, warnings)
		}
		time.Sleep(500 * time.Millisecond)
		after, err := client.GetArtistDetail(ctx, env.itemID)
		if err != nil {
			t.Fatalf("run %d: reading state: %v", run, err)
		}
		if after.BackdropCount != 3 {
			t.Errorf("run %d: BackdropCount = %d, want 3 (unchanged -- Emby's indexed endpoint replaces in place)", run, after.BackdropCount)
		}
	}
}

// pollBackdropDetail polls GetArtistDetail until the reading STABILIZES (two
// consecutive reads report the same BackdropCount) at the caller's wanted
// count, or a short deadline elapses, tolerating Emby's brief eventual
// consistency on BackdropImageTags after a write (noted in #3126's own
// measurement: the tag list can lag the authoritative image list for a short
// window, converging within seconds). Returns the last observed state either
// way -- callers assert on the returned count themselves.
//
// CR review round (PR #3150): the earlier version returned on the FIRST read
// that matched want, which is circular in exactly the direction this test
// exists to measure -- a transient read of 5 that is actually still climbing
// toward 6 (one extra write landing shortly after SyncAllFanartToPlatforms
// returns) would satisfy that check and hide the very overshoot the test is
// supposed to catch. Requiring the count to hold STILL across two reads
// before accepting a want-match closes that hole: a value in the middle of
// converging upward changes between the two reads and resets the stability
// counter, so the poll keeps going instead of returning early on a number
// that was never going to be the final one.
//
// A persistent mismatch (the platform never reaches want at all) is NOT
// specially handled here and does not need to be: this function still
// returns at the deadline in that case, with `last` holding whatever the
// platform actually settled at, and the caller's own exact-count assertion
// is what turns that into a test failure. This function's only job is to not
// return EARLY on a number that was still in flight.
func pollBackdropDetail(ctx context.Context, t *testing.T, client backdropClearer, itemID string, want int) *connection.ArtistPlatformState {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last *connection.ArtistPlatformState
	stable := 0
	for {
		state, err := client.GetArtistDetail(ctx, itemID)
		if err != nil {
			t.Fatalf("polling backdrop state: %v", err)
		}
		if last != nil && state.BackdropCount == last.BackdropCount {
			stable++
		} else {
			stable = 0
		}
		last = state
		if (stable >= 1 && state.BackdropCount == want) || time.Now().After(deadline) {
			return last
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// TestLiveEmby_SyncAllFanartToPlatforms_LocalSetExceedsPlatformCount_Measures
// closes a gap in #3145's Emby contrast test: every OTHER existing Emby live
// test either starts from an EMPTY item (TestLiveEmby_F3_...,
// TestLiveEmby_IndexedReplace_...) or seeds a platform count that already
// equals the local file count before syncing
// (TestLiveEmby_SyncAllFanartToPlatforms_RepeatedRunsDoNotInflate seeds
// nothing and syncs exactly 3 local files, so every uploaded index lands
// EXACTLY next-in-line as the platform's own count grows in lockstep, one
// upload at a time). This test instead seeds the platform BELOW the local
// file count, so within the SAME push some uploaded indices are ahead of
// what the platform held when the push began.
//
// WHAT THIS TEST DOES NOT DRIVE, STATED EXPLICITLY: it does not force
// uploadFanartSet to SKIP a slot (snapshotFanart degrading or refusing a
// file, which is what spends an index without a write and is the condition
// that could make a gap PERSIST across repeated syncs), and it never
// observed a #3126-shaped false-failure warning in any measured run -- see
// the PR report for the raw-probe evidence on that question. This test's
// claim is narrower and is exactly what it asserts below: a local set
// LARGER than an already-populated platform count converges to the local
// count and STAYS there across repeated syncs, rather than growing further
// on run 2 or run 3.
//
// Seeds the platform with N=3 backdrops directly (the same seeding pattern
// TestLiveEmby_F3_BystanderSurvivesAfterPlatformSideDelete uses), points the
// artist's local directory at N+2=5 distinct fanart files, and runs
// SyncAllFanartToPlatforms -- the exact call every operator action on the
// Backdrops tab issues (see uploadFanartSet's doc comment and #3145's
// finding that this affects handleFanartSlotDelete, handleFanartReorder,
// handleFanartBatchDelete, and handleFanartSlotAssign, not just a fanart
// replace) -- THREE times against the SAME unchanged 5-file local set,
// asserting the EXACT count after every run.
//
// Three runs, not two: a second run's count cannot distinguish "the platform
// caught up once and is now stable" from "every run adds more" -- both would
// show growth after run 1. A third run settles it. The exact-count assertion
// on every run (not just a final floor) is what makes this distinction
// mechanical rather than something a reader has to notice in a log: an
// unbounded-append regression would fail on run 1 already (6, not 5), and a
// bounded one-time-catch-up-then-drift regression would fail on run 2 or 3
// even if run 1 happened to read 5.
func TestLiveEmby_SyncAllFanartToPlatforms_LocalSetExceedsPlatformCount_Measures(t *testing.T) {
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

	// Seed the platform with N=3 backdrops directly via the indexed uploader,
	// NOT through the publisher -- this establishes a platform state smaller
	// than the local set is about to be, the way a real artist ends up with
	// fewer platform backdrops than local files (a prior partial sync, a
	// platform-side prune, or simply a smaller earlier local set that grew).
	seedA, seedB, seedC := bandJPEG(t, 0x51), bandJPEG(t, 0x52), bandJPEG(t, 0x53)
	if err := client.UploadImageAtIndex(ctx, env.itemID, "fanart", 0, seedA, "image/jpeg"); err != nil {
		t.Fatalf("seeding backdrop 0: %v", err)
	}
	if err := client.UploadImageAtIndex(ctx, env.itemID, "fanart", 1, seedB, "image/jpeg"); err != nil {
		t.Fatalf("seeding backdrop 1: %v", err)
	}
	if err := client.UploadImageAtIndex(ctx, env.itemID, "fanart", 2, seedC, "image/jpeg"); err != nil {
		t.Fatalf("seeding backdrop 2: %v", err)
	}

	// PRECONDITION: exactly 3 backdrops before the first sync. Asserted
	// explicitly rather than trusted, per the repo's vacuous-precondition
	// convention -- a seed step that silently failed to add all three would
	// otherwise make every later assertion pass for the wrong reason.
	seeded, err := client.GetArtistDetail(ctx, env.itemID)
	if err != nil {
		t.Fatalf("reading state after seed: %v", err)
	}
	if seeded.BackdropCount != 3 {
		t.Fatalf("precondition failed: BackdropCount after seed = %d, want 3 (N=3 seed step did not establish the expected starting state)", seeded.BackdropCount)
	}

	// N+2 = 5 distinct local fanart files -- MORE than the 3 the platform
	// currently holds, so uploadFanartSet's loop (internal/publish/publisher.go)
	// walks past the platform's actual current count partway through this
	// same push. Fixed content for the whole test: every run re-syncs the
	// SAME bytes, which is what lets the final content-hash check below prove
	// a run actually rewrote slot 0 to the local file's bytes rather than
	// merely leaving the seeded bytes in place under a count that happens to
	// read right.
	dir := t.TempDir()
	localFanart := []string{"fanart.jpg", "fanart2.jpg", "fanart3.jpg", "fanart4.jpg", "fanart5.jpg"}
	localContent := make([][]byte, len(localFanart))
	for i, name := range localFanart {
		localContent[i] = bandJPEG(t, 0x60+i)
		if err := os.WriteFile(filepath.Join(dir, name), localContent[i], 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	wantCount := len(localFanart)

	p := New(Deps{
		Logger: logger,
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "live-emby-oor", ConnectionID: "c-emby", PlatformArtistID: env.itemID},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {ID: "c-emby", Name: "live-emby-uat", Type: connection.TypeEmby, URL: env.url, APIKey: env.apiKey, Enabled: true, Status: "ok", Emby: &connection.EmbyConfig{PlatformUserID: env.userID, FeatureImageWrite: true}},
		}},
	})
	art := &artist.Artist{ID: "live-emby-oor", Name: "Live UAT Artist", Path: dir}

	for run := 1; run <= 3; run++ {
		warnings := p.SyncAllFanartToPlatforms(ctx, art)
		// PINNED, not merely logged: a #3126-shaped false failure (an upload
		// that lands but is reported as an error) would show up here, and an
		// operator-facing warning is itself evidence something needs
		// investigating -- the sibling
		// TestLiveEmby_SyncAllFanartToPlatforms_RepeatedRunsDoNotInflate
		// applies the identical Fatalf-on-any-warning bar.
		if len(warnings) != 0 {
			t.Fatalf("run %d: SyncAllFanartToPlatforms returned warnings: %v", run, warnings)
		}
		after := pollBackdropDetail(ctx, t, client, env.itemID, wantCount)
		// THE PINNED ASSERTION: exact count, both directions, every run --
		// not merely "at least as many as local files". A regression to
		// Jellyfin-shaped unbounded append would read 6 already on run 1; a
		// one-time-catch-up-then-drift regression would read past 5 on run 2
		// or run 3 even if run 1 happened to land on 5. Either shape fails
		// here.
		if after.BackdropCount != wantCount {
			t.Errorf("run %d: BackdropCount = %d, want %d (converged to the local file count, not growing further)", run, after.BackdropCount, wantCount)
		}
		t.Logf("run %d: BackdropCount=%d", run, after.BackdropCount)
	}

	// CONTENT VERIFICATION, not just count: prove the final sync actually
	// REWROTE slots to the local bytes rather than a slot silently holding
	// stale (seeded) content while the count coincidentally reads right.
	// Checks the two slots most likely to reveal a wrong-target write: index
	// 0 (originally seeded with different bytes, seedA) and the last local
	// index (originally not present on the platform at all, so its presence
	// with the WRONG bytes would mean something else landed there instead).
	slot0, _, err := client.GetArtistBackdrop(ctx, env.itemID, 0)
	if err != nil {
		t.Fatalf("reading slot 0 after final run: %v", err)
	}
	if hashOf(slot0) != hashOf(localContent[0]) {
		t.Errorf("slot 0 does not hold fanart.jpg's bytes after the final sync -- count reads right but content is wrong")
	}
	lastIdx := len(localContent) - 1
	slotLast, _, err := client.GetArtistBackdrop(ctx, env.itemID, lastIdx)
	if err != nil {
		t.Fatalf("reading slot %d after final run: %v", lastIdx, err)
	}
	if hashOf(slotLast) != hashOf(localContent[lastIdx]) {
		t.Errorf("slot %d does not hold %s's bytes after the final sync -- count reads right but content is wrong", lastIdx, localFanart[lastIdx])
	}
}
