package api

// #2622 -- the three SLOT-TARGETED fanart writes wired into #2540's cross-artist
// backdrop collision seam, completing the chokepoint coverage #2613 (import) and
// #2623 (append / overwrite-primary) started.
//
// These tests share the fixtures those PRs built (collision_notify_populate_test.go
// and collision_notify_image_write_test.go): decodableBackdropJPEG,
// seedCollidingArtist, seedOwnFanart, wireCollisionNotifier, breakFanartRegistry,
// unwritableFanartName, assertFailedAtSave and assertNothingOnDisk. The contract is
// the same one at three more call sites, so it is deliberately pinned the same way.
//
// Two layers are covered here, and they fail differently:
//
//  1. saveFanartSlotChecked, the shared helper that holds the ordering, the
//     fail-open posture and the once-per-scope cache.
//  2. the WIRING at each of the three handlers. A perfect helper nobody calls
//     protects nothing, and an unwired path is never checked -- not late, not on
//     the next rule sweep, never (the rule checker is a deliberate no-op). So each
//     site gets its own end-to-end test driving the real handler.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/event"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/rule"
)

// slotNaming returns the on-disk filename for a fanart slot under the router's
// ACTIVE platform profile, so tests that inject unwritableFanartName still target
// the name the handler will actually write.
func slotNaming(t *testing.T, r *Router, slot int) []string {
	t.Helper()
	ctx := context.Background()
	return []string{img.FanartFilename(r.getActiveFanartPrimary(ctx), slot, r.isKodiNumbering(ctx))}
}

// assertSlotOverwritten pins that the write actually LANDED: the slot's contents
// differ from the bytes seeded there beforehand.
//
// This is the notify-only half of the contract at the handler sites -- a collision
// must never cost the operator their write -- and mere existence would not show it,
// since the slot already holds a seeded file before the handler runs.
//
// It deliberately does NOT claim to verify BYTE SELECTION (which bytes the verdict
// hashed). An earlier version of this helper compared the on-disk perceptual hash
// against the fixture's and claimed exactly that; it was vacuous, and the reason is
// worth recording so it is not re-attempted:
//
// img.ConvertFormat is PIXEL-PRESERVING in every branch. For non-WebP input it
// returns the input bytes unchanged, and for WebP it decodes and re-encodes as PNG,
// which is lossless. A perceptual hash depends only on the decoded pixels, so
// hashing ConvertFormat(data) and hashing data yield the SAME value -- measured
// directly against a real lossy WebP fixture, both 0xe7cf8f9f3f3f7f7f. No fixture
// can make that mutation observable through the hash, WebP included.
//
// So byte selection is guaranteed STRUCTURALLY rather than behaviorally:
// saveFanartSlotChecked takes one []byte and feeds it to both the verdict and the
// write, leaving no call site able to express a divergence. That is the real
// protection, and a test asserting otherwise would only be describing a guarantee
// it cannot enforce.
func assertSlotOverwritten(t *testing.T, path string, seeded []byte) {
	t.Helper()
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the written slot: %v", err)
	}
	if bytes.Equal(onDisk, seeded) {
		t.Errorf("slot still holds the seeded bytes (%d of them): notify-only must never skip the write",
			len(seeded))
	}
}

// TestSaveFanartSlotChecked_CollisionNotifiesOnlyAfterSaveSucceeds pins the
// notify-after-confirmed-write ordering on the SLOT chokepoint.
//
// The durable half of a collision notification is a fixable Action Queue entry
// whose auto-fix BACKS ARTWORK OUT of the artist. Emitting it for a slot write
// that then failed would point a destructive remediation at a file that was never
// created. So the verdict is computed early (while the bytes are in hand) but
// announced only once the save is confirmed.
func TestSaveFanartSlotChecked_CollisionNotifiesOnlyAfterSaveSucceeds(t *testing.T) {
	jpegBytes, phash := decodableBackdropJPEG(t)

	t.Run("save fails: no notification and no durable violation", func(t *testing.T) {
		r := testRouterForLibraryOps(t)
		seedCollidingArtist(t, r, phash)
		pub, raised := wireCollisionNotifier(r)

		// An ORDINARY destination directory with a fanart name img.Save cannot write
		// atomically. The directory has to stay valid: the slot path takes a BACKUP of
		// the existing slot before it writes, and a broken directory would fail THERE,
		// leaving the save itself untested. See unwritableFanartName.
		dir := t.TempDir()
		unwritableFanartName(t, r)

		a := &artist.Artist{ID: "slot-save-fail", Name: "Slot Fails", Path: dir}

		// Precondition: a genuine cross-artist collision really is reachable for these
		// bytes, so "no notification" below reflects the failed write and not an absent
		// verdict. Probed on a THROWAWAY scope so the scope under test starts cold.
		if v := r.newImageWriteScope(a).collisionVerdict(context.Background(), jpegBytes); v == nil {
			t.Fatal("no collision verdict for these bytes; the assertions below would pass for the wrong reason")
		}

		saved, err := r.saveFanartSlotChecked(context.Background(), r.newImageWriteScope(a),
			dir, slotNaming(t, r, 0), jpegBytes, nil)

		// Preconditions: the write really did fail, it failed AT img.Save rather than
		// at the backup ahead of it, and it left nothing behind. Without these the "no
		// notification" assertions below would prove nothing.
		if err == nil {
			t.Fatalf("saveFanartSlotChecked returned nil error; the save was expected to fail (saved=%v)", saved)
		}
		if len(saved) != 0 {
			t.Fatalf("saved = %v, want none", saved)
		}
		assertFailedAtSave(t, err, "backing up")
		assertNothingOnDisk(t, dir)

		if len(pub.events) != 0 {
			t.Errorf("SSE collision events = %d, want 0: notified for a slot write that never landed", len(pub.events))
		}
		if *raised != 0 {
			t.Errorf("durable violations raised = %d, want 0: a fixable back-out entry now points at "+
				"artwork that was never written", *raised)
		}
	})

	t.Run("save succeeds: notification raised and image still written", func(t *testing.T) {
		r := testRouterForLibraryOps(t)
		seedCollidingArtist(t, r, phash)
		pub, raised := wireCollisionNotifier(r)

		dir := t.TempDir()
		a := &artist.Artist{ID: "slot-save-ok", Name: "Slot Works", Path: dir}
		naming := slotNaming(t, r, 0)
		saved, err := r.saveFanartSlotChecked(context.Background(), r.newImageWriteScope(a),
			dir, naming, jpegBytes, nil)
		if err != nil {
			t.Fatalf("saveFanartSlotChecked: %v", err)
		}

		// NOTIFY-ONLY: the collision must never have blocked the write.
		if len(saved) == 0 {
			t.Fatal("no file saved: notify-only must never skip the write")
		}
		if _, statErr := os.Stat(filepath.Join(dir, naming[0])); statErr != nil {
			t.Errorf("slot file not on disk despite a reported save: %v", statErr)
		}

		if len(pub.events) != 1 {
			t.Fatalf("SSE collision events = %d, want exactly 1", len(pub.events))
		}
		if pub.events[0].Type != event.BackdropCollision {
			t.Errorf("event type = %q, want %q", pub.events[0].Type, event.BackdropCollision)
		}
		if *raised != 1 {
			t.Errorf("durable violations raised = %d, want exactly 1", *raised)
		}
	})

	t.Run("no cross-artist collision: image written with no notification", func(t *testing.T) {
		r := testRouterForLibraryOps(t)
		pub, raised := wireCollisionNotifier(r)

		dir := t.TempDir()
		a := &artist.Artist{ID: "slot-no-collision", Name: "Slot No Collision", Path: dir}
		// The registry holds this SAME artist's fanart, so it is populated but cannot
		// mismatch: CompareIdentity excludes the destination artist. Without this the
		// assertion would pass against an empty registry, for the wrong reason.
		seedOwnFanart(t, r, a.ID, phash)

		// TWO independent layers enforce "only a mismatch notifies": collisionVerdict's
		// own verdict filter, and collision.Notifier.Notify, which re-checks
		// res.Verdict before publishing anything. Measured, not assumed -- deleting the
		// filter in collisionVerdict leaves this subtest GREEN, because the notifier
		// absorbs it. What is pinned here is therefore the OUTCOME, which must hold
		// whichever layer catches it, matching how #2623 pins the zero-hash case.

		saved, err := r.saveFanartSlotChecked(context.Background(), r.newImageWriteScope(a),
			dir, slotNaming(t, r, 0), jpegBytes, nil)
		if err != nil {
			t.Fatalf("saveFanartSlotChecked: %v", err)
		}
		if len(saved) == 0 {
			t.Fatal("no file saved")
		}
		if len(pub.events) != 0 || *raised != 0 {
			t.Errorf("events = %d, raised = %d, want 0 and 0: the artist's own fanart is not a cross-artist collision",
				len(pub.events), *raised)
		}
	})

	t.Run("check failure: image still written, no notification", func(t *testing.T) {
		r := testRouterForLibraryOps(t)
		// A genuine cross-artist collision IS present, so the ONLY thing suppressing
		// the notification below is the failed index build.
		seedCollidingArtist(t, r, phash)
		pub, raised := wireCollisionNotifier(r)

		dir := t.TempDir()
		a := &artist.Artist{ID: "slot-check-fails", Name: "Slot Check Fails", Path: dir}
		scope := r.newImageWriteScope(a)

		// Break the registry at its source: BuildFanartIdentityIndex is a whole-library
		// scan of artist_images, so hiding that table is the transient-DB-failure case
		// in miniature.
		breakFanartRegistry(t, r)

		naming := slotNaming(t, r, 0)
		saved, err := r.saveFanartSlotChecked(context.Background(), scope, dir, naming, jpegBytes, nil)
		if err != nil {
			t.Fatalf("saveFanartSlotChecked: a failed collision check must not fail the write: %v", err)
		}
		if len(saved) == 0 {
			t.Fatal("no file saved: a failed collision check must never cost the operator their write")
		}
		if _, statErr := os.Stat(filepath.Join(dir, naming[0])); statErr != nil {
			t.Errorf("slot file not on disk despite a reported save: %v", statErr)
		}

		// Precondition: the build was ATTEMPTED (and failed) rather than skipped.
		if !scope.built {
			t.Error("scope reports no index build; the build must have been attempted")
		}
		if len(pub.events) != 0 || *raised != 0 {
			t.Errorf("events = %d, raised = %d, want 0 and 0: a verdict was announced with no registry to reach it",
				len(pub.events), *raised)
		}
	})
}

// TestSaveFanartSlotChecked_IdentityIndexBuiltOncePerScope pins the once-per-scope
// caching contract (design-2540.md section 4) on the slot path.
//
// BuildFanartIdentityIndex is a WHOLE-LIBRARY scan and deliberately does no caching
// of its own, so honoring "once per scope" is the guard's job. Proven BEHAVIORALLY
// rather than by reading a counter: after the first write the registry is broken, so
// a scope that REUSES its cached index keeps colliding while one that rebuilds per
// image gets a nil index and silently stops -- opposite outcomes.
func TestSaveFanartSlotChecked_IdentityIndexBuiltOncePerScope(t *testing.T) {
	jpegBytes, phash := decodableBackdropJPEG(t)

	r := testRouterForLibraryOps(t)
	seedCollidingArtist(t, r, phash)
	pub, raised := wireCollisionNotifier(r)

	dir := t.TempDir()
	a := &artist.Artist{ID: "slot-scope-reuse", Name: "Slot Scope Reuse", Path: dir}
	scope := r.newImageWriteScope(a)
	if scope == nil {
		t.Fatal("newImageWriteScope returned nil with the seam fully wired")
	}

	// Slot 0 builds the index and finds the seeded cross-artist collision.
	if _, err := r.saveFanartSlotChecked(context.Background(), scope, dir, slotNaming(t, r, 0), jpegBytes, nil); err != nil {
		t.Fatalf("first slot write: %v", err)
	}
	if len(pub.events) != 1 || *raised != 1 {
		t.Fatalf("events = %d, raised = %d after the first slot, want 1 each: the check did not run",
			len(pub.events), *raised)
	}

	breakFanartRegistry(t, r)

	const slots = 4
	for i := 1; i < slots; i++ {
		if _, err := r.saveFanartSlotChecked(context.Background(), scope, dir, slotNaming(t, r, i), jpegBytes, nil); err != nil {
			t.Fatalf("slot write %d: %v", i, err)
		}
	}

	// Every later slot still collided, so every one of them saw the index built for
	// slot 0. A per-image build would have yielded nil here and left the counts at 1.
	if len(pub.events) != slots || *raised != slots {
		t.Errorf("events = %d, raised = %d, want %d each: slots after the first saw no registry, so the "+
			"whole-library scan is being repeated per image instead of cached for the scope",
			len(pub.events), *raised, slots)
	}
}

// ----------------------------------------------------------------------------
// Per-site wiring. Each of the three slot handlers is driven for real, because a
// correct helper at an UNWIRED call site protects nothing.
// ----------------------------------------------------------------------------

// TestHandleFanartSlotAssign_NotifiesCrossArtistCollision covers the
// highest-severity site: assigning a PLATFORM backdrop into a fanart slot. The
// bytes come from the connected platform, which is exactly where the pollution
// this seam exists to catch actually lives -- an Emby backdrop already carrying
// another artist's artwork lands in the local library through this route.
//
// This site writes the CONVERTED bytes (it runs ConvertFormat on the download
// before saving), so the verdict must hash the conversion, not the raw download.
func TestHandleFanartSlotAssign_NotifiesCrossArtistCollision(t *testing.T) {
	jpegBytes, phash := decodableBackdropJPEG(t)
	artistDir := t.TempDir()

	r, artistSvc := testRouterForBackdrops(t)
	seedCollidingArtist(t, r, phash)
	pub, raised := wireCollisionNotifier(r)

	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/Items/emby-artist-1/Images/Backdrop/0" {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpegBytes)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer embySrv.Close()

	a := &artist.Artist{Name: "Slot Assign Artist", SortName: "Slot Assign Artist", Type: "group", Path: artistDir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	addTestConnectionWithURLForBackdrop(t, r, embySrv.URL)
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-artist-1"); err != nil {
		t.Fatalf("setting platform ID: %v", err)
	}

	body := `{"connection_id":"conn-emby","platform_index":0}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fanart/0/assign", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	req.SetPathValue("slot", "0")
	w := httptest.NewRecorder()

	r.handleFanartSlotAssign(w, req)

	// NOTIFY-ONLY: the collision must never have blocked the assignment.
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (notify-only must never fail the write); body: %s", w.Code, w.Body.String())
	}
	primary := r.getActiveFanartPrimary(context.Background())
	paths, _ := img.DiscoverFanart(artistDir, primary)
	if len(paths) != 1 {
		t.Fatalf("got %d fanart files, want 1: the assignment itself must still happen", len(paths))
	}

	if len(pub.events) != 1 {
		t.Fatalf("SSE collision events = %d, want exactly 1: the platform-backdrop slot assign is unwired "+
			"from the collision seam, so cross-artist pollution imported this way is never detected", len(pub.events))
	}
	if pub.events[0].Type != event.BackdropCollision {
		t.Errorf("event type = %q, want %q", pub.events[0].Type, event.BackdropCollision)
	}
	if *raised != 1 {
		t.Errorf("durable violations raised = %d, want exactly 1", *raised)
	}
}

// TestHandleImageFetchFanartSlot_NotifiesCrossArtistCollision covers the
// fetch-into-slot site.
//
// Byte selection is worth noting at this site: unlike every other fanart write in
// handlers_image.go, this path saves the RAW fetched bytes and never calls
// ConvertFormat. The verdict is computed over those same raw bytes, structurally --
// saveFanartSlotChecked takes one slice and uses it for both. See
// assertSlotOverwritten for why that cannot be pinned behaviorally instead.
func TestHandleImageFetchFanartSlot_NotifiesCrossArtistCollision(t *testing.T) {
	jpegBytes, phash := decodableBackdropJPEG(t)

	r := testRouterForLibraryOps(t)
	seedCollidingArtist(t, r, phash)
	pub, raised := wireCollisionNotifier(r)

	dir := t.TempDir()
	a := &artist.Artist{Name: "Fetch Slot Artist", SortName: "Fetch Slot Artist", Path: dir}
	if err := r.artistService.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	// The slot must already exist: handleImageFetchFanartSlot re-validates the slot
	// against the fanart actually on disk before writing (#2331 CR-1).
	names := seedFanartSlots(t, r, dir, 1)
	slotPath := filepath.Join(dir, names[0])
	seeded, err := os.ReadFile(slotPath)
	if err != nil {
		t.Fatalf("reading the seeded slot: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fetch", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageFetchFanartSlot(w, req, a, a.ID, 0, jpegBytes, "https://example.invalid/bg.jpg")

	// NOTIFY-ONLY: the write happens regardless of the verdict.
	assertSlotOverwritten(t, slotPath, seeded)

	if len(pub.events) != 1 {
		t.Fatalf("SSE collision events = %d, want exactly 1: the fetch-into-slot write is unwired from the "+
			"collision seam", len(pub.events))
	}
	if *raised != 1 {
		t.Errorf("durable violations raised = %d, want exactly 1", *raised)
	}
}

// TestHandleImageCropFanartSlot_NotifiesCrossArtistCollision covers the
// crop-into-slot site. Like the fetch path it writes its bytes RAW (the crop
// already produced encoded output).
func TestHandleImageCropFanartSlot_NotifiesCrossArtistCollision(t *testing.T) {
	jpegBytes, phash := decodableBackdropJPEG(t)

	r := testRouterForLibraryOps(t)
	seedCollidingArtist(t, r, phash)
	pub, raised := wireCollisionNotifier(r)

	dir := t.TempDir()
	a := &artist.Artist{Name: "Crop Slot Artist", SortName: "Crop Slot Artist", Path: dir}
	if err := r.artistService.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	names := seedFanartSlots(t, r, dir, 1)
	slotPath := filepath.Join(dir, names[0])
	seeded, err := os.ReadFile(slotPath)
	if err != nil {
		t.Fatalf("reading the seeded slot: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/crop", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleImageCropFanartSlot(w, req, a, a.ID, 0, jpegBytes)

	assertSlotOverwritten(t, slotPath, seeded)

	if len(pub.events) != 1 {
		t.Fatalf("SSE collision events = %d, want exactly 1: the crop-into-slot write is unwired from the "+
			"collision seam", len(pub.events))
	}
	if *raised != 1 {
		t.Errorf("durable violations raised = %d, want exactly 1", *raised)
	}
}

// ----------------------------------------------------------------------------
// #2626 -- the FOURTH fanart-reaching-disk path: applying a user-chosen image
// candidate from the Notifications inbox.
//
// REACHABILITY (proven, not assumed -- this is the whole reason #2626 was not
// simply wired on sight):
//
//	fanart_exists / fanart_min_res / fanart_aspect are seeded ENABLED, and
//	SeedDefaults gives them automation_mode "auto" (service.go). In auto mode
//	ImageFixer.Fix returns the candidate LIST -- rather than downloading one --
//	whenever more than one candidate survives quality filtering and
//	SelectBestCandidate is unset (fixers.go). SelectBestCandidate is declared in
//	model.go and read in exactly one place; NOTHING in the codebase ever sets it,
//	so it is always false. The violation is then persisted as pending_choice
//	(fixer.go), which is precisely the state handleApplyViolationCandidate
//	requires. The operator sees the candidates in the Notifications inbox and
//	clicks one.
//
// So this is the ORDINARY path for a multi-candidate fanart violation, not a
// rare or contrived one: no non-default automation mode, no manual precondition,
// no state that does not arise on its own. Before this change it wrote a
// cross-artist backdrop to disk with zero collision detection -- confirmed live
// against the unwired handler (HTTP 200, fanart.jpg on disk, 0 events, 0
// violations raised).
// ----------------------------------------------------------------------------

// applyCandidateFixture seeds an artist plus a pending_choice fanart violation
// carrying one candidate, and points the router's SSRF client at body. It
// returns the artist and the candidate URL the request must name.
func applyCandidateFixture(t *testing.T, r *Router, svc *artist.Service, dir string, body []byte) (*artist.Artist, string) {
	t.Helper()
	ctx := context.Background()

	a := &artist.Artist{Name: "Apply Candidate", SortName: "Apply Candidate", Path: dir}
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	const candidateURL = "https://8.8.8.8/chosen.jpg"
	v := &rule.RuleViolation{
		RuleID:     rule.RuleFanartExists,
		ArtistID:   a.ID,
		ArtistName: a.Name,
		Severity:   "warning",
		Message:    "no fanart",
		Fixable:    true,
		Status:     rule.ViolationStatusPendingChoice,
		Candidates: []rule.ImageCandidate{{
			URL: candidateURL, Width: 1920, Height: 1080,
			Source: "candidate-provider", ImageType: "fanart",
		}},
	}
	if err := r.ruleService.UpsertViolation(ctx, v); err != nil {
		t.Fatalf("seeding pending-choice violation: %v", err)
	}

	r.ssrfClient = &http.Client{Transport: &stubRoundTripper{body: body}}
	return a, v.ID
}

func applyCandidate(t *testing.T, r *Router, violationID, candidateURL string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/"+violationID+"/apply-candidate",
		strings.NewReader(`{"url":"`+candidateURL+`","image_type":"fanart"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", violationID)
	w := httptest.NewRecorder()
	r.handleApplyViolationCandidate(w, req)
	return w
}

// TestApplyViolationCandidate_NotifiesCrossArtistCollision pins the #2626 wiring:
// a candidate carrying another artist's backdrop is still applied (notify-only),
// and raises exactly one collision notification and one durable violation.
func TestApplyViolationCandidate_NotifiesCrossArtistCollision(t *testing.T) {
	jpegBytes, phash := decodableBackdropJPEG(t)

	r, svc := newImageHandlerTestServer(t)
	seedCollidingArtist(t, r, phash)
	pub, raised := wireCollisionNotifier(r)

	dir := t.TempDir()
	_, violationID := applyCandidateFixture(t, r, svc, dir, jpegBytes)

	w := applyCandidate(t, r, violationID, "https://8.8.8.8/chosen.jpg")

	// NOTIFY-ONLY: the collision must never have blocked the operator's choice.
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (notify-only must never fail the apply); body: %s", w.Code, w.Body.String())
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "fanart*"))
	if len(matches) == 0 {
		t.Fatal("no fanart on disk: the candidate must still be applied")
	}

	if len(pub.events) != 1 {
		t.Fatalf("SSE collision events = %d, want exactly 1: the apply-candidate path is unwired from the "+
			"collision seam, so a cross-artist backdrop chosen from the Notifications inbox is never detected",
			len(pub.events))
	}
	if pub.events[0].Type != event.BackdropCollision {
		t.Errorf("event type = %q, want %q", pub.events[0].Type, event.BackdropCollision)
	}
	if *raised != 1 {
		t.Errorf("durable violations raised = %d, want exactly 1", *raised)
	}
}

// TestApplyViolationCandidate_NoNotificationWhenSaveFails pins the ordering on
// this path: a failed apply must raise NOTHING, since the notification's durable
// half carries an auto-fix that backs artwork out of the artist.
func TestApplyViolationCandidate_NoNotificationWhenSaveFails(t *testing.T) {
	jpegBytes, phash := decodableBackdropJPEG(t)

	r, svc := newImageHandlerTestServer(t)
	seedCollidingArtist(t, r, phash)
	pub, raised := wireCollisionNotifier(r)

	// The same fault the other chokepoints use: an ORDINARY directory with a
	// fanart name img.Save cannot write atomically, so the failure lands AT the
	// save rather than at a step ahead of it. See unwritableFanartName.
	dir := t.TempDir()
	unwritableFanartName(t, r)

	// This handler swallows the save error into a 500 rather than returning it, so
	// the error text is captured from the log to prove WHICH step failed. Without
	// that, a fault firing at the download or the format conversion would satisfy
	// every assertion below while leaving the save itself untested.
	var logs bytes.Buffer
	r.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError}))

	a, violationID := applyCandidateFixture(t, r, svc, dir, jpegBytes)

	// Precondition: a genuine collision really is reachable for these bytes, so
	// "no notification" below reflects the failed write and not an absent verdict.
	if v := r.newImageWriteScope(a).collisionVerdict(context.Background(), jpegBytes); v == nil {
		t.Fatal("no collision verdict for these bytes; the assertions below would pass for the wrong reason")
	}

	w := applyCandidate(t, r, violationID, "https://8.8.8.8/chosen.jpg")

	if w.Code == http.StatusOK {
		t.Fatalf("status = 200, want a failure: the save was expected to fail; body: %s", w.Body.String())
	}

	// The failure must be img.Save's write, NOT the download or the format
	// conversion that run ahead of it -- both of which would also produce a 500 and
	// an empty directory. Same guard the other chokepoints use, applied to the
	// logged error since this path does not return one.
	assertFailedAtSave(t, errors.New(logs.String()), "downloading image", "converting image format")
	assertNothingOnDisk(t, dir)

	if len(pub.events) != 0 {
		t.Errorf("SSE collision events = %d, want 0: notified for an apply that never landed", len(pub.events))
	}
	if *raised != 0 {
		t.Errorf("durable violations raised = %d, want 0: a fixable back-out entry now points at artwork "+
			"that was never written", *raised)
	}
}

// TestApplyViolationCandidate_NoCollisionAppliesSilently pins the negative case,
// non-vacuously: the registry is populated with the artist's OWN fanart, so it
// cannot mismatch (CompareIdentity excludes the destination artist).
func TestApplyViolationCandidate_NoCollisionAppliesSilently(t *testing.T) {
	jpegBytes, phash := decodableBackdropJPEG(t)

	r, svc := newImageHandlerTestServer(t)
	pub, raised := wireCollisionNotifier(r)

	dir := t.TempDir()
	a, violationID := applyCandidateFixture(t, r, svc, dir, jpegBytes)
	seedOwnFanart(t, r, a.ID, phash)

	w := applyCandidate(t, r, violationID, "https://8.8.8.8/chosen.jpg")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "fanart*")); len(matches) == 0 {
		t.Fatal("no fanart on disk")
	}
	if len(pub.events) != 0 || *raised != 0 {
		t.Errorf("events = %d, raised = %d, want 0 and 0: the artist's own fanart is not a cross-artist collision",
			len(pub.events), *raised)
	}
}
