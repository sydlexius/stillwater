package image

import (
	"bytes"
	"context"
	"encoding/base64"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// makeJPEG creates a JPEG-encoded image of the given dimensions.
func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encoding test jpeg: %v", err)
	}
	return buf.Bytes()
}

// makePNG creates a PNG-encoded image of the given dimensions.
func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 64, A: 200})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding test png: %v", err)
	}
	return buf.Bytes()
}

func TestDetectFormat_JPEG(t *testing.T) {
	data := makeJPEG(t, 10, 10)
	format, replay, err := DetectFormat(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if format != FormatJPEG {
		t.Errorf("got format %q, want %q", format, FormatJPEG)
	}
	// Verify replay is still decodable
	_, err = jpeg.Decode(replay)
	if err != nil {
		t.Errorf("replay reader should still decode: %v", err)
	}
}

func TestDetectFormat_PNG(t *testing.T) {
	data := makePNG(t, 10, 10)
	format, replay, err := DetectFormat(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if format != FormatPNG {
		t.Errorf("got format %q, want %q", format, FormatPNG)
	}
	_, err = png.Decode(replay)
	if err != nil {
		t.Errorf("replay reader should still decode: %v", err)
	}
}

func TestDetectFormat_Unknown(t *testing.T) {
	_, _, err := DetectFormat(bytes.NewReader([]byte("not an image")))
	if err == nil {
		t.Error("expected error for unknown format")
	}
}

func TestGetDimensions(t *testing.T) {
	tests := []struct {
		name  string
		data  []byte
		wantW int
		wantH int
	}{
		{"jpeg 100x50", makeJPEG(t, 100, 50), 100, 50},
		{"png 200x300", makePNG(t, 200, 300), 200, 300},
		{"jpeg 1x1", makeJPEG(t, 1, 1), 1, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, h, err := GetDimensions(bytes.NewReader(tt.data))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if w != tt.wantW || h != tt.wantH {
				t.Errorf("got %dx%d, want %dx%d", w, h, tt.wantW, tt.wantH)
			}
		})
	}
}

func TestResize_Downscale(t *testing.T) {
	data := makeJPEG(t, 1000, 800)
	result, format, err := Resize(bytes.NewReader(data), 500, 500)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if format != FormatJPEG {
		t.Errorf("got format %q, want %q", format, FormatJPEG)
	}

	w, h, err := GetDimensions(bytes.NewReader(result))
	if err != nil {
		t.Fatalf("reading result dimensions: %v", err)
	}
	if w > 500 || h > 500 {
		t.Errorf("result %dx%d exceeds max 500x500", w, h)
	}
	// Aspect ratio should be maintained (1000:800 = 5:4)
	if w != 500 || h != 400 {
		t.Errorf("expected 500x400, got %dx%d", w, h)
	}
}

func TestResize_AlreadyFits(t *testing.T) {
	data := makePNG(t, 100, 100)
	result, format, err := Resize(bytes.NewReader(data), 500, 500)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if format != FormatPNG {
		t.Errorf("got format %q, want %q", format, FormatPNG)
	}

	w, h, err := GetDimensions(bytes.NewReader(result))
	if err != nil {
		t.Fatalf("reading result dimensions: %v", err)
	}
	if w != 100 || h != 100 {
		t.Errorf("expected 100x100, got %dx%d", w, h)
	}
}

// minimalWebP is a minimal valid 1x1 lossless WebP image used for format-conversion tests.
// Generated offline; contains a single white pixel encoded with VP8L.
var minimalWebP = []byte{
	0x52, 0x49, 0x46, 0x46, 0x24, 0x00, 0x00, 0x00, // RIFF....
	0x57, 0x45, 0x42, 0x50, 0x56, 0x50, 0x38, 0x4c, // WEBPVP8L
	0x15, 0x00, 0x00, 0x00, 0x2f, 0x00, 0x00, 0x00, // ..../.
	0x10, 0x07, 0x10, 0x11, 0x11, 0x88, 0x88, 0x08,
	0x08, 0x00, 0x00, 0x00,
}

func TestConvertFormat_WebP_ConvertsToPNG(t *testing.T) {
	result, format, err := ConvertFormat(bytes.NewReader(minimalWebP))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if format != FormatPNG {
		t.Errorf("got format %q, want %q", format, FormatPNG)
	}
	w, h, err := GetDimensions(bytes.NewReader(result))
	if err != nil {
		t.Fatalf("reading result dimensions: %v", err)
	}
	if w != 1 || h != 1 {
		t.Errorf("expected 1x1, got %dx%d -- dimensions not preserved through WebP-to-PNG conversion", w, h)
	}
}

func TestConvertFormat_JPEG_PreservesNativeResolution(t *testing.T) {
	// A large JPEG (simulating a 4K backdrop) must not be downscaled.
	data := makeJPEG(t, 3840, 2160)
	result, format, err := ConvertFormat(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if format != FormatJPEG {
		t.Errorf("got format %q, want %q", format, FormatJPEG)
	}
	w, h, err := GetDimensions(bytes.NewReader(result))
	if err != nil {
		t.Fatalf("reading result dimensions: %v", err)
	}
	if w != 3840 || h != 2160 {
		t.Errorf("expected 3840x2160, got %dx%d -- image was silently downscaled", w, h)
	}
}

func TestConvertFormat_PNG_PreservesNativeResolution(t *testing.T) {
	data := makePNG(t, 3000, 3000)
	result, format, err := ConvertFormat(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if format != FormatPNG {
		t.Errorf("got format %q, want %q", format, FormatPNG)
	}
	w, h, err := GetDimensions(bytes.NewReader(result))
	if err != nil {
		t.Fatalf("reading result dimensions: %v", err)
	}
	if w != 3000 || h != 3000 {
		t.Errorf("expected 3000x3000, got %dx%d", w, h)
	}
}

func TestOptimize_JPEG(t *testing.T) {
	data := makeJPEG(t, 200, 200)
	result, err := Optimize(bytes.NewReader(data), FormatJPEG, 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Lower quality should produce smaller file (usually)
	if len(result) == 0 {
		t.Error("result should not be empty")
	}
	// Verify it is a valid JPEG
	w, h, err := GetDimensions(bytes.NewReader(result))
	if err != nil {
		t.Fatalf("result not decodable: %v", err)
	}
	if w != 200 || h != 200 {
		t.Errorf("dimensions changed: got %dx%d", w, h)
	}
}

func TestOptimize_PNG(t *testing.T) {
	data := makePNG(t, 100, 100)
	result, err := Optimize(bytes.NewReader(data), FormatPNG, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) == 0 {
		t.Error("result should not be empty")
	}
}

func TestConvertToFormat(t *testing.T) {
	// JPEG to PNG
	jpegData := makeJPEG(t, 50, 50)
	pngResult, err := ConvertToFormat(bytes.NewReader(jpegData), FormatPNG)
	if err != nil {
		t.Fatalf("converting jpeg to png: %v", err)
	}
	format, _, err := DetectFormat(bytes.NewReader(pngResult))
	if err != nil {
		t.Fatalf("detecting converted format: %v", err)
	}
	if format != FormatPNG {
		t.Errorf("got format %q, want %q", format, FormatPNG)
	}

	// PNG to JPEG
	pngData := makePNG(t, 50, 50)
	jpegResult, err := ConvertToFormat(bytes.NewReader(pngData), FormatJPEG)
	if err != nil {
		t.Fatalf("converting png to jpeg: %v", err)
	}
	format, _, err = DetectFormat(bytes.NewReader(jpegResult))
	if err != nil {
		t.Fatalf("detecting converted format: %v", err)
	}
	if format != FormatJPEG {
		t.Errorf("got format %q, want %q", format, FormatJPEG)
	}
}

func TestConvertToFormat_Unsupported(t *testing.T) {
	data := makeJPEG(t, 10, 10)
	_, err := ConvertToFormat(bytes.NewReader(data), "webp")
	if err == nil {
		t.Error("expected error for unsupported target format")
	}
}

func TestValidateAspectRatio(t *testing.T) {
	tests := []struct {
		name      string
		w, h      int
		expected  float64
		tolerance float64
		want      bool
	}{
		{"exact 1:1", 500, 500, 1.0, 0.1, true},
		{"exact 16:9", 1920, 1080, 16.0 / 9.0, 0.05, true},
		{"close 16:9", 1900, 1080, 16.0 / 9.0, 0.05, true},
		{"far from 16:9", 500, 500, 16.0 / 9.0, 0.05, false},
		{"zero height", 500, 0, 1.0, 0.1, false},
		{"zero expected", 500, 500, 0.0, 0.1, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ValidateAspectRatio(tt.w, tt.h, tt.expected, tt.tolerance)
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCrop(t *testing.T) {
	data := makePNG(t, 200, 200)
	result, format, err := Crop(bytes.NewReader(data), 50, 50, 100, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if format != FormatPNG {
		t.Errorf("got format %q, want %q", format, FormatPNG)
	}

	w, h, err := GetDimensions(bytes.NewReader(result))
	if err != nil {
		t.Fatalf("reading result dimensions: %v", err)
	}
	if w != 100 || h != 100 {
		t.Errorf("expected 100x100, got %dx%d", w, h)
	}
}

func TestCrop_OutOfBounds(t *testing.T) {
	data := makePNG(t, 100, 100)
	_, _, err := Crop(bytes.NewReader(data), 50, 50, 100, 100)
	if err == nil {
		t.Error("expected error for out-of-bounds crop")
	}
}

func TestProbeRemoteImage(t *testing.T) {
	data := makeJPEG(t, 640, 480)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Write(data) //nolint:errcheck
	}))
	defer ts.Close()

	info, err := ProbeRemoteImage(context.Background(), ts.URL+"/test.jpg")
	if err != nil {
		t.Fatalf("ProbeRemoteImage: %v", err)
	}
	if info.Width != 640 || info.Height != 480 {
		t.Errorf("dimensions = %dx%d, want 640x480", info.Width, info.Height)
	}
	if info.FileSize != int64(len(data)) {
		t.Errorf("FileSize = %d, want %d", info.FileSize, len(data))
	}
}

func TestProbeRemoteImage_NotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer ts.Close()

	_, err := ProbeRemoteImage(context.Background(), ts.URL+"/missing.jpg")
	if err == nil {
		t.Error("expected error for 404 response")
	}
}

func TestFitDimensions(t *testing.T) {
	tests := []struct {
		name         string
		origW, origH int
		maxW, maxH   int
		wantW, wantH int
	}{
		{"no scale needed", 100, 100, 500, 500, 100, 100},
		{"scale width", 1000, 500, 500, 500, 500, 250},
		{"scale height", 500, 1000, 500, 500, 250, 500},
		{"scale both", 2000, 1000, 500, 500, 500, 250},
		{"landscape to square", 1600, 900, 800, 800, 800, 450},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, h := fitDimensions(tt.origW, tt.origH, tt.maxW, tt.maxH)
			if w != tt.wantW || h != tt.wantH {
				t.Errorf("got %dx%d, want %dx%d", w, h, tt.wantW, tt.wantH)
			}
		})
	}
}

// makePNGWithPadding creates a PNG where the content occupies the center region
// and the surrounding border is fully transparent.
func makePNGWithPadding(t *testing.T, totalW, totalH, padLeft, padRight, padTop, padBottom int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, totalW, totalH))
	// Fill content area with opaque pixels
	for y := padTop; y < totalH-padBottom; y++ {
		for x := padLeft; x < totalW-padRight; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	// Padding stays at zero value (fully transparent)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding test png: %v", err)
	}
	return buf.Bytes()
}

func TestTrimAlphaBounds_PNG_WithPadding(t *testing.T) {
	// 200x100 image with 20px padding on left/right (10%) and 10px on top/bottom (10%)
	data := makePNGWithPadding(t, 200, 100, 20, 20, 10, 10)

	content, original, err := TrimAlphaBounds(bytes.NewReader(data), 128)
	if err != nil {
		t.Fatalf("TrimAlphaBounds: %v", err)
	}

	if original.Dx() != 200 || original.Dy() != 100 {
		t.Errorf("original = %v, want 200x100", original)
	}
	if content.Min.X != 20 || content.Min.Y != 10 {
		t.Errorf("content.Min = (%d,%d), want (20,10)", content.Min.X, content.Min.Y)
	}
	if content.Max.X != 180 || content.Max.Y != 90 {
		t.Errorf("content.Max = (%d,%d), want (180,90)", content.Max.X, content.Max.Y)
	}
}

func TestTrimAlphaBounds_PNG_NoPadding(t *testing.T) {
	// Fully opaque PNG -- content should equal original
	data := makePNG(t, 100, 100)

	content, original, err := TrimAlphaBounds(bytes.NewReader(data), 128)
	if err != nil {
		t.Fatalf("TrimAlphaBounds: %v", err)
	}
	if content != original {
		t.Errorf("expected content == original for fully opaque PNG; content=%v, original=%v", content, original)
	}
}

func TestTrimAlphaBounds_JPEG(t *testing.T) {
	// JPEG has no alpha channel -- content should equal original
	data := makeJPEG(t, 100, 50)

	content, original, err := TrimAlphaBounds(bytes.NewReader(data), 128)
	if err != nil {
		t.Fatalf("TrimAlphaBounds: %v", err)
	}
	if content != original {
		t.Errorf("expected content == original for JPEG; content=%v, original=%v", content, original)
	}
	if original.Dx() != 100 || original.Dy() != 50 {
		t.Errorf("original = %v, want 100x50", original)
	}
}

func TestContentBounds_PNG_WithPadding(t *testing.T) {
	data := makePNGWithPadding(t, 200, 100, 20, 20, 10, 10)

	content, original, err := ContentBounds(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ContentBounds: %v", err)
	}

	if original.Dx() != 200 || original.Dy() != 100 {
		t.Errorf("original = %v, want 200x100", original)
	}
	if content.Min.X != 20 || content.Min.Y != 10 {
		t.Errorf("content.Min = (%d,%d), want (20,10)", content.Min.X, content.Min.Y)
	}
	if content.Max.X != 180 || content.Max.Y != 90 {
		t.Errorf("content.Max = (%d,%d), want (180,90)", content.Max.X, content.Max.Y)
	}
}

func TestContentBounds_PNG_NoPadding(t *testing.T) {
	data := makePNG(t, 100, 100)

	content, original, err := ContentBounds(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ContentBounds: %v", err)
	}
	if content != original {
		t.Errorf("expected content == original for fully opaque PNG; content=%v, original=%v", content, original)
	}
}

func makeJPEGWithWhitespace(t *testing.T, totalW, totalH, padLeft, padRight, padTop, padBottom int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, totalW, totalH))
	// Fill with white (near-white to match the > 240 threshold).
	for y := 0; y < totalH; y++ {
		for x := 0; x < totalW; x++ {
			img.Set(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
		}
	}
	// Draw colored content in the center.
	for y := padTop; y < totalH-padBottom; y++ {
		for x := padLeft; x < totalW-padRight; x++ {
			img.Set(x, y, color.RGBA{R: 100, G: 50, B: 150, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatalf("encoding JPEG: %v", err)
	}
	return buf.Bytes()
}

func TestContentBounds_JPEG_WithWhitespace(t *testing.T) {
	data := makeJPEGWithWhitespace(t, 200, 100, 20, 20, 10, 10)

	content, original, err := ContentBounds(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ContentBounds: %v", err)
	}

	if original.Dx() != 200 || original.Dy() != 100 {
		t.Errorf("original = %v, want 200x100", original)
	}
	// JPEG compression may shift boundaries by a pixel or two.
	// Check that the content is significantly smaller than the original.
	contentArea := content.Dx() * content.Dy()
	totalArea := original.Dx() * original.Dy()
	paddingRatio := 1.0 - float64(contentArea)/float64(totalArea)
	if paddingRatio < 0.10 {
		t.Errorf("expected significant padding detected in JPEG; paddingRatio = %.2f", paddingRatio)
	}
}

func TestTrimWithMargin_PNG(t *testing.T) {
	// 200x80 PNG with 30px padding on all sides. Content = 140x20.
	data := makePNGWithPadding(t, 200, 80, 30, 30, 30, 30)

	trimmed, format, err := TrimWithMargin(bytes.NewReader(data), 5)
	if err != nil {
		t.Fatalf("TrimWithMargin: %v", err)
	}
	if format != FormatPNG {
		t.Errorf("format = %q, want %q", format, FormatPNG)
	}

	w, h, err := GetDimensions(bytes.NewReader(trimmed))
	if err != nil {
		t.Fatalf("GetDimensions: %v", err)
	}
	// Content is 140x20, margin is 5px each side, so result should be ~150x30.
	if w < 140 || w > 160 || h < 20 || h > 40 {
		t.Errorf("trimmed dimensions = %dx%d, expected roughly 150x30", w, h)
	}
}

func TestTrimWithMargin_JPEG(t *testing.T) {
	// JPEG with whitespace borders -- verify TrimWithMargin detects and trims them.
	data := makeJPEGWithWhitespace(t, 200, 100, 30, 30, 30, 30)

	trimmed, format, err := TrimWithMargin(bytes.NewReader(data), 2)
	if err != nil {
		t.Fatalf("TrimWithMargin: %v", err)
	}
	if format != FormatJPEG {
		t.Errorf("format = %q, want %q", format, FormatJPEG)
	}

	w, h, err := GetDimensions(bytes.NewReader(trimmed))
	if err != nil {
		t.Fatalf("GetDimensions: %v", err)
	}
	origW, origH, _ := GetDimensions(bytes.NewReader(data))
	if w >= origW || h >= origH {
		t.Errorf("trimmed %dx%d should be smaller than original %dx%d", w, h, origW, origH)
	}
}

func TestTrimWithMargin_LargeMargin(t *testing.T) {
	// Margin larger than the padding should return the full image.
	data := makePNGWithPadding(t, 100, 100, 10, 10, 10, 10)

	trimmed, _, err := TrimWithMargin(bytes.NewReader(data), 500)
	if err != nil {
		t.Fatalf("TrimWithMargin: %v", err)
	}

	w, h, err := GetDimensions(bytes.NewReader(trimmed))
	if err != nil {
		t.Fatalf("GetDimensions: %v", err)
	}
	if w != 100 || h != 100 {
		t.Errorf("expected original 100x100 when margin exceeds padding, got %dx%d", w, h)
	}
}

func TestTrimWithMargin_NoContent(t *testing.T) {
	// Fully transparent PNG -- should return unchanged.
	img := image.NewRGBA(image.Rect(0, 0, 50, 50))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding PNG: %v", err)
	}

	trimmed, _, err := TrimWithMargin(bytes.NewReader(buf.Bytes()), 2)
	if err != nil {
		t.Fatalf("TrimWithMargin: %v", err)
	}

	w, h, err := GetDimensions(bytes.NewReader(trimmed))
	if err != nil {
		t.Fatalf("GetDimensions: %v", err)
	}
	if w != 50 || h != 50 {
		t.Errorf("expected unchanged 50x50, got %dx%d", w, h)
	}
}

func TestGeneratePlaceholder_JPEG(t *testing.T) {
	data := makeJPEG(t, 500, 500)
	result, err := GeneratePlaceholder(bytes.NewReader(data), "thumb")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	prefix := "data:image/jpeg;base64,"
	if !strings.HasPrefix(result, prefix) {
		t.Fatalf("result should start with %q, got prefix %q", prefix, result[:min(len(result), len(prefix)+5)])
	}
	// Decode and verify dimensions
	b64 := result[len(prefix):]
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	w, h, err := GetDimensions(bytes.NewReader(decoded))
	if err != nil {
		t.Fatalf("decoding placeholder dimensions: %v", err)
	}
	if w != 16 || h != 16 {
		t.Errorf("placeholder dimensions = %dx%d, want 16x16", w, h)
	}
}

func TestGeneratePlaceholder_PNG(t *testing.T) {
	data := makePNG(t, 500, 500)
	result, err := GeneratePlaceholder(bytes.NewReader(data), "fanart")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Non-logo PNG should become JPEG placeholder
	if !strings.HasPrefix(result, "data:image/jpeg;base64,") {
		t.Errorf("non-logo PNG placeholder should use JPEG encoding, got prefix %q", result[:min(len(result), 30)])
	}
}

func TestGeneratePlaceholder_Logo(t *testing.T) {
	data := makePNG(t, 500, 500)
	result, err := GeneratePlaceholder(bytes.NewReader(data), "logo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Logo should stay PNG
	if !strings.HasPrefix(result, "data:image/png;base64,") {
		t.Errorf("logo placeholder should use PNG encoding, got prefix %q", result[:min(len(result), 30)])
	}
}

func TestGeneratePlaceholder_InvalidInput(t *testing.T) {
	result, err := GeneratePlaceholder(bytes.NewReader([]byte("not an image")), "thumb")
	if err == nil {
		t.Error("expected error for invalid input")
	}
	if result != "" {
		t.Errorf("expected empty result, got %q", result)
	}
}

func TestGeneratePlaceholder_TooLargeBytes(t *testing.T) {
	// Create data that exceeds maxPlaceholderBytes (25 MB).
	// We don't need a valid image -- the size check happens before decode.
	// Prepend a valid JPEG header so DetectFormat succeeds.
	header := makeJPEG(t, 1, 1)
	padding := make([]byte, 26<<20) // 26 MB
	data := append(header, padding...)

	result, err := GeneratePlaceholder(bytes.NewReader(data), "thumb")
	if err == nil {
		t.Error("expected error for oversized input")
	}
	if result != "" {
		t.Errorf("expected empty result, got %q", result[:min(len(result), 30)])
	}
	if err != nil && !strings.Contains(err.Error(), "too large") {
		t.Errorf("error should mention 'too large', got: %v", err)
	}
}

func TestGeneratePlaceholder_ZeroDimensions(t *testing.T) {
	// Construct a minimal valid PNG with 0x0 dimensions in the IHDR chunk.
	// PNG signature (8 bytes) + IHDR chunk (25 bytes) + IEND chunk (12 bytes).
	// IHDR: length(4) + "IHDR"(4) + width(4) + height(4) + bitdepth(1) +
	//       colortype(1) + compression(1) + filter(1) + interlace(1) + CRC(4)
	var buf bytes.Buffer

	// PNG signature
	buf.Write([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})

	// IHDR chunk: length = 13
	buf.Write([]byte{0x00, 0x00, 0x00, 0x0D}) // length
	ihdr := []byte{
		0x49, 0x48, 0x44, 0x52, // "IHDR"
		0x00, 0x00, 0x00, 0x00, // width = 0
		0x00, 0x00, 0x00, 0x00, // height = 0
		0x08,             // bit depth
		0x02,             // color type (RGB)
		0x00, 0x00, 0x00, // compression, filter, interlace
	}
	buf.Write(ihdr)
	// CRC32 over "IHDR" + data
	crc := crc32.ChecksumIEEE(ihdr)
	buf.Write([]byte{byte(crc >> 24), byte(crc >> 16), byte(crc >> 8), byte(crc)})

	// IEND chunk
	buf.Write([]byte{0x00, 0x00, 0x00, 0x00})
	iend := []byte{0x49, 0x45, 0x4E, 0x44}
	buf.Write(iend)
	crc = crc32.ChecksumIEEE(iend)
	buf.Write([]byte{byte(crc >> 24), byte(crc >> 16), byte(crc >> 8), byte(crc)})

	result, err := GeneratePlaceholder(bytes.NewReader(buf.Bytes()), "thumb")
	if err == nil {
		t.Error("expected error for zero-dimension image")
	}
	if result != "" {
		t.Errorf("expected empty result, got %q", result)
	}
}

func TestIsLowResolution(t *testing.T) {
	tests := []struct {
		name      string
		w, h      int
		imageType string
		want      bool
	}{
		// Unknown dimensions are never low-res.
		{"zero width", 0, 500, "thumb", false},
		{"zero height", 500, 0, "thumb", false},
		{"both zero", 0, 0, "thumb", false},

		// Thumb / default type.
		{"thumb at minimum", 500, 500, "thumb", false},
		{"thumb below width", 499, 500, "thumb", true},
		{"thumb below height", 500, 499, "thumb", true},
		{"thumb good", 1000, 1000, "thumb", false},
		{"poster same as thumb", 500, 500, "poster", false},
		{"poster low", 400, 400, "poster", true},

		// Fanart / backdrop.
		{"fanart at minimum", 960, 540, "fanart", false},
		{"fanart below width", 959, 540, "fanart", true},
		{"fanart below height", 960, 539, "fanart", true},
		{"fanart HD", 1920, 1080, "fanart", false},
		{"background alias", 960, 540, "background", false},
		{"background low", 800, 400, "background", true},

		// Banner.
		{"banner at minimum", 758, 140, "banner", false},
		{"banner standard", 1000, 185, "banner", false},
		{"banner below width", 757, 185, "banner", true},
		{"banner below height", 1000, 139, "banner", true},

		// Logo / hdlogo.
		{"logo at minimum", 400, 155, "logo", false},
		{"logo standard", 800, 310, "logo", false},
		{"logo below width", 399, 310, "logo", true},
		{"logo below height", 800, 154, "logo", true},
		{"hdlogo good", 800, 310, "hdlogo", false},
		{"hdlogo low", 200, 80, "hdlogo", true},

		// Provider alias normalization.
		{"background alias maps to fanart", 1920, 1080, "background", false},
		{"background alias low", 800, 400, "background", true},
		{"widethumb alias maps to thumb", 500, 500, "widethumb", false},
		{"widethumb alias low", 400, 400, "widethumb", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsLowResolution(tt.w, tt.h, tt.imageType)
			if got != tt.want {
				t.Errorf("IsLowResolution(%d, %d, %q) = %v, want %v",
					tt.w, tt.h, tt.imageType, got, tt.want)
			}
		})
	}
}
