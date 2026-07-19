package api

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/conflict"
	img "github.com/sydlexius/stillwater/internal/image"
)

func TestHandleImageRevert_SingleSlotRestoresBackup(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "Revert Artist", SortName: "Revert Artist", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	canonical := filepath.Join(dir, "folder.jpg")
	writeJPEG(t, canonical, 64, 64)
	orig, _ := os.ReadFile(canonical)
	if err := img.BackupSingleSlot(dir, "thumb", thumbNaming); err != nil {
		t.Fatalf("seed backup: %v", err)
	}
	// Simulate a destructive edit (different image content).
	writeJPEG(t, canonical, 96, 96)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/thumb/revert", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()
	r.handleImageRevert(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	// Assert the single-slot 200 contract explicitly (status/type), not just the
	// file restore: a handler regression emitting the wrong JSON while still
	// restoring the file on disk would otherwise pass.
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding revert body: %v; body=%s", err, w.Body.String())
	}
	if body["status"] != "reverted" {
		t.Errorf("status field = %v, want reverted", body["status"])
	}
	if body["type"] != "thumb" {
		t.Errorf("type field = %v, want thumb", body["type"])
	}
	got, _ := os.ReadFile(canonical)
	if !bytes.Equal(got, orig) {
		t.Errorf("canonical after revert does not match original (%d vs %d bytes)", len(got), len(orig))
	}
	// sync_warnings must be present in the response (F5).
	if _, ok := body["sync_warnings"]; !ok {
		t.Errorf("revert response missing sync_warnings: %s", w.Body.String())
	}
}

// thumbNaming is the thumb filename list the test router resolves to (no active
// platform profile -> image.DefaultFileNames).
var thumbNaming = img.DefaultFileNames["thumb"]

func TestHandleImageRevert_SingleSlotNoBackup404(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "NoBackup", SortName: "NoBackup", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	writeJPEG(t, filepath.Join(dir, "folder.jpg"), 100, 100)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/thumb/revert", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()
	r.handleImageRevert(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleImageRevert_FanartDropsNewestSlot(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "Fanart Revert", SortName: "Fanart Revert", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	// Three fanart slots: fanart.jpg, fanart1.jpg, fanart2.jpg (T6).
	writeJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)
	writeJPEG(t, filepath.Join(dir, "fanart1.jpg"), 1920, 1080)
	writeJPEG(t, filepath.Join(dir, "fanart2.jpg"), 1920, 1080)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fanart/revert", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "fanart")
	w := httptest.NewRecorder()
	r.handleImageRevert(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	// Assert the user-visible 200 contract (status/type/count), not just the file
	// side effect: a regression in these fields is an API-contract break.
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding revert body: %v; body=%s", err, w.Body.String())
	}
	if body["status"] != "reverted" {
		t.Errorf("status field = %v, want reverted", body["status"])
	}
	if body["type"] != "fanart" {
		t.Errorf("type field = %v, want fanart", body["type"])
	}
	if got, ok := body["count"].(float64); !ok || int(got) != 2 {
		t.Errorf("count field = %v, want 2", body["count"])
	}
	// Only the newest (fanart2.jpg) is dropped; fanart.jpg + fanart1.jpg remain.
	if _, err := os.Stat(filepath.Join(dir, "fanart2.jpg")); !os.IsNotExist(err) {
		t.Errorf("newest fanart slot (fanart2.jpg) should be dropped, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "fanart1.jpg")); err != nil {
		t.Errorf("fanart1.jpg must remain: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "fanart.jpg")); err != nil {
		t.Errorf("original fanart slot must remain: %v", err)
	}
	// sync_warnings must be present in the fanart revert response (F5).
	if _, ok := body["sync_warnings"]; !ok {
		t.Errorf("fanart revert response missing sync_warnings: %s", w.Body.String())
	}
}

func TestHandleImageRevert_FanartNoExtraSlot404(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "Single Fanart", SortName: "Single Fanart", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	writeJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080) // only the original slot
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/fanart/revert", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "fanart")
	w := httptest.NewRecorder()
	r.handleImageRevert(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleImageRevert_InvalidType400(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithPlatform(t)
	a := &artist.Artist{Name: "BadType", SortName: "BadType", Path: t.TempDir()}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/poster/revert", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "poster")
	w := httptest.NewRecorder()
	r.handleImageRevert(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleImageRevert_Blocked409(t *testing.T) {
	t.Parallel()
	d := conflict.NewBlockingForTest(testDiscardLogger())
	r, artistSvc := testRouterWithPlatform(t)
	r.conflictDetector = d
	r.conflictGate = conflict.NewGate(d)
	dir := t.TempDir()
	a := &artist.Artist{Name: "Blocked Revert", SortName: "Blocked Revert", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	canonical := filepath.Join(dir, "folder.jpg")
	writeJPEG(t, canonical, 64, 64)
	canonicalBefore, _ := os.ReadFile(canonical)
	if err := img.BackupSingleSlot(dir, "thumb", thumbNaming); err != nil {
		t.Fatalf("seed backup: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/thumb/revert", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()
	r.handleImageRevert(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", w.Code, w.Body.String())
	}
	// The gated 409 must carry the shared ConflictWriteBlock payload, not a bare
	// error - otherwise a regression dropping the conflict fields slips through.
	assertConflictWriteBlock(t, w.Body.Bytes())
	// T1: a blocked revert must not touch disk - backup survives, canonical unchanged.
	if !img.HasBackup(dir, "thumb") {
		t.Error("blocked revert must leave the backup intact")
	}
	canonicalAfter, _ := os.ReadFile(canonical)
	if !bytes.Equal(canonicalBefore, canonicalAfter) {
		t.Error("blocked revert must not overwrite the canonical image")
	}
}

func TestHandleImageInfo_ReportsBackupExists(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "Info Backup", SortName: "Info Backup", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	canonical := filepath.Join(dir, "folder.jpg")
	writeJPEG(t, canonical, 64, 64)

	infoBackupExists := func(imageType string) bool {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/images/"+imageType+"/info", nil)
		req.SetPathValue("id", a.ID)
		req.SetPathValue("type", imageType)
		w := httptest.NewRecorder()
		r.handleImageInfo(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("info status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decoding info body: %v; body=%s", err, w.Body.String())
		}
		v, ok := body["backup_exists"].(bool)
		if !ok {
			t.Fatalf("backup_exists missing or not a bool in %s", w.Body.String())
		}
		return v
	}

	// No backup yet -> backup_exists false (typed bool, T7).
	if infoBackupExists("thumb") {
		t.Error("want backup_exists=false before any backup")
	}

	// After a backup exists -> backup_exists true.
	if err := img.BackupSingleSlot(dir, "thumb", thumbNaming); err != nil {
		t.Fatalf("seed backup: %v", err)
	}
	if !infoBackupExists("thumb") {
		t.Error("want backup_exists=true after backup")
	}

	// T7: fanart info reports backup_exists=false even with a stray .sw-backup
	// entry, because fanart is multi-slot and has no single-slot backup.
	writeJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)
	strayDir := filepath.Join(dir, img.BackupDirName, "fanart")
	if err := os.MkdirAll(strayDir, 0o750); err != nil {
		t.Fatalf("creating stray backup dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(strayDir, "fanart.jpg"), []byte("stray"), 0o644); err != nil {
		t.Fatalf("writing stray backup: %v", err)
	}
	if infoBackupExists("fanart") {
		t.Error("fanart info must report backup_exists=false even with a stray backup entry")
	}
}

// TestHandleImageRevert_CropRoundTrip exercises the full crop -> revert flow
// through the handlers and asserts the post-revert canonical matches the
// pre-crop original (T3), including a png->jpg variant proving F4.
func TestHandleImageRevert_CropRoundTrip(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		origName string
		origFmt  string
		cropFmt  string
		// staleName is the post-crop canonical written under the crop format's
		// name when it differs from origName (format-changing crop). Revert must
		// remove it via CleanupConflictingFormats. Empty when crop keeps origName.
		staleName string
	}{
		{"jpg-to-jpg", "folder.jpg", "jpeg", "jpeg", ""},
		{"png-to-jpg", "folder.png", "png", "jpeg", "folder.jpg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, artistSvc := testRouterWithPlatform(t)
			dir := t.TempDir()
			a := &artist.Artist{Name: "RoundTrip", SortName: "RoundTrip", Path: dir}
			if err := artistSvc.Create(context.Background(), a); err != nil {
				t.Fatalf("creating artist: %v", err)
			}
			origPath := filepath.Join(dir, tc.origName)
			writeImageFmt(t, origPath, tc.origFmt, 80, 80)
			orig, _ := os.ReadFile(origPath)

			// Crop: process+save a different-content image of the crop format.
			cropData := encodeImageFmt(t, tc.cropFmt, 120, 120)
			if _, err := r.processAndSaveImage(context.Background(), nil, dir, "thumb", cropData, nil); err != nil {
				t.Fatalf("crop save: %v", err)
			}

			// Revert through the handler.
			req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/thumb/revert", nil)
			req.SetPathValue("id", a.ID)
			req.SetPathValue("type", "thumb")
			w := httptest.NewRecorder()
			r.handleImageRevert(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("revert status = %d, want 200; body: %s", w.Code, w.Body.String())
			}
			// Original basename + format must be restored byte-for-byte.
			got, err := os.ReadFile(origPath)
			if err != nil {
				t.Fatalf("reading restored original at %s: %v", origPath, err)
			}
			if !bytes.Equal(got, orig) {
				t.Errorf("post-revert canonical does not equal pre-crop original (%d vs %d bytes)", len(got), len(orig))
			}
			// Format-changing revert must also CLEAN UP the stale post-crop file
			// (e.g. the folder.jpg written over a png original); leaving it behind
			// would let two canonical formats coexist (F4).
			if tc.staleName != "" {
				if _, err := os.Stat(filepath.Join(dir, tc.staleName)); !os.IsNotExist(err) {
					t.Errorf("stale post-crop file %s must be removed on revert, stat err = %v", tc.staleName, err)
				}
			}
		})
	}
}

// TestHandleImageRevert_OneDeep crops twice and asserts exactly one backup
// remains holding the FIRST crop's output, and revert returns to that
// intermediate state (T4).
func TestHandleImageRevert_OneDeep(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := &artist.Artist{Name: "OneDeep", SortName: "OneDeep", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	origPath := filepath.Join(dir, "folder.jpg")
	writeJPEG(t, origPath, 60, 60)

	// First crop: backs up the original; canonical becomes crop1.
	crop1 := encodeImageFmt(t, "jpeg", 100, 100)
	if _, err := r.processAndSaveImage(context.Background(), nil, dir, "thumb", crop1, nil); err != nil {
		t.Fatalf("crop1: %v", err)
	}
	intermediate, _ := os.ReadFile(origPath)

	// Second crop: backs up crop1 (one-deep replaces the original backup).
	crop2 := encodeImageFmt(t, "jpeg", 140, 140)
	if _, err := r.processAndSaveImage(context.Background(), nil, dir, "thumb", crop2, nil); err != nil {
		t.Fatalf("crop2: %v", err)
	}

	// Exactly one backup file remains.
	typeDir := filepath.Join(dir, img.BackupDirName, "thumb")
	entries, err := os.ReadDir(typeDir)
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one backup after two crops, got %d", len(entries))
	}

	// Revert returns to the intermediate (first-crop) state, not the original.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/thumb/revert", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()
	r.handleImageRevert(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("revert status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	got, _ := os.ReadFile(origPath)
	if !bytes.Equal(got, intermediate) {
		t.Errorf("one-deep revert should restore the FIRST crop, got %d bytes want %d", len(got), len(intermediate))
	}
}

// encodeImageFmt returns encoded image bytes of the given format and size.
func encodeImageFmt(t *testing.T, format string, w, h int) []byte {
	t.Helper()
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			m.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 64, A: 255})
		}
	}
	var buf bytes.Buffer
	var err error
	if format == "png" {
		err = png.Encode(&buf, m)
	} else {
		err = jpeg.Encode(&buf, m, &jpeg.Options{Quality: 90})
	}
	if err != nil {
		t.Fatalf("encoding %s: %v", format, err)
	}
	return buf.Bytes()
}

// writeImageFmt writes an encoded image of the given format to path.
func writeImageFmt(t *testing.T, path, format string, w, h int) {
	t.Helper()
	if err := os.WriteFile(path, encodeImageFmt(t, format, w, h), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
