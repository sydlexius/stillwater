package api

// Additional coverage for the fanart save-error branch of processAndSaveImage.
// The happy paths live in handlers_image_coverage_test.go and
// handlers_image_test.go; this pins the fanart-specific error surface.
// (degenerateTrimReason is already covered by TestDegenerateTrimReason in
// handlers_image_test.go.)

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// jpegBytes returns an encoded JPEG of the given size for feeding image
// pipelines that expect real decodable image data.
func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			m.Set(x, y, color.RGBA{R: 90, G: 140, B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, m, nil); err != nil {
		t.Fatalf("encoding JPEG: %v", err)
	}
	return buf.Bytes()
}

func TestProcessAndSaveImage_FanartSaveError(t *testing.T) {
	t.Parallel()
	r, _ := newImageHandlerTestServer(t)

	// Point the "directory" at a regular file so nothing can be written into it.
	//
	// This assertion used to demand a plain "saving" error, on the reasoning that
	// "fanart takes the no-backup branch". That branch was the #2413 defect: fanart
	// was the one image type whose OVERWRITE could destroy the user's artwork without
	// a backup. It is now protected like every other type, so the strict backup probe
	// (#1161) fails FIRST and we ABORT BEFORE the destructive save -- which is the
	// point. The original must never be destroyed just because we could not verify it.
	tmp := t.TempDir()
	fileAsDir := filepath.Join(tmp, "not-a-dir")
	if err := os.WriteFile(fileAsDir, []byte("x"), 0o644); err != nil {
		t.Fatalf("seeding blocker file: %v", err)
	}

	saved, err := r.processAndSaveImage(context.Background(), fileAsDir, "fanart", jpegBytes(t, 60, 40), nil)
	if err == nil {
		t.Fatalf("expected an error when the target dir is a file, got saved=%v", saved)
	}
	if saved != nil {
		t.Errorf("saved = %v; want nil on failure", saved)
	}
	if !strings.Contains(err.Error(), "aborting destructive save") {
		t.Errorf("error = %q; want the strict backup probe to ABORT the destructive save before it "+
			"can destroy an original it could not back up (#2413/#1161)", err.Error())
	}
}
