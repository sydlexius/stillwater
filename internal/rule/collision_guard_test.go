package rule

// collision_guard_test.go -- behavior tests for the #2540 cross-artist backdrop
// seam at the two internal/rule write chokepoints (#2565).
//
// Every assertion here is on an OBSERVABLE OUTCOME -- a file on disk, a
// recorded Notify call, an index-build count -- never on the mere absence of an
// error. The ordering test (TestCollisionGuard_FailedSave_DoesNotNotify) is the
// load-bearing one: the durable half of a notification carries a destructive
// back-out auto-fix, so a notification must never outlive a write that failed.

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/provider"
)

// --- fakes -----------------------------------------------------------------

type recordedNotify struct {
	destArtistID   string
	destArtistName string
	res            img.IdentityResult
}

// fakeCollisionNotifier records every Notify call so tests can assert both that
// a notification happened and that it did NOT.
type fakeCollisionNotifier struct {
	calls []recordedNotify
}

func (f *fakeCollisionNotifier) Notify(_ context.Context, destArtistID, destArtistName string, res img.IdentityResult) {
	f.calls = append(f.calls, recordedNotify{destArtistID, destArtistName, res})
}

// fakeIdentityIndexer serves a canned registry and counts builds, which is how
// the once-per-scope contract is asserted.
type fakeIdentityIndexer struct {
	entries []img.FanartIdentityEntry
	err     error
	calls   int
}

func (f *fakeIdentityIndexer) BuildFanartIdentityIndex(_ context.Context) ([]img.FanartIdentityEntry, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.entries, nil
}

// --- helpers ---------------------------------------------------------------

// phashOfConverted returns the perceptual hash of the bytes that would actually
// land on disk for the given download, i.e. post-ConvertFormat. Seeding the
// registry with this value under a DIFFERENT artist id guarantees a genuine
// IdentityMismatch rather than one manufactured by a hand-picked constant.
func phashOfConverted(t *testing.T, data []byte) uint64 {
	t.Helper()
	converted, _, err := img.ConvertFormat(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("converting fixture: %v", err)
	}
	h, err := img.PerceptualHash(bytes.NewReader(converted))
	if err != nil {
		t.Fatalf("hashing fixture: %v", err)
	}
	if h == 0 {
		t.Fatal("fixture hashed to 0, which CompareIdentity treats as unusable; the fixture cannot exercise a collision")
	}
	return h
}

// makeStructuredJPEG encodes a JPEG with real block-level brightness variation.
//
// makeTestJPEG (fixer_test.go) paints a SOLID COLOR, whose dHash is all-zero
// because every pixel equals its right neighbor -- and CompareIdentity treats a
// zero hash as unusable, so a solid fixture can never exercise a collision at
// all. Every test here needs a fixture that survives JPEG compression and the
// 9x8 grayscale downscale with a non-zero hash, so the pattern is coarse 8x8
// blocks rather than per-pixel noise.
func makeStructuredJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	bw, bh := max(w/8, 1), max(h/8, 1)
	for y := range h {
		for x := range w {
			// A SCRAMBLED per-block value, not an arithmetic ramp. A ramp is
			// monotonic left-to-right, so every dHash bit ("is the left pixel
			// brighter?") comes out the same and the hash is again 0 -- the first
			// version of this helper hit exactly that. The integer avalanche below
			// makes neighboring blocks differ in both directions with high contrast.
			hb := uint32((x/bw)*374761393 + (y/bh)*668265263)
			hb = (hb ^ (hb >> 13)) * 1274126177
			v := uint8(hb >> 24)
			im.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, im, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encoding structured test jpeg: %v", err)
	}
	return buf.Bytes()
}

// imageServer serves the given bytes as a JPEG and returns its URL.
func imageServer(t *testing.T, body []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/fanart.jpg"
}

func testArtist(dir string) *artist.Artist {
	return &artist.Artist{
		ID:       "dest-artist",
		Name:     "Dest Artist",
		SortName: "Dest Artist",
		Path:     dir,
	}
}

// newGuardedFixer builds an ImageFixer wired to the given fakes, with a plain
// HTTP client because the httptest fixtures bind to loopback.
func newGuardedFixer(n *fakeCollisionNotifier, ix *fakeIdentityIndexer) *ImageFixer {
	f := &ImageFixer{logger: testLogger(), httpClient: &http.Client{Timeout: fetchTimeout}}
	f.SetCollisionGuard(n, ix)
	return f
}

func newGuardedBulk(n *fakeCollisionNotifier, ix *fakeIdentityIndexer) *BulkExecutor {
	e := &BulkExecutor{logger: testLogger(), httpClient: &http.Client{Timeout: fetchTimeout}}
	e.SetCollisionGuard(n, ix)
	return e
}

func fanartCandidates(urls ...string) []provider.ImageResult {
	out := make([]provider.ImageResult, 0, len(urls))
	for _, u := range urls {
		out = append(out, provider.ImageResult{URL: u, Type: provider.ImageFanart, Width: 800, Height: 600, Source: "test"})
	}
	return out
}

func fanartFetchResult(urls ...string) *provider.FetchResult {
	return &provider.FetchResult{Images: fanartCandidates(urls...)}
}

// assertFanartOnDisk fails unless a fanart file was genuinely written. Asserting
// the artifact rather than a return value is the point: a notify-only guard that
// silently suppressed the write would otherwise pass.
func assertFanartOnDisk(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading artist dir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) != "" && e.Name() != ".sw-backup" {
			return
		}
	}
	t.Fatalf("expected a fanart file in %q, found %v", dir, entries)
}

// --- downloadAndPersist (rule auto-fetch) ----------------------------------

func TestDownloadAndPersist_CrossArtistCollision_NotifiesAndStillWrites(t *testing.T) {
	jpeg := makeStructuredJPEG(t, 800, 600)
	url := imageServer(t, jpeg)
	dir := t.TempDir()
	a := testArtist(dir)

	// The registry holds THIS EXACT picture under a different artist: a genuine
	// cross-artist collision, not a synthetic near-hash.
	n := &fakeCollisionNotifier{}
	ix := &fakeIdentityIndexer{entries: []img.FanartIdentityEntry{
		{ArtistID: "other-artist", PHash: phashOfConverted(t, jpeg)},
	}}
	f := newGuardedFixer(n, ix)

	res := f.downloadAndPersist(context.Background(), a, &Violation{RuleID: RuleFanartExists},
		&imageFixContext{imageType: "fanart"}, fanartCandidates(url))

	if !res.Fixed {
		t.Fatalf("the guard is NOTIFY-ONLY and must never block the write; got Fixed=false (%s)", res.Message)
	}
	assertFanartOnDisk(t, dir)

	if len(n.calls) != 1 {
		t.Fatalf("Notify calls = %d, want exactly 1 for a cross-artist collision", len(n.calls))
	}
	got := n.calls[0]
	if got.res.Verdict != img.IdentityMismatch {
		t.Errorf("verdict = %v, want IdentityMismatch", got.res.Verdict)
	}
	if got.res.CollidingArtistID != "other-artist" {
		t.Errorf("CollidingArtistID = %q, want %q", got.res.CollidingArtistID, "other-artist")
	}
	if got.destArtistID != a.ID || got.destArtistName != a.Name {
		t.Errorf("notified dest = (%q,%q), want (%q,%q)", got.destArtistID, got.destArtistName, a.ID, a.Name)
	}
}

func TestDownloadAndPersist_SameArtistMatch_DoesNotNotify(t *testing.T) {
	jpeg := makeStructuredJPEG(t, 800, 600)
	url := imageServer(t, jpeg)
	dir := t.TempDir()
	a := testArtist(dir)

	// Identical picture, but already owned by the DESTINATION artist.
	// CompareIdentity excludes destArtistID, so this is IdentityMatch: re-fetching
	// an artist's own backdrop is not pollution.
	n := &fakeCollisionNotifier{}
	ix := &fakeIdentityIndexer{entries: []img.FanartIdentityEntry{
		{ArtistID: a.ID, PHash: phashOfConverted(t, jpeg)},
	}}
	f := newGuardedFixer(n, ix)

	res := f.downloadAndPersist(context.Background(), a, &Violation{RuleID: RuleFanartExists},
		&imageFixContext{imageType: "fanart"}, fanartCandidates(url))

	if !res.Fixed {
		t.Fatalf("expected the write to succeed; got %s", res.Message)
	}
	assertFanartOnDisk(t, dir)
	if len(n.calls) != 0 {
		t.Fatalf("Notify calls = %d, want 0 for a same-artist match: %+v", len(n.calls), n.calls)
	}
}

// TestDownloadAndPersist_FailedSave_DoesNotNotify is the ORDERING GUARD, the
// most important test in this change. A detected collision on a save that then
// FAILS must produce no notification: the durable half of a notification is an
// Action Queue entry whose auto-fix backs artwork OUT of the artist, so
// notifying here would aim a destructive remediation at a file that was never
// created.
func TestDownloadAndPersist_FailedSave_DoesNotNotify(t *testing.T) {
	jpeg := makeStructuredJPEG(t, 800, 600)
	url := imageServer(t, jpeg)

	// a.Path is a regular FILE, so every write beneath it fails with ENOTDIR.
	// The collision is still fully detectable -- the bytes are in hand and the
	// registry matches -- so this isolates the ordering, not the detection.
	base := t.TempDir()
	notADir := filepath.Join(base, "artist-path-is-a-file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatalf("seeding non-directory artist path: %v", err)
	}
	a := testArtist(notADir)

	n := &fakeCollisionNotifier{}
	ix := &fakeIdentityIndexer{entries: []img.FanartIdentityEntry{
		{ArtistID: "other-artist", PHash: phashOfConverted(t, jpeg)},
	}}
	f := newGuardedFixer(n, ix)

	res := f.downloadAndPersist(context.Background(), a, &Violation{RuleID: RuleFanartExists},
		&imageFixContext{imageType: "fanart"}, fanartCandidates(url))

	// Precondition: the save really did fail. Without this the test would pass
	// vacuously if the write started succeeding.
	if res.Fixed {
		t.Fatalf("precondition failed: the save was expected to fail, but downloadAndPersist reported Fixed=true (%s)", res.Message)
	}
	if len(n.calls) != 0 {
		t.Fatalf("Notify calls = %d, want 0: a failed save must never raise a collision whose auto-fix backs artwork out of the artist; got %+v", len(n.calls), n.calls)
	}
}

// TestDownloadAndPersist_IndexBuildFailure_FailsOpen pins the fail-open
// contract: a failure to EVALUATE a collision must never prevent a write.
func TestDownloadAndPersist_IndexBuildFailure_FailsOpen(t *testing.T) {
	jpeg := makeStructuredJPEG(t, 800, 600)
	url := imageServer(t, jpeg)
	dir := t.TempDir()
	a := testArtist(dir)

	n := &fakeCollisionNotifier{}
	ix := &fakeIdentityIndexer{err: errors.New("registry read failed")}
	f := newGuardedFixer(n, ix)

	res := f.downloadAndPersist(context.Background(), a, &Violation{RuleID: RuleFanartExists},
		&imageFixContext{imageType: "fanart"}, fanartCandidates(url))

	if !res.Fixed {
		t.Fatalf("a broken identity index must not block the write (fail-open); got %s", res.Message)
	}
	assertFanartOnDisk(t, dir)
	if len(n.calls) != 0 {
		t.Fatalf("Notify calls = %d, want 0 when the index could not be built", len(n.calls))
	}
}

// TestDownloadAndPersist_NonFanart_SkipsTheCheckEntirely asserts the fanart gate
// as an observable outcome: a thumb fix must not even build the registry.
func TestDownloadAndPersist_NonFanart_SkipsTheCheckEntirely(t *testing.T) {
	jpeg := makeStructuredJPEG(t, 800, 600)
	url := imageServer(t, jpeg)
	dir := t.TempDir()
	a := testArtist(dir)

	n := &fakeCollisionNotifier{}
	ix := &fakeIdentityIndexer{entries: []img.FanartIdentityEntry{
		{ArtistID: "other-artist", PHash: phashOfConverted(t, jpeg)},
	}}
	f := newGuardedFixer(n, ix)

	res := f.downloadAndPersist(context.Background(), a, &Violation{RuleID: RuleThumbExists},
		&imageFixContext{imageType: "thumb"}, fanartCandidates(url))

	if !res.Fixed {
		t.Fatalf("expected the thumb write to succeed; got %s", res.Message)
	}
	if ix.calls != 0 {
		t.Errorf("BuildFanartIdentityIndex calls = %d, want 0 for a non-fanart write", ix.calls)
	}
	if len(n.calls) != 0 {
		t.Errorf("Notify calls = %d, want 0 for a non-fanart write", len(n.calls))
	}
}

// TestDownloadAndPersist_BuildsIndexOncePerFix pins the once-per-scope contract
// at this site: BuildFanartIdentityIndex is a whole-library read, so it must be
// hoisted OUT of the candidate loop.
//
// The first candidate is a real, fetchable image that is too small for the
// resolution gate. That shape is deliberate: it makes the loop run a full extra
// iteration THROUGH the download and the gate, so an index build left anywhere
// from the top of the loop down to the collision check itself is caught as a
// second build. A first candidate that merely 404'd would short-circuit before
// most of those placements and the assertion would prove almost nothing.
func TestDownloadAndPersist_BuildsIndexOncePerFix(t *testing.T) {
	big := makeStructuredJPEG(t, 800, 600)
	small := makeStructuredJPEG(t, 100, 75)
	tooSmall := imageServer(t, small)
	good := imageServer(t, big)

	dir := t.TempDir()
	a := testArtist(dir)
	n := &fakeCollisionNotifier{}
	ix := &fakeIdentityIndexer{entries: []img.FanartIdentityEntry{
		{ArtistID: "other-artist", PHash: phashOfConverted(t, big)},
	}}
	f := newGuardedFixer(n, ix)

	res := f.downloadAndPersist(context.Background(), a, &Violation{RuleID: RuleFanartExists},
		&imageFixContext{imageType: "fanart", minW: 400, minH: 300}, fanartCandidates(tooSmall, good))

	// Preconditions: the loop really did reject the first candidate at the gate
	// and then save the second. Without both, "built once" would be trivially
	// true because only one iteration ran.
	if !res.Fixed {
		t.Fatalf("precondition failed: expected the second candidate to save; got %s", res.Message)
	}
	if len(n.calls) != 1 {
		t.Fatalf("precondition failed: expected exactly 1 notification from the saved candidate; got %d", len(n.calls))
	}
	if ix.calls != 1 {
		t.Errorf("BuildFanartIdentityIndex calls = %d, want exactly 1 across a 2-candidate fix (once per scope, hoisted out of the loop)", ix.calls)
	}
}

// --- saveBestImage (bulk auto-fix) -----------------------------------------

func TestSaveBestImage_CrossArtistCollision_NotifiesAndStillWrites(t *testing.T) {
	jpeg := makeStructuredJPEG(t, 800, 600)
	url := imageServer(t, jpeg)
	dir := t.TempDir()
	a := testArtist(dir)

	n := &fakeCollisionNotifier{}
	e := newGuardedBulk(n, &fakeIdentityIndexer{})
	idx := &fanartIndex{entries: []img.FanartIdentityEntry{{ArtistID: "other-artist", PHash: phashOfConverted(t, jpeg)}}}

	saved := e.saveBestImage(context.Background(), a, "fanart", fanartFetchResult(url), idx)

	if saved == "" {
		t.Fatal("the guard is NOTIFY-ONLY and must never block the bulk write; got an empty saved path")
	}
	assertFanartOnDisk(t, dir)
	if !a.FanartExists {
		t.Error("a.FanartExists should be set after a successful bulk save")
	}
	if len(n.calls) != 1 {
		t.Fatalf("Notify calls = %d, want exactly 1 for a cross-artist collision", len(n.calls))
	}
	if n.calls[0].res.CollidingArtistID != "other-artist" {
		t.Errorf("CollidingArtistID = %q, want %q", n.calls[0].res.CollidingArtistID, "other-artist")
	}
}

func TestSaveBestImage_SameArtistMatch_DoesNotNotify(t *testing.T) {
	jpeg := makeStructuredJPEG(t, 800, 600)
	url := imageServer(t, jpeg)
	dir := t.TempDir()
	a := testArtist(dir)

	n := &fakeCollisionNotifier{}
	e := newGuardedBulk(n, &fakeIdentityIndexer{})
	idx := &fanartIndex{entries: []img.FanartIdentityEntry{{ArtistID: a.ID, PHash: phashOfConverted(t, jpeg)}}}

	if saved := e.saveBestImage(context.Background(), a, "fanart", fanartFetchResult(url), idx); saved == "" {
		t.Fatal("expected the bulk write to succeed")
	}
	assertFanartOnDisk(t, dir)
	if len(n.calls) != 0 {
		t.Fatalf("Notify calls = %d, want 0 for a same-artist match: %+v", len(n.calls), n.calls)
	}
}

// TestSaveBestImage_FailedSave_DoesNotNotify is the bulk-path half of the
// ordering guard. This path runs unattended and library-wide (Mode "auto"), so a
// notification surviving a failed write is at its most dangerous here.
func TestSaveBestImage_FailedSave_DoesNotNotify(t *testing.T) {
	jpeg := makeStructuredJPEG(t, 800, 600)
	url := imageServer(t, jpeg)

	base := t.TempDir()
	notADir := filepath.Join(base, "artist-path-is-a-file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatalf("seeding non-directory artist path: %v", err)
	}
	a := testArtist(notADir)

	n := &fakeCollisionNotifier{}
	e := newGuardedBulk(n, &fakeIdentityIndexer{})
	idx := &fanartIndex{entries: []img.FanartIdentityEntry{{ArtistID: "other-artist", PHash: phashOfConverted(t, jpeg)}}}

	saved := e.saveBestImage(context.Background(), a, "fanart", fanartFetchResult(url), idx)

	if saved != "" {
		t.Fatalf("precondition failed: the save was expected to fail, but saveBestImage returned %q", saved)
	}
	if len(n.calls) != 0 {
		t.Fatalf("Notify calls = %d, want 0: a failed bulk save must never raise a collision whose auto-fix backs artwork out of the artist; got %+v", len(n.calls), n.calls)
	}
	// The in-run index append obeys the same confirmed-save discipline as the
	// notification, and for the same reason: an entry for a write that failed
	// describes artwork that is not on disk, and it would then be compared
	// against -- and could flag -- every later artist in the job.
	if len(idx.entries) != 1 {
		t.Errorf("identity index has %d entries, want 1 (unchanged): a FAILED save must never be recorded in the in-run registry", len(idx.entries))
	}
}

// TestSaveBestImage_EmptyIndex_FailsOpen covers the fail-open path the bulk site
// reaches when the guard is unwired (run() passes a nil *fanartIndex): the
// candidate must still be fetched and saved, and nothing may be notified.
func TestSaveBestImage_EmptyIndex_FailsOpen(t *testing.T) {
	jpeg := makeStructuredJPEG(t, 800, 600)
	url := imageServer(t, jpeg)
	dir := t.TempDir()
	a := testArtist(dir)

	n := &fakeCollisionNotifier{}
	e := newGuardedBulk(n, &fakeIdentityIndexer{})

	if saved := e.saveBestImage(context.Background(), a, "fanart", fanartFetchResult(url), nil); saved == "" {
		t.Fatal("a nil identity index must not block the bulk write (fail-open)")
	}
	assertFanartOnDisk(t, dir)
	if len(n.calls) != 0 {
		t.Fatalf("Notify calls = %d, want 0 with no registry to compare against", len(n.calls))
	}
}

// TestCollisionGuard_UnwiredIsANoOp pins that a nil guard -- every construction
// that never calls SetCollisionGuard, including all pre-existing tests and the
// headless paths -- behaves exactly as before, with no panic.
func TestCollisionGuard_UnwiredIsANoOp(t *testing.T) {
	var g *collisionGuard
	if g.active("fanart") {
		t.Error("a nil guard must not report active")
	}
	if idx := g.buildIndex(context.Background()); idx != nil {
		t.Errorf("a nil guard must build no index; got %v", idx)
	}
	if v := g.verdict("a", nil, []img.FanartIdentityEntry{{ArtistID: "b", PHash: 1}}); v != nil {
		t.Errorf("a nil guard must produce no verdict; got %v", v)
	}
	g.notify(context.Background(), "a", "A", &img.IdentityResult{Verdict: img.IdentityMismatch})

	// A guard built with a missing collaborator collapses to the same nil no-op.
	if newCollisionGuard(nil, &fakeIdentityIndexer{}, testLogger()) != nil {
		t.Error("a guard with no notifier must be nil")
	}
	if newCollisionGuard(&fakeCollisionNotifier{}, nil, testLogger()) != nil {
		t.Error("a guard with no indexer must be nil")
	}
}

// TestCollisionGuard_UndecodableBytes_FailsOpen covers the phash-failure branch:
// bytes that cannot be decoded must yield NO verdict, never a fabricated
// collision. A fabricated one would be the worst outcome available here -- it
// would aim the back-out auto-fix at an artist over an image the code could not
// even read.
//
// Honest note on what this does and does not pin: deleting the err != nil check
// alone does NOT turn this test red, because PerceptualHash returns 0 on
// failure and CompareIdentity independently treats a zero candidate hash as
// unusable. The two layers are redundant on purpose. What this test does catch
// is the branch being made FAIL-CLOSED (verified: replacing the nil return with
// a synthesized IdentityMismatch turns it red by name).
func TestCollisionGuard_UndecodableBytes_FailsOpen(t *testing.T) {
	g := newCollisionGuard(&fakeCollisionNotifier{}, &fakeIdentityIndexer{}, testLogger())
	if g == nil {
		t.Fatal("expected a wired guard")
	}
	v := g.verdict("dest", []byte("not an image"), []img.FanartIdentityEntry{{ArtistID: "other", PHash: 1}})
	if v != nil {
		t.Errorf("an undecodable candidate must yield no verdict (fail-open); got %+v", v)
	}
}

// TestSaveBestImage_NonFanart_SkipsTheCheckEntirely is the bulk-path mirror of
// TestDownloadAndPersist_NonFanart_SkipsTheCheckEntirely, and it pins the exact
// asymmetry that made this site wrong: the identity index is JOB-scoped but
// FANART-ONLY, while saveBestImage runs once per NEEDED type. Gating on the
// index alone hashed a thumb or logo candidate against the fanart registry and
// could raise a BACKDROP collision -- whose auto-fix backs fanart out -- for a
// write that was never a backdrop.
//
// The registry is seeded with the candidate's OWN hash under a different artist,
// so a missing type gate produces a notification here, not a silent pass. Both
// non-fanart types are covered because the type dimension is precisely what was
// untested: every other TestSaveBestImage_* case passes the literal "fanart".
func TestSaveBestImage_NonFanart_SkipsTheCheckEntirely(t *testing.T) {
	for _, imageType := range []string{"thumb", "logo"} {
		t.Run(imageType, func(t *testing.T) {
			jpeg := makeStructuredJPEG(t, 800, 600)
			url := imageServer(t, jpeg)
			dir := t.TempDir()
			a := testArtist(dir)

			n := &fakeCollisionNotifier{}
			e := newGuardedBulk(n, &fakeIdentityIndexer{})
			idx := &fanartIndex{entries: []img.FanartIdentityEntry{
				{ArtistID: "other-artist", PHash: phashOfConverted(t, jpeg)},
			}}

			result := &provider.FetchResult{Images: []provider.ImageResult{
				{URL: url, Type: provider.ImageType(imageType), Width: 800, Height: 600, Source: "test"},
			}}

			saved := e.saveBestImage(context.Background(), a, imageType, result, idx)

			// Precondition: the write really happened. Without it, "no notification"
			// would be trivially true because nothing was ever saved.
			if saved == "" {
				t.Fatalf("precondition failed: expected the %s write to succeed, got an empty saved path", imageType)
			}
			if len(n.calls) != 0 {
				t.Fatalf("Notify calls = %d, want 0: a %s write must never be compared against the fanart-only registry; got %+v", len(n.calls), imageType, n.calls)
			}
			if len(idx.entries) != 1 {
				t.Errorf("identity index grew to %d entries; a %s write must not be recorded in the fanart registry", len(idx.entries), imageType)
			}
		})
	}
}

// TestSaveBestImage_InRunCollision_SecondArtistIsDetected covers the in-run
// blind spot the once-per-job index would otherwise leave wide open.
//
// The scenario is the primary bulk threat, not an edge case: fetchImages
// name-resolves artists that have no MBID and takes the first result carrying
// one, so several artists in one run can resolve to the same wrong MBID and all
// receive the same backdrop. The pre-run index here is EMPTY -- the true owner
// is not in the library -- which is exactly the case where a purely pre-run
// index reports nothing at all, for any of them.
//
// So the first write must be silent (there is genuinely nothing to compare it
// against) and must SEED the index; the second must then be flagged against the
// first.
func TestSaveBestImage_InRunCollision_SecondArtistIsDetected(t *testing.T) {
	jpeg := makeStructuredJPEG(t, 800, 600)
	url := imageServer(t, jpeg)

	n := &fakeCollisionNotifier{}
	e := newGuardedBulk(n, &fakeIdentityIndexer{})
	idx := &fanartIndex{} // the true owner is absent: nothing to compare against

	first := &artist.Artist{ID: "artist-one", Name: "Artist One", SortName: "Artist One", Path: t.TempDir()}
	if saved := e.saveBestImage(context.Background(), first, "fanart", fanartFetchResult(url), idx); saved == "" {
		t.Fatal("precondition failed: expected the first bulk fanart write to succeed")
	}
	if len(n.calls) != 0 {
		t.Fatalf("Notify calls after the first write = %d, want 0: an empty registry has nothing to collide with; got %+v", len(n.calls), n.calls)
	}
	if len(idx.entries) != 1 {
		t.Fatalf("identity index has %d entries after one confirmed fanart write, want 1: without the in-run append the second artist below can never be detected", len(idx.entries))
	}

	second := &artist.Artist{ID: "artist-two", Name: "Artist Two", SortName: "Artist Two", Path: t.TempDir()}
	if saved := e.saveBestImage(context.Background(), second, "fanart", fanartFetchResult(url), idx); saved == "" {
		t.Fatal("the guard is NOTIFY-ONLY and must never block the second write")
	}
	assertFanartOnDisk(t, second.Path)

	if len(n.calls) != 1 {
		t.Fatalf("Notify calls = %d, want exactly 1: the second artist in the SAME job received the first artist's backdrop and must be flagged; got %+v", len(n.calls), n.calls)
	}
	got := n.calls[0]
	if got.destArtistID != second.ID {
		t.Errorf("notified dest artist = %q, want %q (the artist that received the duplicate)", got.destArtistID, second.ID)
	}
	if got.res.Verdict != img.IdentityMismatch {
		t.Errorf("verdict = %v, want IdentityMismatch", got.res.Verdict)
	}
	if got.res.CollidingArtistID != first.ID {
		t.Errorf("CollidingArtistID = %q, want %q (the artist written earlier in this same job)", got.res.CollidingArtistID, first.ID)
	}
}

// --- bulk job scope --------------------------------------------------------

// TestBulkRun_BuildsIdentityIndexOncePerJob pins the BULK SCOPE DECISION as an
// observable outcome: the cross-artist registry is built ONCE for an entire bulk
// job, not once per artist.
//
// This is the load-bearing half of that decision. BuildFanartIdentityIndex is a
// whole-library read, so a per-artist build would be N whole-library scans
// across N artists -- quadratic, on the unattended library-wide auto-fix path.
// The job below walks three artists; the build count must stay at 1.
//
// The job type is deliberately unrecognized so processArtist takes its default
// branch: that exercises the real run() loop and the real per-artist threading
// of the index without dragging a provider Orchestrator into the fixture.
func TestBulkRun_BuildsIdentityIndexOncePerJob(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	artistSvc := artist.NewService(db)
	ids := make([]string, 0, 3)
	for _, name := range []string{"Artist One", "Artist Two", "Artist Three"} {
		a := &artist.Artist{Name: name, SortName: name, Path: t.TempDir()}
		if err := artistSvc.Create(ctx, a); err != nil {
			t.Fatalf("creating artist %q: %v", name, err)
		}
		ids = append(ids, a.ID)
	}

	bulkSvc := NewBulkService(db)
	job, err := bulkSvc.CreateJob(ctx, "unrecognized_type_for_test", BulkModeYOLO, len(ids))
	if err != nil {
		t.Fatalf("creating bulk job: %v", err)
	}
	job.ArtistIDs = ids

	n := &fakeCollisionNotifier{}
	ix := &fakeIdentityIndexer{entries: []img.FanartIdentityEntry{{ArtistID: "other-artist", PHash: 1}}}
	e := &BulkExecutor{
		bulkService:   bulkSvc,
		artistService: artistSvc,
		logger:        testLogger(),
		httpClient:    &http.Client{Timeout: fetchTimeout},
	}
	e.SetCollisionGuard(n, ix)

	e.run(ctx, job)

	// Precondition: the job really did walk all three artists. Without this,
	// "built once" could just mean the loop never ran.
	if job.ProcessedItems != len(ids) {
		t.Fatalf("precondition failed: ProcessedItems = %d, want %d (the run must actually walk every artist for the once-per-job count to mean anything)", job.ProcessedItems, len(ids))
	}
	if job.Status != BulkStatusCompleted {
		t.Fatalf("precondition failed: job status = %q, want %q", job.Status, BulkStatusCompleted)
	}

	if ix.calls != 1 {
		t.Errorf("BuildFanartIdentityIndex calls = %d, want exactly 1 for a %d-artist job (once per job, not once per artist)", ix.calls, len(ids))
	}
}
