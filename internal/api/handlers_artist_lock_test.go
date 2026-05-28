package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/nfo"
)

// TestHandleLockArtistField exercises the POST and DELETE field-lock
// endpoints. It uses the handler functions directly, setting path values via
// SetPathValue because the tests bypass the router's pattern matching.
func TestHandleLockArtistField(t *testing.T) {
	t.Parallel()
	r, svc := testRouter(t)
	ctx := context.Background()

	a := &artist.Artist{Name: "Lock Target", Path: "/music/lock-target"}
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Lock the biography field.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/field-locks/biography", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("field", "biography")
	w := httptest.NewRecorder()
	r.handleLockArtistField(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("lock status = %d, body = %s", w.Code, w.Body.String())
	}

	var locked artist.Artist
	if err := json.Unmarshal(w.Body.Bytes(), &locked); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(locked.LockedFields) != 1 || locked.LockedFields[0] != "biography" {
		t.Errorf("expected [biography], got %v", locked.LockedFields)
	}

	// Unlock the field.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/field-locks/biography", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("field", "biography")
	w = httptest.NewRecorder()
	r.handleUnlockArtistField(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("unlock status = %d, body = %s", w.Code, w.Body.String())
	}
	var unlocked artist.Artist
	if err := json.Unmarshal(w.Body.Bytes(), &unlocked); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(unlocked.LockedFields) != 0 {
		t.Errorf("expected empty locked_fields, got %v", unlocked.LockedFields)
	}
}

// TestHandleLockArtistField_NotFound verifies the handler emits a 404 when
// the artist does not exist.
func TestHandleLockArtistField_NotFound(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/missing/field-locks/biography", nil)
	req.SetPathValue("id", "missing")
	req.SetPathValue("field", "biography")
	w := httptest.NewRecorder()
	r.handleLockArtistField(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// TestHandleLockArtistImage verifies per-image lock toggling and the
// cross-artist ownership check.
func TestHandleLockArtistImage(t *testing.T) {
	t.Parallel()
	r, svc := testRouter(t)
	ctx := context.Background()

	a := &artist.Artist{Name: "Image Lock", Path: "/music/image-lock"}
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	img := &artist.ArtistImage{
		ArtistID:  a.ID,
		ImageType: "thumb",
		SlotIndex: 0,
		Exists:    true,
	}
	if err := svc.UpsertImage(ctx, img); err != nil {
		t.Fatalf("UpsertImage: %v", err)
	}
	imgs, err := svc.GetImagesForArtist(ctx, a.ID)
	if err != nil || len(imgs) != 1 {
		t.Fatalf("GetImagesForArtist: %v, len=%d", err, len(imgs))
	}
	imgID := imgs[0].ID

	// Lock.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/image-locks/"+imgID, nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("imageId", imgID)
	w := httptest.NewRecorder()
	r.handleLockArtistImage(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("lock status = %d, body = %s", w.Code, w.Body.String())
	}
	imgs, _ = svc.GetImagesForArtist(ctx, a.ID)
	if !imgs[0].Locked {
		t.Error("expected image to be locked after POST")
	}

	// Wrong image id should return 404.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/image-locks/bogus", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("imageId", "bogus")
	w = httptest.NewRecorder()
	r.handleLockArtistImage(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("wrong-image status = %d, want 404", w.Code)
	}

	// Unlock.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/image-locks/"+imgID, nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("imageId", imgID)
	w = httptest.NewRecorder()
	r.handleUnlockArtistImage(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("unlock status = %d, body = %s", w.Code, w.Body.String())
	}
	imgs, _ = svc.GetImagesForArtist(ctx, a.ID)
	if imgs[0].Locked {
		t.Error("expected image to be unlocked after DELETE")
	}
}

// TestHandleUnlockArtistField_NotFound verifies 404 from the DELETE endpoint
// when the artist does not exist.
func TestHandleUnlockArtistField_NotFound(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/missing/field-locks/biography", nil)
	req.SetPathValue("id", "missing")
	req.SetPathValue("field", "biography")
	w := httptest.NewRecorder()
	r.handleUnlockArtistField(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// TestHandleLockArtistImage_MissingArtist verifies 404 when the artist id in
// the path does not resolve to any images (GetImagesForArtist returns empty,
// so the ownership check fails).
func TestHandleLockArtistImage_MissingArtist(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/missing/image-locks/img-1", nil)
	req.SetPathValue("id", "missing")
	req.SetPathValue("imageId", "img-1")
	w := httptest.NewRecorder()
	r.handleLockArtistImage(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// TestHandleLockArtist exercises the whole-artist lock/unlock round trip.
// POST sets the lock, GET confirms it, DELETE removes it. PushLocks runs
// inline; without a configured connection it is a no-op.
func TestHandleLockArtist(t *testing.T) {
	t.Parallel()
	r, svc := testRouter(t)
	ctx := context.Background()
	a := &artist.Artist{Name: "Wholesale Lock"}
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/lock", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleLockArtist(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("lock status = %d; body = %s", w.Code, w.Body.String())
	}
	var locked artist.Artist
	if err := json.Unmarshal(w.Body.Bytes(), &locked); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !locked.Locked {
		t.Error("expected Locked=true after handleLockArtist")
	}

	// Second lock should 409 since AlreadyLocked is surfaced explicitly.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/lock", nil)
	req.SetPathValue("id", a.ID)
	w = httptest.NewRecorder()
	r.handleLockArtist(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("double-lock status = %d, want 409", w.Code)
	}

	// Unlock path.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/lock", nil)
	req.SetPathValue("id", a.ID)
	w = httptest.NewRecorder()
	r.handleUnlockArtist(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("unlock status = %d; body = %s", w.Code, w.Body.String())
	}
	var unlocked artist.Artist
	if err := json.Unmarshal(w.Body.Bytes(), &unlocked); err != nil {
		t.Fatalf("decode unlock response: %v", err)
	}
	if unlocked.Locked {
		t.Error("expected Locked=false in handleUnlockArtist response body")
	}
	// Unlock-when-not-locked is also a 409.
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/lock", nil)
	req.SetPathValue("id", a.ID)
	r.handleUnlockArtist(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("double-unlock status = %d, want 409", w.Code)
	}
}

func TestHandleLockArtist_NotFound(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/missing/lock", nil)
	req.SetPathValue("id", "missing")
	w := httptest.NewRecorder()
	r.handleLockArtist(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandleUnlockArtist_NotFound(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/missing/lock", nil)
	req.SetPathValue("id", "missing")
	w := httptest.NewRecorder()
	r.handleUnlockArtist(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// TestHandleLockArtist_RewritesNFO is the regression fixture for issue #1726.
// Locking an artist via the API must rewrite the on-disk NFO so its
// <lockdata> matches the new DB state; otherwise the next scan would
// re-import the stale value and undo the toggle.
func TestHandleLockArtist_RewritesNFO(t *testing.T) {
	t.Parallel()
	r, svc := testRouter(t)
	ctx := context.Background()

	dir := t.TempDir()
	a := &artist.Artist{Name: "Lock NFO Rewrite", Path: dir}
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Seed an NFO with lockdata=false so the lock path has something to
	// overwrite. The handler's WriteBackNFO is a no-op when no NFO is
	// present on disk (that is by design: creating new NFOs is the rule
	// engine's job, not the lock handler's).
	nfoPath := filepath.Join(dir, "artist.nfo")
	seed := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<artist>
  <name>Lock NFO Rewrite</name>
  <lockdata>false</lockdata>
</artist>`)
	if err := os.WriteFile(nfoPath, seed, 0o644); err != nil {
		t.Fatalf("seed NFO: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/lock", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleLockArtist(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("lock status = %d, body = %s", w.Code, w.Body.String())
	}

	data, err := os.ReadFile(nfoPath)
	if err != nil {
		t.Fatalf("read NFO: %v", err)
	}
	parsed, err := nfo.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parse NFO: %v", err)
	}
	if !parsed.LockData {
		t.Error("NFO LockData = false after lock; expected true (#1726)")
	}

	// Unlock and verify the NFO flips back.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/lock", nil)
	req.SetPathValue("id", a.ID)
	w = httptest.NewRecorder()
	r.handleUnlockArtist(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("unlock status = %d, body = %s", w.Code, w.Body.String())
	}
	data, err = os.ReadFile(nfoPath)
	if err != nil {
		t.Fatalf("read NFO after unlock: %v", err)
	}
	parsed, err = nfo.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parse NFO after unlock: %v", err)
	}
	if parsed.LockData {
		t.Error("NFO LockData = true after unlock; expected false (#1726)")
	}
	// Belt-and-braces: also assert the literal element disappeared from
	// the serialized form, so a future parser change cannot mask a
	// regression by always returning false for the LockData field.
	if strings.Contains(string(data), "<lockdata>true</lockdata>") {
		t.Errorf("NFO still contains <lockdata>true</lockdata> after unlock:\n%s", data)
	}
}

// TestHandleUnlockArtistImage_WrongImage verifies 404 when the imageId does
// not belong to the artist on the DELETE path.
func TestHandleUnlockArtistImage_WrongImage(t *testing.T) {
	t.Parallel()
	r, svc := testRouter(t)
	ctx := context.Background()
	a := &artist.Artist{Name: "Unlock Wrong", Path: "/music/unlock-wrong"}
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/image-locks/bogus", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("imageId", "bogus")
	w := httptest.NewRecorder()
	r.handleUnlockArtistImage(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}
