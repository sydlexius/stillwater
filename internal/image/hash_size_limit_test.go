package image

import (
	"bytes"
	"errors"
	gimage "image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// writeSparseFile creates a file of exactly size bytes without writing size
// bytes: Truncate leaves a sparse hole, so the fixture costs no real disk and
// no test-side allocation. This is what lets the test exercise the REAL
// maxDecodeBytes bound instead of a lowered test-only constant -- there is no
// production/test skew to reason about, and no multi-GB fixture.
func writeSparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating sparse fixture: %v", err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(size); err != nil {
		t.Fatalf("truncating sparse fixture to %d: %v", size, err)
	}
}

// A file one byte past the bound must be rejected via the sentinel, and must
// be rejected on the SIZE path -- not incidentally by the decoder failing on
// garbage bytes. Asserting errors.Is(ErrImageTooLarge) is what pins that: a
// hole full of zero bytes is also an undecodable image, so a test that merely
// asserted "err != nil" would pass with the guard removed.
func TestHashFile_OverLimit_ReturnsSentinel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized.jpg")
	writeSparseFile(t, path, maxDecodeBytes+1)

	got, err := HashFile(path, true)
	if err == nil {
		t.Fatal("HashFile on an oversized file: got nil error, want ErrImageTooLarge")
	}
	if !errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("HashFile on an oversized file: got %v, want it to wrap ErrImageTooLarge", err)
	}
	// The guard must fail closed. Returning a content hash computed over a
	// truncated prefix would be worse than returning nothing: it would be a
	// hash that silently does not identify the file it names.
	if got.Content != "" || got.Perceptual != 0 {
		t.Fatalf("HashFile on an oversized file returned hashes %+v, want the zero value", got)
	}
}

// The boundary itself is not an error: a file of exactly maxDecodeBytes is
// within budget. This pins the comparison as > rather than >=, which an
// off-by-one in the guard would flip.
func TestHashFile_AtLimit_NotRejectedForSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atlimit.jpg")
	writeSparseFile(t, path, maxDecodeBytes)

	got, err := HashFile(path, false)
	if err != nil {
		t.Fatalf("HashFile at exactly the limit: unexpected error %v", err)
	}
	if got.Content == "" {
		t.Fatal("HashFile at exactly the limit: got empty content hash, want a hash")
	}
}

// Regression guard: a normal, well-under-limit image still hashes, and hashes
// to the same value the unbounded read produced. ContentHash over the raw file
// bytes is the independent oracle -- if the bounded read silently truncated or
// otherwise altered the byte stream, these would diverge.
func TestHashFile_UnderLimit_HashesUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "small.png")
	// A descending horizontal gradient, so the dHash is genuinely non-zero.
	// makePNG's ASCENDING gradient hashes to 0 legitimately (every pixel is
	// darker than its right neighbor), which would make the assertion below
	// unable to tell a real hash from the "not computed" zero value.
	raw := makeDescendingGradientPNG(t, 32, 32)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	got, err := HashFile(path, true)
	if err != nil {
		t.Fatalf("HashFile on a small valid image: %v", err)
	}
	if want := ContentHash(raw); got.Content != want {
		t.Fatalf("content hash = %s, want %s (bounded read altered the byte stream)", got.Content, want)
	}
	wantPerceptual, err := PerceptualHash(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("computing oracle perceptual hash: %v", err)
	}
	if wantPerceptual == 0 {
		t.Fatal("oracle perceptual hash is 0; the fixture cannot distinguish computed from uncomputed")
	}
	if got.Perceptual != wantPerceptual {
		t.Fatalf("perceptual hash = %#x, want %#x", got.Perceptual, wantPerceptual)
	}
}

// makeDescendingGradientPNG encodes a left-to-right DARKENING gradient, whose
// dHash is non-zero by construction (every pixel is brighter than the one to
// its right, so every comparison bit is set).
func makeDescendingGradientPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := gimage.NewRGBA(gimage.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			v := uint8(255 - (x*255)/w)
			img.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding gradient png: %v", err)
	}
	return buf.Bytes()
}

// An unreadable file must NOT come back as ErrImageTooLarge. The sentinel only
// earns its keep if callers can trust it to mean "oversized" specifically;
// collapsing every failure into it would make it useless for triage.
func TestHashFile_Missing_IsNotTooLarge(t *testing.T) {
	_, err := HashFile(filepath.Join(t.TempDir(), "absent.jpg"), false)
	if err == nil {
		t.Fatal("HashFile on a missing file: got nil error")
	}
	if errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("HashFile on a missing file: got ErrImageTooLarge, want a distinct I/O error (%v)", err)
	}
}
