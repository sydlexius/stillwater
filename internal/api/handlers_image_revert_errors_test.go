package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/event"
	img "github.com/sydlexius/stillwater/internal/image"
)

// TestHandleImageRevert_ArtistNotFound404 covers the GetByID-error branch: a
// revert for a non-existent artist ID returns 404 with the artist-not-found body.
func TestHandleImageRevert_ArtistNotFound404(t *testing.T) {
	t.Parallel()
	r, _ := testRouterWithPlatform(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/does-not-exist/images/thumb/revert", nil)
	req.SetPathValue("id", "does-not-exist")
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()
	r.handleImageRevert(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
}

// TestHandleImageRevert_MissingID400 covers the RequirePathParam-fail branch:
// a request with no "id" path value is rejected with 400 before any work.
func TestHandleImageRevert_MissingID400(t *testing.T) {
	t.Parallel()
	r, _ := testRouterWithPlatform(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists//images/thumb/revert", nil)
	// Deliberately do NOT set the "id" path value.
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()
	r.handleImageRevert(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

// TestHandleImageRevert_NoImageDir422 covers the requireImageDir-fail branch: an
// artist with no filesystem path and no image cache dir has no image directory,
// so revert is rejected before any disk access.
func TestHandleImageRevert_NoImageDir422(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithPlatform(t)
	// Empty Path + empty imageCacheDir => imageDir() == "" => requireImageDir false.
	a := &artist.Artist{Name: "No Dir", SortName: "No Dir", Path: ""}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/thumb/revert", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()
	r.handleImageRevert(w, req)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body: %s", w.Code, w.Body.String())
	}
}

// TestHandleImageRevert_FanartDiscoverError500 covers the DiscoverFanart-error
// branch: the artist directory path points at a regular file, so reading it as a
// directory fails and the fanart revert returns 500.
func TestHandleImageRevert_FanartDiscoverError500(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithPlatform(t)
	// Path is a regular FILE, not a directory; os.ReadDir on it errors.
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatalf("seeding file path: %v", err)
	}
	a := &artist.Artist{Name: "Bad Dir", SortName: "Bad Dir", Path: f}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fanart/revert", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "fanart")
	w := httptest.NewRecorder()
	r.handleImageRevert(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", w.Code, w.Body.String())
	}
}

// TestHandleImageRevert_FanartRemoveError500 covers the fileRemover.Remove-error
// branch: there are two fanart slots (so a derived slot exists to drop) but the
// remover fails, so the fanart revert returns 500.
func TestHandleImageRevert_FanartRemoveError500(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithPlatform(t)
	r.fileRemover = failingRemover{err: fmt.Errorf("permission denied")}
	dir := t.TempDir()
	a := &artist.Artist{Name: "Remove Fail", SortName: "Remove Fail", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	writeJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)
	writeJPEG(t, filepath.Join(dir, "fanart1.jpg"), 1920, 1080)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fanart/revert", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "fanart")
	w := httptest.NewRecorder()
	r.handleImageRevert(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", w.Code, w.Body.String())
	}
	// The slot must remain on disk since the remove failed.
	if _, err := os.Stat(filepath.Join(dir, "fanart1.jpg")); err != nil {
		t.Errorf("fanart1.jpg should still exist after a failed remove: %v", err)
	}
}

// TestHandleImageRevert_SingleSlotRestoreError500 covers the RestoreSingleSlot
// non-NotExist error branch: a backup file with undecodable bytes makes the
// internal img.Save fail, so the single-slot revert returns 500 (not 404).
func TestHandleImageRevert_SingleSlotRestoreError500(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "Restore Fail", SortName: "Restore Fail", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	// Seed a current canonical thumb so we can prove the failed restore is
	// fail-closed on disk (the canonical must NOT be clobbered when Save errors).
	canonical := filepath.Join(dir, "folder.jpg")
	writeJPEG(t, canonical, 70, 70)
	canonicalBefore, _ := os.ReadFile(canonical)

	// Seed a corrupt backup so RestoreSingleSlot finds it but img.Save rejects it.
	typeDir := filepath.Join(dir, img.BackupDirName, "thumb")
	if err := os.MkdirAll(typeDir, 0o750); err != nil {
		t.Fatalf("creating backup type dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(typeDir, "stale.png"), []byte("not-an-image"), 0o644); err != nil {
		t.Fatalf("seeding corrupt backup: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/thumb/revert", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()
	r.handleImageRevert(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", w.Code, w.Body.String())
	}
	// Fail-closed: a restore error must not have overwritten the canonical thumb.
	canonicalAfter, _ := os.ReadFile(canonical)
	if !bytes.Equal(canonicalBefore, canonicalAfter) {
		t.Error("failed single-slot restore must not clobber the canonical image on disk")
	}
}

// TestHandleImageRevert_NilGateProceeds covers the gateImageWriteStrict nil-gate
// branch: with no conflict gate configured the destructive revert proceeds (no
// 409), here dropping the newest fanart slot.
func TestHandleImageRevert_NilGateProceeds(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithPlatform(t)
	// No gate configured -> strict gate returns true without evaluating.
	r.conflictGate = nil
	// Wire an event bus so revertSideEffects publishes the ArtistUpdated event
	// (exercises the eventBus-non-nil branch).
	bus := event.NewBus(r.logger, 1024)
	go bus.Start()
	t.Cleanup(bus.Stop)
	r.eventBus = bus
	dir := t.TempDir()
	a := &artist.Artist{Name: "Nil Gate", SortName: "Nil Gate", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	writeJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)
	writeJPEG(t, filepath.Join(dir, "fanart1.jpg"), 1920, 1080)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fanart/revert", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "fanart")
	w := httptest.NewRecorder()
	r.handleImageRevert(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "fanart1.jpg")); !os.IsNotExist(err) {
		t.Errorf("newest fanart slot should be dropped, stat err = %v", err)
	}
}
