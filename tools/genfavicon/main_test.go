package main

import (
	"errors"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// repoSVG is the master SVG relative to this package directory.
const repoSVG = "../../web/static/img/favicon.svg"

// setupDir copies the real master SVG into a fresh temp dir and returns it.
func setupDir(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(repoSVG)
	if err != nil {
		t.Fatalf("read master SVG: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, svgName), src, 0o600); err != nil {
		t.Fatalf("write SVG into temp dir: %v", err)
	}
	return dir
}

func decodePNG(t *testing.T, path string) image.Image {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return img
}

// TestRunEmitsAllTargets verifies run() rasterizes every target PNG at the
// correct dimensions from the master SVG.
func TestRunEmitsAllTargets(t *testing.T) {
	dir := setupDir(t)
	if err := run(dir); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, tg := range targets {
		img := decodePNG(t, filepath.Join(dir, tg.name))
		b := img.Bounds()
		if b.Dx() != tg.size || b.Dy() != tg.size {
			t.Errorf("%s: got %dx%d, want %dx%d", tg.name, b.Dx(), b.Dy(), tg.size, tg.size)
		}
	}
}

// TestRenderFidelity checks the rendered mark at 512px: a transparent corner
// (outside the squircle), the #2563eb blue background, and white glyph pixels.
func TestRenderFidelity(t *testing.T) {
	dir := setupDir(t)
	if err := run(dir); err != nil {
		t.Fatalf("run: %v", err)
	}
	img := decodePNG(t, filepath.Join(dir, "android-chrome-512x512.png"))

	// Corner is outside the n=4 squircle -> fully transparent.
	if _, _, _, a := img.At(1, 1).RGBA(); a != 0 {
		t.Errorf("corner (1,1) alpha = %d, want 0 (outside squircle)", a>>8)
	}

	// Scan for the blue background and white glyph across the image.
	var haveBlue, haveWhite bool
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y && (!haveBlue || !haveWhite); y += 4 {
		for x := bounds.Min.X; x < bounds.Max.X; x += 4 {
			r, g, b, a := img.At(x, y).RGBA()
			if a>>8 < 250 {
				continue
			}
			r8, g8, b8 := r>>8, g>>8, b>>8
			if near(r8, 0x25) && near(g8, 0x63) && near(b8, 0xeb) {
				haveBlue = true
			}
			if r8 > 245 && g8 > 245 && b8 > 245 {
				haveWhite = true
			}
		}
	}
	if !haveBlue {
		t.Error("no #2563eb blue background pixels found")
	}
	if !haveWhite {
		t.Error("no white glyph pixels found")
	}
}

// TestRunMissingSVG surfaces a read error when the master SVG is absent.
func TestRunMissingSVG(t *testing.T) {
	if err := run(t.TempDir()); err == nil {
		t.Fatal("expected error for missing SVG, got nil")
	}
}

// TestWritePNGCreateError covers the os.Create failure branch.
func TestWritePNGCreateError(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	// A path whose parent directory does not exist -> os.Create fails.
	if err := writePNG(img, filepath.Join(t.TempDir(), "nope", "x.png")); err == nil {
		t.Fatal("expected create error, got nil")
	}
}

// TestRunMkdirError covers the MkdirAll failure branch: a directory path that
// collides with an existing regular file cannot be created.
func TestRunMkdirError(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if err := run(filepath.Join(f, "sub")); err == nil {
		t.Fatal("expected mkdir error, got nil")
	}
}

// TestRunEncodeError covers the encode-failure path in writePNG and the
// resulting error return in run().
func TestRunEncodeError(t *testing.T) {
	dir := setupDir(t)
	orig := encodePNG
	t.Cleanup(func() { encodePNG = orig })
	encodePNG = func(io.Writer, image.Image) error { return errors.New("boom") }
	if err := run(dir); err == nil {
		t.Fatal("expected encode error, got nil")
	}
}

func near(v, target uint32) bool {
	d := int(v) - int(target)
	if d < 0 {
		d = -d
	}
	return d <= 12
}
