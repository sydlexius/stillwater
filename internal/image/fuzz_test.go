package image

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// FuzzPerceptualHash feeds arbitrary byte slices to the perceptual hash
// function. It must never panic -- returning an error for invalid image
// data is expected and correct.
func FuzzPerceptualHash(f *testing.F) {
	// Seed with valid PNG images
	f.Add(encodePNG(f, solidImage(10, 10, color.White)))
	f.Add(encodePNG(f, solidImage(100, 100, color.RGBA{R: 128, G: 64, B: 32, A: 255})))
	f.Add(encodePNG(f, checkerboard(50, 50)))

	// Seed with valid JPEG images
	f.Add(encodeJPEG(f, solidImage(10, 10, color.Black)))
	f.Add(encodeJPEG(f, solidImage(100, 100, color.RGBA{R: 200, G: 100, B: 50, A: 255})))

	// Seed with minimal valid images
	f.Add(encodePNG(f, solidImage(1, 1, color.White)))
	f.Add(encodePNG(f, solidImage(9, 8, color.Gray{Y: 128})))

	// Partially transparent PNG to exercise alpha-channel handling.
	f.Add(encodePNG(f, solidImage(4, 4, color.RGBA{R: 255, G: 0, B: 0, A: 128})))

	// Seed with empty and garbage data
	f.Add([]byte{})
	f.Add([]byte("not an image"))
	f.Add([]byte{0xFF, 0xD8, 0xFF}) // truncated JPEG header

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic. Errors on invalid input are expected.
		_, _ = PerceptualHash(bytes.NewReader(data))
	})
}

// FuzzPerceptualHashDeterminism verifies that hashing the same valid image
// data always produces the same hash value.
func FuzzPerceptualHashDeterminism(f *testing.F) {
	f.Add(encodePNG(f, solidImage(50, 50, color.RGBA{R: 100, G: 150, B: 200, A: 255})))
	f.Add(encodeJPEG(f, solidImage(80, 60, color.RGBA{R: 30, G: 60, B: 90, A: 255})))
	// Partially transparent PNG to exercise alpha-channel handling.
	f.Add(encodePNG(f, solidImage(4, 4, color.RGBA{R: 255, G: 0, B: 0, A: 128})))

	f.Fuzz(func(t *testing.T, data []byte) {
		h1, err1 := PerceptualHash(bytes.NewReader(data))
		h2, err2 := PerceptualHash(bytes.NewReader(data))

		if (err1 == nil) != (err2 == nil) {
			t.Fatalf("inconsistent error results: err1=%v, err2=%v", err1, err2)
		}
		if err1 != nil {
			return
		}
		if h1 != h2 {
			t.Errorf("non-deterministic hash: %x vs %x", h1, h2)
		}
	})
}

// checkerboard creates a checkerboard image for fuzz seeding.
func checkerboard(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if (x/5+y/5)%2 == 0 {
				img.Set(x, y, color.White)
			} else {
				img.Set(x, y, color.Black)
			}
		}
	}
	return img
}

// encodePNG encodes an image as PNG for fuzz seed data.
func encodePNG(f interface {
	Helper()
	Fatalf(string, ...any)
}, img image.Image) []byte {
	f.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		f.Fatalf("encoding PNG seed: %v", err)
	}
	return buf.Bytes()
}

// encodeJPEG encodes an image as JPEG for fuzz seed data.
func encodeJPEG(f interface {
	Helper()
	Fatalf(string, ...any)
}, img image.Image) []byte {
	f.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		f.Fatalf("encoding JPEG seed: %v", err)
	}
	return buf.Bytes()
}
