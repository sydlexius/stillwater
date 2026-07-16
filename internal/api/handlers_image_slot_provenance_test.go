package api

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/rule"
)

// blockVal returns a deterministic pseudo-random gray level for a block of a
// distinctJPEG variant. It is a plain integer hash rather than math/rand so the
// fixtures are reproducible across runs and platforms without seeding state.
func blockVal(bx, by, variant int) uint8 {
	h := uint32(bx)*374761393 + uint32(by)*668265263 + uint32(variant)*2246822519
	h ^= h >> 13
	h *= 1274126177
	h ^= h >> 16
	return uint8(h >> 8)
}

// distinctJPEG encodes a 1920x1080 JPEG whose PERCEPTUAL hash is both stable
// and distinct per variant.
//
// This exists because the pre-existing fixtures cannot support a per-slot
// provenance test. writeJPEG paints a uniform grey and testJPEG a uniform
// zero-value canvas; a dHash compares ADJACENT pixels, so every flat image
// hashes to the same all-zero value. A test built on those could assert that
// "slot 1 has a phash" but never that it has the RIGHT one -- the wrong file's
// hash would be byte-identical to the right file's. Distinct fixtures are what
// make "the correct file's hash landed on the correct slot" a real claim.
//
// The pattern is drawn in blocks because PerceptualHashFromImage downsamples to
// 9x8 before hashing; per-pixel noise would be averaged away by the resize,
// while block-level structure survives it.
func distinctJPEG(t *testing.T, variant int) []byte {
	t.Helper()
	const (
		blocks = 8
		w      = 1920
		h      = 1080
	)
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			v := blockVal(x*blocks/w, y*blocks/h, variant)
			m.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, m, nil); err != nil {
		t.Fatalf("encoding JPEG: %v", err)
	}
	return buf.Bytes()
}

// fanartRowBySlot returns the artist_images row for the given fanart slot.
func fanartRowBySlot(t *testing.T, svc *artist.Service, artistID string, slot int) (artist.ArtistImage, bool) {
	t.Helper()
	imgs, err := svc.GetImagesForArtist(context.Background(), artistID)
	if err != nil {
		t.Fatalf("GetImagesForArtist: %v", err)
	}
	for _, im := range imgs {
		if im.ImageType == "fanart" && im.SlotIndex == slot {
			return im, true
		}
	}
	return artist.ArtistImage{}, false
}

// seedPrimaryFanart creates an artist whose primary fanart is written through
// the production img.Save path (so the file carries a REAL embedded phash),
// materializes its slot-0 row, and stamps that file's genuine provenance onto
// it.
//
// Seeding slot 0 with the primary's TRUE values, rather than an invented
// sentinel, is deliberate. setArtistImageFlag legitimately re-records slot 0
// from the primary on the non-append paths, so a sentinel would be overwritten
// by correct behavior and the test would fail for the wrong reason. Seeding
// the real values makes "slot 0 unchanged" hold under both the append path
// (which never touches it) and the per-slot path (which re-records it
// identically), while still going RED the moment a DIFFERENT file's hash lands
// there.
//
// Returns the artist, its directory, and the primary's collected provenance.
func seedPrimaryFanart(t *testing.T, svc *artist.Service, name string) (*artist.Artist, string, img.ProvenanceData) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	a := &artist.Artist{Name: name, SortName: name, Path: dir, FanartExists: true, FanartCount: 1}
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Write via img.Save so the primary gets an embedded DHash exactly as a
	// production save would; CollectProvenance reads the phash back OUT of EXIF,
	// so a file written with a bare os.WriteFile would have no phash at all.
	if _, err := img.Save(dir, "fanart", distinctJPEG(t, 0),
		[]string{"fanart.jpg"}, false, &img.ExifMeta{Source: "seed-source"}, testDiscardLogger()); err != nil {
		t.Fatalf("saving primary fanart: %v", err)
	}
	primaryPath := filepath.Join(dir, "fanart.jpg")
	prim := img.CollectProvenance(primaryPath, testDiscardLogger())

	// PRECONDITION: the fixture actually carries a perceptual hash. If it did
	// not, every "phash" assertion below would be comparing empty strings.
	if prim.PHash == "" {
		t.Fatalf("precondition: primary fixture has no embedded phash; the fixture cannot support this test")
	}

	if err := svc.Update(ctx, a); err != nil {
		t.Fatalf("updating artist to materialize slot 0 row: %v", err)
	}
	if err := svc.UpdateImageProvenance(ctx, a.ID, "fanart", 0,
		prim.PHash, prim.ContentHash, prim.Source, prim.FileFormat, prim.LastWrittenAt); err != nil {
		t.Fatalf("seeding slot 0 provenance: %v", err)
	}

	// PRECONDITION: the seed landed. Without this the "slot 0 unchanged" check
	// could compare nothing against nothing and pass vacuously.
	row, ok := fanartRowBySlot(t, svc, a.ID, 0)
	if !ok {
		t.Fatalf("precondition: slot 0 row was not created by Update")
	}
	if row.PHash != prim.PHash {
		t.Fatalf("precondition: slot 0 phash = %q, want seeded %q", row.PHash, prim.PHash)
	}
	return a, dir, prim
}

// assertSlot0HoldsPrimary proves no other file's provenance was written onto
// the primary's row.
func assertSlot0HoldsPrimary(t *testing.T, svc *artist.Service, artistID string, prim img.ProvenanceData) {
	t.Helper()
	row, ok := fanartRowBySlot(t, svc, artistID, 0)
	if !ok {
		t.Fatalf("slot 0 row vanished")
	}
	if row.PHash != prim.PHash {
		t.Errorf("slot 0 phash = %q, want the PRIMARY's %q -- another file's provenance was written onto slot 0's row",
			row.PHash, prim.PHash)
	}
	if row.ContentHash != prim.ContentHash {
		t.Errorf("slot 0 content_hash = %q, want the PRIMARY's %q -- another file's provenance was written onto slot 0's row",
			row.ContentHash, prim.ContentHash)
	}
}

// fetchFanart drives the fetch handler, optionally targeting an explicit slot.
func fetchFanart(t *testing.T, r *Router, a *artist.Artist, body string, imageBytes []byte) {
	t.Helper()
	r.ssrfClient = &http.Client{Transport: &stubRoundTripper{body: imageBytes}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fetch?skip_crop=true", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)

	w := serveValidated(t, http.HandlerFunc(r.handleImageFetch), req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
}

// TestFanartAppend_RecordsProvenanceOnAppendedSlot is the #2564 regression
// guard for the fanart APPEND path.
//
// Before the fix, the append branch of finalizeImageSave routed only to
// updateArtistFanartCount, which never records provenance. The appended slot's
// row was created by UpsertAll with every provenance column empty and then
// preserved that way on every rescan, so slots 1+ never received a phash. A
// per-slot phash reader over such a row finds no data and reports the artist
// clean because it had nothing to judge -- a false green inside a tool whose
// entire purpose is detecting image corruption.
//
// REVERT-AND-RERUN: deleting the recordImageProvenance call in
// finalizeImageSave, or pinning its slot argument to 0, must turn this RED.
func TestFanartAppend_RecordsProvenanceOnAppendedSlot(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a, dir, prim := seedPrimaryFanart(t, svc, "Append Provenance")

	fetchFanart(t, r, a, `{"url":"https://8.8.8.8/bg.jpg","type":"fanart"}`, distinctJPEG(t, 7))

	// PRECONDITION: the append actually happened on disk. If it silently
	// no-opped, the provenance assertions below would judge a write that never
	// occurred.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var appended string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".jpg") && e.Name() != "fanart.jpg" {
			appended = filepath.Join(dir, e.Name())
		}
	}
	if appended == "" {
		t.Fatalf("precondition: no appended fanart file on disk; entries=%v", entries)
	}

	// The appended file's own provenance, read from the file the handler wrote.
	// Comparing against this (rather than merely "non-empty") is what makes the
	// assertion specific: it proves the RIGHT file's hash reached the slot.
	want := img.CollectProvenance(appended, testDiscardLogger())
	if want.PHash == "" {
		t.Fatalf("precondition: appended file carries no phash; cannot judge what was recorded")
	}
	// PRECONDITION: the two fixtures are perceptually distinct. If they hashed
	// alike, every cross-slot comparison below would be vacuous.
	if want.PHash == prim.PHash {
		t.Fatalf("precondition: appended and primary fixtures share phash %q; the fixtures cannot discriminate slots", want.PHash)
	}

	// THE GUARD: the appended slot must carry its own perceptual hash.
	row, ok := fanartRowBySlot(t, svc, a.ID, 1)
	if !ok {
		t.Fatalf("no artist_images row for fanart slot 1 after append")
	}
	if row.PHash == "" {
		t.Errorf("slot 1 phash is empty -- the appended fanart was never given provenance, so a per-slot phash reader would see no data and false-green this artist (#2564)")
	}
	if row.PHash != want.PHash {
		t.Errorf("slot 1 phash = %q, want the APPENDED file's %q", row.PHash, want.PHash)
	}

	assertSlot0HoldsPrimary(t, svc, a.ID, prim)
}

// TestFanartExplicitSlot_RecordsProvenanceOnThatSlot is the #2564 regression
// guard for the per-slot Crop/Fetch edit path (#2281).
//
// This path writes a specific numbered fanart file, then fell through to
// setArtistImageFlag -- which rediscovers the PRIMARY on disk and re-records
// slot 0, leaving the slot it actually edited starved. It is the second of the
// only two writers of slot >0, and it had the same defect as the append path.
//
// REVERT-AND-RERUN: deleting the recordImageProvenance call in
// finalizeImageSave, or pinning its slot argument to 0, must turn this RED.
func TestFanartExplicitSlot_RecordsProvenanceOnThatSlot(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a, dir, prim := seedPrimaryFanart(t, svc, "Explicit Slot Provenance")

	// Put a file in slot 1 so the explicit-slot edit has a target to replace,
	// and materialize its row -- UpsertAll leaves its provenance empty, which is
	// precisely the starved state this fix exists to end.
	if err := os.WriteFile(filepath.Join(dir, "fanart1.jpg"), distinctJPEG(t, 3), 0o644); err != nil {
		t.Fatalf("seeding slot 1 file: %v", err)
	}
	a.FanartCount = 2
	if err := svc.Update(context.Background(), a); err != nil {
		t.Fatalf("updating artist to materialize slot 1 row: %v", err)
	}
	pre, ok := fanartRowBySlot(t, svc, a.ID, 1)
	if !ok {
		t.Fatalf("precondition: slot 1 row was not created")
	}
	if pre.PHash != "" {
		t.Fatalf("precondition: slot 1 phash = %q, want empty (the starved state under test)", pre.PHash)
	}

	fetchFanart(t, r, a, `{"url":"https://8.8.8.8/bg.jpg","type":"fanart","slot":1}`, distinctJPEG(t, 7))

	want := img.CollectProvenance(filepath.Join(dir, "fanart1.jpg"), testDiscardLogger())
	if want.PHash == "" {
		t.Fatalf("precondition: edited slot-1 file carries no phash; cannot judge what was recorded")
	}
	if want.PHash == prim.PHash {
		t.Fatalf("precondition: edited and primary fixtures share phash %q; the fixtures cannot discriminate slots", want.PHash)
	}

	row, ok := fanartRowBySlot(t, svc, a.ID, 1)
	if !ok {
		t.Fatalf("no artist_images row for fanart slot 1 after explicit-slot edit")
	}
	if row.PHash == "" {
		t.Errorf("slot 1 phash is empty -- the per-slot edit (#2281) wrote slot 1 but recorded provenance elsewhere, leaving the edited slot starved (#2564)")
	}
	if row.PHash != want.PHash {
		t.Errorf("slot 1 phash = %q, want the EDITED file's %q", row.PHash, want.PHash)
	}

	assertSlot0HoldsPrimary(t, svc, a.ID, prim)
}

// TestApplyViolationCandidate_RecordsProvenanceOnAppliedImage is the #2564
// regression guard for the apply-candidate path.
//
// handleApplyViolationCandidate downloads a user-chosen candidate and writes it
// to the artist's primary slot, then persists the artist -- and Update creates
// the artist_images row through UpsertAll, which leaves every provenance column
// empty and then deliberately PRESERVES them empty on each later rescan. Without
// the recording call the row is therefore starved permanently, not transiently:
// a per-slot phash reader over it finds nothing to judge and reports the artist
// clean. That false green is exactly the defect class #2564 exists to close, and
// this is the one image-write path a user reaches by hand from the notifications
// screen, so it earns its own guard.
//
// The assertion is specific rather than "phash is non-empty": it compares the
// row against provenance collected from the file the handler actually wrote, and
// checks that the CANDIDATE's provider source made it through. The handler
// builds an ExifMeta from matchedCandidate.Source, so a row carrying that source
// proves the chosen candidate's identity survived the download-save-record trip
// rather than some other file's.
//
// REVERT-AND-RERUN: deleting the recordImageProvenanceSlot0 call in
// handleApplyViolationCandidate must turn this RED.
func TestApplyViolationCandidate_RecordsProvenanceOnAppliedImage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, svc := newImageHandlerTestServer(t)

	dir := t.TempDir()
	a := &artist.Artist{Name: "Apply Candidate Provenance", SortName: "Apply Candidate Provenance", Path: dir}
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// A pending_choice violation carrying the candidate the request will name.
	// The handler validates the posted url+image_type against these stored
	// candidates, so this is the only way to reach the save.
	const candidateURL = "https://8.8.8.8/chosen.jpg"
	const candidateSource = "candidate-provider"
	v := &rule.RuleViolation{
		RuleID:     rule.RuleFanartExists,
		ArtistID:   a.ID,
		ArtistName: a.Name,
		Severity:   "warning",
		Message:    "no fanart",
		Fixable:    true,
		Status:     rule.ViolationStatusPendingChoice,
		Candidates: []rule.ImageCandidate{{
			URL:       candidateURL,
			Width:     1920,
			Height:    1080,
			Source:    candidateSource,
			ImageType: "fanart",
		}},
	}
	if err := r.ruleService.UpsertViolation(ctx, v); err != nil {
		t.Fatalf("seeding pending-choice violation: %v", err)
	}

	// PRECONDITION: the slot-0 row is either absent or starved before the
	// request. If it already carried a phash, the assertion below could pass on
	// pre-existing data without the handler recording anything.
	if row, ok := fanartRowBySlot(t, svc, a.ID, 0); ok && row.PHash != "" {
		t.Fatalf("precondition: slot 0 already has phash %q before apply; the test could not attribute a later value to the handler", row.PHash)
	}

	r.ssrfClient = &http.Client{Transport: &stubRoundTripper{body: distinctJPEG(t, 5)}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/"+v.ID+"/apply-candidate",
		strings.NewReader(`{"url":"`+candidateURL+`","image_type":"fanart"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", v.ID)

	w := httptest.NewRecorder()
	r.handleApplyViolationCandidate(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	// PRECONDITION: the candidate actually landed on disk. Without this the
	// provenance assertions would judge a write that never happened -- the
	// handler reporting success while doing nothing.
	appliedPath := filepath.Join(dir, "fanart.jpg")
	if _, err := os.Stat(appliedPath); err != nil {
		t.Fatalf("precondition: applied candidate not on disk at %s: %v", appliedPath, err)
	}

	// The applied file's own provenance, read back from the bytes the handler
	// wrote. Comparing against this is what makes the claim specific.
	want := img.CollectProvenance(appliedPath, testDiscardLogger())
	if want.PHash == "" {
		t.Fatalf("precondition: applied file carries no embedded phash; cannot judge what was recorded")
	}

	// THE GUARD: the applied image's slot must carry its own provenance.
	row, ok := fanartRowBySlot(t, svc, a.ID, 0)
	if !ok {
		t.Fatalf("no artist_images row for fanart slot 0 after apply-candidate")
	}
	if row.PHash == "" {
		t.Errorf("slot 0 phash is empty -- the applied candidate was never given provenance, so a per-slot phash reader would see no data and false-green this artist (#2564)")
	}
	if row.PHash != want.PHash {
		t.Errorf("slot 0 phash = %q, want the APPLIED file's %q", row.PHash, want.PHash)
	}
	if row.ContentHash != want.ContentHash {
		t.Errorf("slot 0 content_hash = %q, want the APPLIED file's %q", row.ContentHash, want.ContentHash)
	}
	// The handler stamps the CHOSEN candidate's provider into the file's EXIF;
	// seeing it on the row proves the candidate's identity survived the trip.
	if row.Source != candidateSource {
		t.Errorf("slot 0 source = %q, want the chosen candidate's %q", row.Source, candidateSource)
	}
}
