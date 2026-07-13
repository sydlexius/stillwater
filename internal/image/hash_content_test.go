package image

// Coverage for the exact-hash primitive added for #2341 and the single-read
// shape it exists to support (#2349).

import (
	"bytes"
	stdimage "image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

func writeTestJPEG(t *testing.T, path string, variant, quality int) []byte {
	t.Helper()
	const w, h = 120, 90
	img := stdimage.NewRGBA(stdimage.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8((x*255/w + variant*53) % 256)
			img.Set(x, y, color.RGBA{R: v, G: 255 - v, B: 64, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatalf("encoding jpeg: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return buf.Bytes()
}

func TestContentHash_IdenticalBytesMatch_DifferentBytesDoNot(t *testing.T) {
	a := []byte("the same bytes")
	b := []byte("the same bytes")
	c := []byte("the same bytes.")

	if ContentHash(a) != ContentHash(b) {
		t.Error("identical byte sequences produced different content hashes")
	}
	if ContentHash(a) == ContentHash(c) {
		t.Error("a one-byte difference produced the same content hash")
	}
	if len(ContentHash(a)) != 64 {
		t.Errorf("content hash is %d chars, want 64 (hex sha256)", len(ContentHash(a)))
	}
}

// TestHashFile_DecodesOnlyWhenAsked is the cost property the duplicate rules
// depend on: the content hash is always available from a read, but the decode
// (the expensive half) happens only when the caller actually needs it.
func TestHashFile_DecodesOnlyWhenAsked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fanart.jpg")
	raw := writeTestJPEG(t, path, 1, 90)

	// Without the perceptual hash: content hash present, perceptual zero.
	cheap, err := HashFile(path, false)
	if err != nil {
		t.Fatalf("HashFile(needPerceptual=false): %v", err)
	}
	if cheap.Content != ContentHash(raw) {
		t.Error("content hash does not match a hash of the file's bytes")
	}
	if cheap.Perceptual != 0 {
		t.Errorf("Perceptual = %d; want 0 when the decode was not requested", cheap.Perceptual)
	}

	// With it: both present.
	full, err := HashFile(path, true)
	if err != nil {
		t.Fatalf("HashFile(needPerceptual=true): %v", err)
	}
	if full.Content != cheap.Content {
		t.Error("content hash differs between the decoding and non-decoding paths")
	}
	if full.Perceptual == 0 {
		t.Error("Perceptual = 0; want a real hash when the decode was requested")
	}
}

// TestHashFile_ByteIdenticalCopiesHashAlike is the exact rule's core premise.
func TestHashFile_ByteIdenticalCopiesHashAlike(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "fanart.jpg")
	copied := filepath.Join(dir, "fanart2.jpg")

	raw := writeTestJPEG(t, original, 2, 90)
	if err := os.WriteFile(copied, raw, 0o644); err != nil {
		t.Fatalf("copying file: %v", err)
	}

	a, err := HashFile(original, false)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}
	b, err := HashFile(copied, false)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}
	if a.Content != b.Content {
		t.Error("byte-identical copies produced different content hashes")
	}
}

// TestHashFile_ReencodedCopyDiffersByBytesButNotByEye documents the exact
// tier's blind spot, which is the entire reason the perceptual tier is kept.
func TestHashFile_ReencodedCopyDiffersByBytesButNotByEye(t *testing.T) {
	dir := t.TempDir()
	high := filepath.Join(dir, "fanart.jpg")
	low := filepath.Join(dir, "fanart2.jpg")

	writeTestJPEG(t, high, 3, 95)
	writeTestJPEG(t, low, 3, 55)

	a, err := HashFile(high, true)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}
	b, err := HashFile(low, true)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}

	if a.Content == b.Content {
		t.Fatal("fixture is wrong: the two encodings must differ byte-wise")
	}
	// Same picture: the perceptual hashes must agree even though the bytes do not.
	if sim := Similarity(a.Perceptual, b.Perceptual); sim < 0.90 {
		t.Errorf("perceptual similarity = %.2f; the same image re-encoded should still "+
			"read as a duplicate to the perceptual tier", sim)
	}
}

func TestHashFile_MissingFileErrors(t *testing.T) {
	if _, err := HashFile(filepath.Join(t.TempDir(), "nope.jpg"), true); err == nil {
		t.Error("HashFile on a missing file returned nil error")
	}
}

// TestHashFile_UndecodableFileStillYieldsContentHash: a file that cannot be
// decoded can still be byte-compared, so the content hash must survive the
// decode failure rather than being thrown away with it.
func TestHashFile_UndecodableFileStillYieldsContentHash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.jpg")
	junk := []byte("this is definitely not a JPEG")
	if err := os.WriteFile(path, junk, 0o644); err != nil {
		t.Fatalf("writing junk file: %v", err)
	}

	h, err := HashFile(path, true)
	if err == nil {
		t.Error("expected a decode error for an undecodable file")
	}
	if h.Content != ContentHash(junk) {
		t.Error("content hash was discarded along with the decode failure; an " +
			"undecodable file can still be compared byte-for-byte")
	}
}
