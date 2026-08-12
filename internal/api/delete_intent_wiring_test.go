package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	img "github.com/sydlexius/stillwater/internal/image"
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
// them. assertNoDeleteIntentYet pins that as a PRECONDITION rather than
// assuming it: without it, a leaked marker would make every assertion below
// pass no matter what the handler did.

// assertNoDeleteIntentYet fails unless dir carries no delete marker of any age
// for imageType.
func assertNoDeleteIntentYet(t *testing.T, dir, imageType string) {
	t.Helper()
	if img.DeleteIntentAfter(dir, imageType, time.Time{}) {
		t.Fatalf("precondition failed: %s already carries a %s delete marker before the request, so the "+
			"assertion below would pass whether or not the handler wrote one", dir, imageType)
	}
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
// named files, and returns the artist and that directory.
func newDeleteIntentArtist(t *testing.T, files ...string) (*Router, *artist.Artist, string) {
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
	return r, a, dir
}

// TestHandleDeleteImage_Fanart_MarksDeleteIntent covers handleDeleteImage's
// fanart branch, which unlinks every numbered variant at once.
func TestHandleDeleteImage_Fanart_MarksDeleteIntent(t *testing.T) {
	t.Parallel()
	r, a, dir := newDeleteIntentArtist(t, "fanart.jpg", "fanart1.jpg")
	assertNoDeleteIntentYet(t, dir, "fanart")

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
}

// TestHandleDeleteImage_SingleSlot_MarksDeleteIntent covers handleDeleteImage's
// single-slot branch (thumb/logo/banner). It is a separate call site from the
// fanart branch above, so dropping either one alone must be caught.
func TestHandleDeleteImage_SingleSlot_MarksDeleteIntent(t *testing.T) {
	t.Parallel()
	r, a, dir := newDeleteIntentArtist(t, "banner.jpg")
	assertNoDeleteIntentYet(t, dir, "banner")

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
}

// TestHandleFanartBatchDelete_MarksDeleteIntent covers the batch delete, which
// renumbers survivors after unlinking -- the case that forced the marker key to
// carry no slot component.
func TestHandleFanartBatchDelete_MarksDeleteIntent(t *testing.T) {
	t.Parallel()
	r, a, dir := newDeleteIntentArtist(t, "fanart.jpg", "fanart1.jpg", "fanart2.jpg")
	assertNoDeleteIntentYet(t, dir, "fanart")

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
}

// TestHandleFanartSlotDelete_MarksDeleteIntent covers the single-slot backdrop
// delete in handlers_backdrop.go, the fourth and last call site.
func TestHandleFanartSlotDelete_MarksDeleteIntent(t *testing.T) {
	t.Parallel()
	r, a, dir := newDeleteIntentArtist(t, "fanart.jpg", "fanart1.jpg")
	assertNoDeleteIntentYet(t, dir, "fanart")

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
}
