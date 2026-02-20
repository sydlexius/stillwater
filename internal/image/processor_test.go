package image

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
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
		name   string
		data   []byte
		wantW  int
		wantH  int
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

func TestFitDimensions(t *testing.T) {
	tests := []struct {
		name               string
		origW, origH       int
		maxW, maxH         int
		wantW, wantH       int
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
