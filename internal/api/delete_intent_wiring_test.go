package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/image/deleteintenttest"
)

// #2712 follow-up: assert THE WIRING, not just the mechanism.
//
// The delete-intent marker only closes #2712 if the delete handlers actually
// write one. Nothing in this package asserted that, so all four
// img.MarkDeleteIntent calls could be replaced with no-ops and the whole
// internal/api suite stayed green -- a future refactor could drop one and
// re-open the bug behind a fully green gate. The marker's own semantics are
// covered in internal/image and its consumption in internal/publish; these
// tests cover only the join between them, which is the part that had none.
//
// Each test drives a real handler and then asks the marker store the same
// question the publisher's repair asks: was an operator delete of this
// (directory, image type) recorded at or after the instant captured before the
// request? A no-op'd MarkDeleteIntent answers no.
//
// Every test uses its own t.TempDir, so the marker store (package-level in
// internal/image, keyed by cleaned directory) cannot leak a marker between
// them. deleteintenttest.NewUnlinkProbe pins that as a PRECONDITION rather than
// assuming it: without it, a leaked marker would make every assertion below
// pass no matter what the handler did.
//
// PLACEMENT, NOT MERE EXISTENCE (#3015 fix round). These tests originally
// asserted only that a marker existed once the handler returned. That is a
// strictly weaker claim than the design makes and it is satisfied equally by a
// handler that marks AFTER its unlink -- which leaves exactly the window
// img.MarkDeleteIntent's doc comment forbids: the file gone, the intent not yet
// visible, an in-flight push free to restore it. Relocating the marks to after
// their unlinks was measured to leave this whole suite green.
//
// So every test below now samples the marker store from INSIDE the unlink,
// through this package's existing Router.fileRemover seam (fs.go) and the
// shared probe in internal/image/deleteintenttest -- the same probe
// internal/rule's fixer tests use, so all seven writers of this marker are
// checked by one assertion in one wording.

// probingRemover is a FileRemover that routes each removal through an
// UnlinkProbe, which samples the delete-intent marker immediately before the
// file is touched.
type probingRemover struct {
	probe *deleteintenttest.UnlinkProbe
	inner FileRemover
}

func (p probingRemover) Remove(name string) error {
	return p.probe.Around(name, func() error { return p.inner.Remove(name) })
}

// assertDeleteIntentSince fails unless a delete marker for (dir, imageType) was
// recorded at or after since -- exactly the test reassertLocalImage applies.
func assertDeleteIntentSince(t *testing.T, dir, imageType string, since time.Time, callSite string) {
	t.Helper()
	if !img.DeleteIntentAfter(dir, imageType, since) {
		t.Fatalf("no %s delete marker recorded for %s by the %s path: an in-flight push would read ENOENT, "+
			"find no operator intent, and restore the artwork the operator just deleted (#2712)",
			imageType, dir, callSite)
	}
}

// newDeleteIntentArtist creates an artist rooted at a fresh temp dir holding the
// named files, wires an UnlinkProbe into the router's FileRemover, and returns
// the artist, that directory, and the probe.
//
// watched names the image types the probe samples at each unlink; it must be
// the type(s) the handler under test is expected to mark.
func newDeleteIntentArtist(t *testing.T, watched []string, files ...string) (*Router, *artist.Artist, string, *deleteintenttest.UnlinkProbe) {
	t.Helper()
	dir := t.TempDir()
	r, artistSvc := testRouterForBackdrops(t)

	a := &artist.Artist{Name: "Delete Intent", SortName: "Delete Intent", Type: "group", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture-"+name), 0o644); err != nil {
			t.Fatalf("writing fixture %s: %v", name, err)
		}
	}
	// The probe is constructed AFTER the fixtures are written and BEFORE the
	// handler runs, so its `since` baseline brackets the request alone. It also
	// asserts no marker exists yet, which is the precondition that keeps every
	// assertion below from passing on a marker this test did not write.
	probe := deleteintenttest.NewUnlinkProbe(t, dir, watched...)
	r.fileRemover = probingRemover{probe: probe, inner: r.fileRemover}
	return r, a, dir, probe
}

// TestHandleDeleteImage_Fanart_MarksDeleteIntent covers handleDeleteImage's
// fanart branch, which unlinks every numbered variant at once.
func TestHandleDeleteImage_Fanart_MarksDeleteIntent(t *testing.T) {
	t.Parallel()
	r, a, dir, probe := newDeleteIntentArtist(t, []string{"fanart"}, "fanart.jpg", "fanart1.jpg")

	before := time.Now()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/images/fanart", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "fanart")
	w := httptest.NewRecorder()

	r.handleDeleteImage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	assertDeleteIntentSince(t, dir, "fanart", before, "handleDeleteImage fanart delete-all")
	probe.AssertMarkedBeforeUnlink("handleDeleteImage fanart delete-all")
}

// TestHandleDeleteImage_SingleSlot_MarksDeleteIntent covers handleDeleteImage's
// single-slot branch (thumb/logo/banner). It is a separate call site from the
// fanart branch above, so dropping either one alone must be caught.
func TestHandleDeleteImage_SingleSlot_MarksDeleteIntent(t *testing.T) {
	t.Parallel()
	r, a, dir, probe := newDeleteIntentArtist(t, []string{"banner"}, "banner.jpg")

	before := time.Now()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/images/banner", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "banner")
	w := httptest.NewRecorder()

	r.handleDeleteImage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	assertDeleteIntentSince(t, dir, "banner", before, "handleDeleteImage single-slot")
	probe.AssertMarkedBeforeUnlink("handleDeleteImage single-slot")
}

// TestHandleFanartBatchDelete_MarksDeleteIntent covers the batch delete, which
// renumbers survivors after unlinking -- the case that forced the marker key to
// carry no slot component.
func TestHandleFanartBatchDelete_MarksDeleteIntent(t *testing.T) {
	t.Parallel()
	r, a, dir, probe := newDeleteIntentArtist(t, []string{"fanart"}, "fanart.jpg", "fanart1.jpg", "fanart2.jpg")

	before := time.Now()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/images/fanart/batch",
		strings.NewReader(`{"indices": [1]}`))
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleFanartBatchDelete(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	assertDeleteIntentSince(t, dir, "fanart", before, "handleFanartBatchDelete")
	probe.AssertMarkedBeforeUnlink("handleFanartBatchDelete")
}

// TestHandleFanartSlotDelete_MarksDeleteIntent covers the single-slot backdrop
// delete in handlers_backdrop.go, the fourth and last call site.
func TestHandleFanartSlotDelete_MarksDeleteIntent(t *testing.T) {
	t.Parallel()
	r, a, dir, probe := newDeleteIntentArtist(t, []string{"fanart"}, "fanart.jpg", "fanart1.jpg")

	before := time.Now()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/images/fanart/1", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("slot", "1")
	w := httptest.NewRecorder()

	r.handleFanartSlotDelete(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	assertDeleteIntentSince(t, dir, "fanart", before, "handleFanartSlotDelete")
	probe.AssertMarkedBeforeUnlink("handleFanartSlotDelete")
}

// TestHandleImageRevert_Fanart_MarksDeleteIntent covers the fanart revert
// branch (#3015), the fifth call site and the one operator-triggered unlink in
// this package that recorded nothing.
//
// Revert drops the NEWEST derived fanart slot, so the fixture needs two files
// for the handler to have anything to drop -- with only the original present it
// answers 404 and never reaches an unlink.
func TestHandleImageRevert_Fanart_MarksDeleteIntent(t *testing.T) {
	t.Parallel()
	r, a, dir, probe := newDeleteIntentArtist(t, []string{"fanart"}, "fanart.jpg", "fanart1.jpg")

	before := time.Now()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fanart/revert", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "fanart")
	w := httptest.NewRecorder()

	r.handleImageRevert(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	// The unlink really happened. Without this the marker assertion could pass
	// for a handler that marked and then failed to remove anything.
	if _, statErr := os.Stat(filepath.Join(dir, "fanart1.jpg")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("precondition failed: the derived fanart slot is still on disk (stat err = %v), so no "+
			"delete occurred and there is nothing for a marker to attest to", statErr)
	}
	assertDeleteIntentSince(t, dir, "fanart", before, "handleImageRevert fanart")
	probe.AssertMarkedBeforeUnlink("handleImageRevert fanart")
}

// TestHandleImageRevert_OwnPushIsNotSelfSuppressed pins the ORDERING property
// the revert path's marker depends on (#3015).
//
// The handler marks, unlinks, and then pushes through revertSideEffects ->
// SyncAllFanartToPlatforms, which stamps its snapAt at ITS function entry. That
// entry is necessarily after the mark, so DeleteIntentAfter's "recorded at or
// after since" test answers false for it and the push's own repair is left free
// to fix a genuine peer clobber. A marker that covered the handler's own push
// would silently disable the #2698 protection for every revert.
//
// The snapshot instant is taken AFTER the real handler returns, so it is a
// measurement of the ordering the production sequence actually produces rather
// than of a hand-placed pair of calls.
func TestHandleImageRevert_OwnPushIsNotSelfSuppressed(t *testing.T) {
	t.Parallel()
	r, a, dir, probe := newDeleteIntentArtist(t, []string{"fanart"}, "fanart.jpg", "fanart1.jpg")

	before := time.Now()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fanart/revert", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "fanart")
	w := httptest.NewRecorder()

	r.handleImageRevert(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	// PRECONDITION: a marker is live. Otherwise the stand-down below is not a
	// statement about ordering, just about there being nothing to consult.
	assertDeleteIntentSince(t, dir, "fanart", before, "handleImageRevert fanart")
	probe.AssertMarkedBeforeUnlink("handleImageRevert fanart")

	// A push entering after the handler returned. The clock has finite
	// resolution, so a short sleep keeps "strictly after" from collapsing into
	// "equal", which DeleteIntentAfter deliberately reads as concurrent.
	time.Sleep(2 * time.Millisecond)
	snapAt := time.Now()

	if img.DeleteIntentAfter(dir, "fanart", snapAt) {
		t.Fatal("the revert handler's marker covers a push stamped after it: the handler's own " +
			"SyncAllFanartToPlatforms call would decline to repair a genuine peer clobber, losing the " +
			"#2698 protection for every revert (#3015)")
	}
}
