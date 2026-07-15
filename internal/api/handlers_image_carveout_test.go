package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/rule"
)

// TestHandleImageCrop_AutoLocksSlotBeforeRuleEval is the #2533 API-layer
// regression. A manual crop must auto-lock its image slot BEFORE the
// synchronous post-save rule re-evaluation runs, so the rule-engine carve-out
// (Pipeline.imageSlotProtected) suppresses the same-request auto-fix that
// previously destroyed the operator's crop.
//
// This proves the API-layer half of the fix (the crop handler commits the
// lock before runRulesAfterRefresh). The rule-engine half -- that a protected
// slot survives a full evaluation pass with a real fetch-replace fixer,
// byte-for-byte, with no provider download -- is proven at the rule layer by
// TestPipeline_RunForArtist_ProtectsUserSetImage. Together they close the loop:
// crop locks the slot, and a locked slot is off-limits to the auto-fixer.
func TestHandleImageCrop_AutoLocksSlotBeforeRuleEval(t *testing.T) {
	r, artistSvc, ruleSvc := testRouterWithPipelineFull(t)
	ctx := context.Background()

	// Enable thumb_square in auto mode so the synchronous rule pass actually
	// exercises the image fix path a crop would otherwise trigger.
	sq, err := ruleSvc.GetByID(ctx, rule.RuleThumbSquare)
	if err != nil {
		t.Fatalf("getting thumb_square: %v", err)
	}
	sq.Enabled = true
	sq.AutomationMode = rule.AutomationModeAuto
	if err := ruleSvc.Update(ctx, sq); err != nil {
		t.Fatalf("updating thumb_square: %v", err)
	}

	dir := t.TempDir()
	a := &artist.Artist{Name: "Crop Lock Artist", SortName: "Crop Lock Artist", Path: dir}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// A deliberately NON-SQUARE thumb -- exactly the crop that trips
	// thumb_square and, before the fix, got clobbered.
	src := image.NewRGBA(image.Rect(0, 0, 600, 400))
	var jpegBuf bytes.Buffer
	if err := jpeg.Encode(&jpegBuf, src, nil); err != nil {
		t.Fatalf("encoding JPEG: %v", err)
	}
	reqBody, err := json.Marshal(map[string]string{
		"image_data": base64.StdEncoding.EncodeToString(jpegBuf.Bytes()),
		"type":       "thumb",
	})
	if err != nil {
		t.Fatalf("marshaling body: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/crop", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleImageCrop(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("crop status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	// The cropped thumb is on disk (default thumb name is folder.jpg) and
	// survived the synchronous post-crop rule pass with its NON-SQUARE geometry
	// intact -- a thumb_square auto-fix would have replaced it with a square.
	thumbPath := filepath.Join(dir, "folder.jpg")
	saved, err := os.ReadFile(thumbPath)
	if err != nil {
		t.Fatalf("cropped thumb not on disk: %v", err)
	}
	if len(saved) == 0 {
		t.Fatal("cropped thumb is empty")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(saved))
	if err != nil {
		t.Fatalf("decoding surviving thumb: %v", err)
	}
	if cfg.Width == cfg.Height {
		t.Errorf("thumb is now square (%dx%d) -- the operator's non-square crop was replaced by an auto-fix", cfg.Width, cfg.Height)
	}

	// The slot is auto-locked -- this is the API-layer behavior under test.
	// finalizeImageSave must have committed the lock before returning (and,
	// critically, before runRulesAfterRefresh ran).
	imgs, err := artistSvc.GetImagesForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetImagesForArtist: %v", err)
	}
	var thumb *artist.ArtistImage
	for i := range imgs {
		if imgs[i].ImageType == "thumb" && imgs[i].SlotIndex == 0 {
			thumb = &imgs[i]
		}
	}
	if thumb == nil {
		t.Fatal("no thumb slot row after crop")
	}
	if !thumb.Locked {
		t.Error("thumb slot was not auto-locked after a manual crop; the same-request auto-fixer is not suppressed")
	}
}

// TestHandleImageCrop_PerSlotFanart_LocksEditedSlotNotZero guards the #2533
// fix against a slot-targeting bug: a per-slot fanart crop (#2281) must
// auto-lock the slot it actually wrote, not slot 0. An earlier revision locked
// slot 0 unconditionally for every non-append save, which both wrongly locked
// an untouched image (permanently suppressing its legitimate auto-fixes) and
// left the operator's edited slot unprotected.
func TestHandleImageCrop_PerSlotFanart_LocksEditedSlotNotZero(t *testing.T) {
	// The auto-lock happens in finalizeImageSave before rule re-eval, so no
	// pipeline is needed; but the per-slot fanart path resolves naming through
	// the platform service, so use the platform-wired router.
	r, artistSvc := testRouterWithPlatform(t)
	ctx := context.Background()

	dir := t.TempDir()
	a := &artist.Artist{Name: "Fanart Slot Artist", SortName: "Fanart Slot Artist", Path: dir, FanartExists: true, FanartCount: 2}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	// Two fanart slots on disk (Kodi numbering) so slot 1 is a valid target,
	// with matching artist_images rows so the lock has a row to land on.
	writeJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)
	writeJPEG(t, filepath.Join(dir, "fanart1.jpg"), 1920, 1080)
	for _, slot := range []int{0, 1} {
		if err := artistSvc.UpsertImage(ctx, &artist.ArtistImage{
			ArtistID: a.ID, ImageType: "fanart", SlotIndex: slot, Exists: true,
		}); err != nil {
			t.Fatalf("seeding fanart slot %d: %v", slot, err)
		}
	}

	src := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	var jpegBuf bytes.Buffer
	if err := jpeg.Encode(&jpegBuf, src, nil); err != nil {
		t.Fatalf("encoding JPEG: %v", err)
	}
	slot := 1
	reqBody, err := json.Marshal(map[string]any{
		"image_data": base64.StdEncoding.EncodeToString(jpegBuf.Bytes()),
		"type":       "fanart",
		"slot":       slot,
	})
	if err != nil {
		t.Fatalf("marshaling body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/crop", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleImageCrop(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("crop status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	imgs, err := artistSvc.GetImagesForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetImagesForArtist: %v", err)
	}
	locked := map[int]bool{}
	for _, im := range imgs {
		if im.ImageType == "fanart" {
			locked[im.SlotIndex] = im.Locked
		}
	}
	if !locked[1] {
		t.Error("edited fanart slot 1 was not auto-locked")
	}
	if locked[0] {
		t.Error("fanart slot 0 was wrongly auto-locked; the crop targeted slot 1, not slot 0")
	}
}
