package image

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func solidImage(w, h int, c color.Color) image.Image { //nolint:unparam // test helper, w varies conceptually
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

func encodeImage(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding image: %v", err)
	}
	return buf.Bytes()
}

func TestPerceptualHash_IdenticalImages(t *testing.T) {
	img := solidImage(100, 100, color.RGBA{R: 128, G: 64, B: 32, A: 255})
	data := encodeImage(t, img)

	h1, err := PerceptualHash(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("PerceptualHash: %v", err)
	}
	h2, err := PerceptualHash(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("PerceptualHash: %v", err)
	}

	if h1 != h2 {
		t.Errorf("identical images produced different hashes: %x vs %x", h1, h2)
	}
	if Similarity(h1, h2) != 1.0 {
		t.Errorf("identical hashes should have similarity 1.0, got %f", Similarity(h1, h2))
	}
}

func TestPerceptualHash_SolidImages(t *testing.T) {
	// Solid images have no gradient, so dHash produces all-zero hashes
	// regardless of color. Both should hash identically.
	black := solidImage(100, 100, color.Black)
	white := solidImage(100, 100, color.White)

	h1 := PerceptualHashFromImage(black)
	h2 := PerceptualHashFromImage(white)

	if h1 != h2 {
		t.Errorf("solid black hash (%x) should equal solid white hash (%x) -- dHash has no gradient in either", h1, h2)
	}
	if Similarity(h1, h2) != 1.0 {
		t.Errorf("solid images should have similarity 1.0, got %f", Similarity(h1, h2))
	}
}

func TestPerceptualHash_CheckerboardVsSolid(t *testing.T) {
	// Create a checkerboard image (alternating black/white blocks)
	checker := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			if (x/10+y/10)%2 == 0 {
				checker.Set(x, y, color.White)
			} else {
				checker.Set(x, y, color.Black)
			}
		}
	}

	solid := solidImage(100, 100, color.RGBA{R: 128, G: 128, B: 128, A: 255})

	h1 := PerceptualHashFromImage(checker)
	h2 := PerceptualHashFromImage(solid)

	sim := Similarity(h1, h2)
	// A checkerboard should differ meaningfully from a solid gray
	if sim > 0.9 {
		t.Errorf("checkerboard vs solid similarity = %.2f, expected < 0.9", sim)
	}
}

func TestPerceptualHash_ScaledVersions(t *testing.T) {
	// Create a pattern image at two different sizes
	pattern := image.NewRGBA(image.Rect(0, 0, 200, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 200; x++ {
			if (x/20+y/20)%2 == 0 {
				pattern.Set(x, y, color.White)
			} else {
				pattern.Set(x, y, color.Black)
			}
		}
	}

	small := image.NewRGBA(image.Rect(0, 0, 50, 50))
	for y := 0; y < 50; y++ {
		for x := 0; x < 50; x++ {
			if (x/5+y/5)%2 == 0 {
				small.Set(x, y, color.White)
			} else {
				small.Set(x, y, color.Black)
			}
		}
	}

	h1 := PerceptualHashFromImage(pattern)
	h2 := PerceptualHashFromImage(small)

	sim := Similarity(h1, h2)
	// Same pattern at different scales should have high similarity
	if sim < 0.7 {
		t.Errorf("scaled versions similarity = %.2f, expected >= 0.7", sim)
	}
}

func TestHammingDistance(t *testing.T) {
	tests := []struct {
		a, b uint64
		want int
	}{
		{0, 0, 0},
		{0, 1, 1},
		{0xFFFFFFFFFFFFFFFF, 0, 64},
		{0x0F0F0F0F0F0F0F0F, 0xF0F0F0F0F0F0F0F0, 64},
		{0xFF, 0xFE, 1},
	}
	for _, tt := range tests {
		got := HammingDistance(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("HammingDistance(%x, %x) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestSimilarity(t *testing.T) {
	if got := Similarity(0, 0); got != 1.0 {
		t.Errorf("Similarity(0, 0) = %f, want 1.0", got)
	}
	if got := Similarity(0, 0xFFFFFFFFFFFFFFFF); got != 0.0 {
		t.Errorf("Similarity(0, max) = %f, want 0.0", got)
	}
	// 32 bits different = 50% similarity
	if got := Similarity(0, 0xFFFFFFFF); got != 0.5 {
		t.Errorf("Similarity(0, 0xFFFFFFFF) = %f, want 0.5", got)
	}
}

func TestHashHex_RoundTrip(t *testing.T) {
	original := uint64(0xDEADBEEFCAFE1234)
	hex := HashHex(original)
	if hex != "deadbeefcafe1234" {
		t.Errorf("HashHex = %q, want %q", hex, "deadbeefcafe1234")
	}

	parsed, err := ParseHashHex(hex)
	if err != nil {
		t.Fatalf("ParseHashHex: %v", err)
	}
	if parsed != original {
		t.Errorf("round-trip failed: got %x, want %x", parsed, original)
	}
}
