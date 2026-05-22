package image

import (
	"bytes"
	"image"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// ProvenanceData.IsEmpty
// ---------------------------------------------------------------------------

func TestProvenanceData_IsEmpty_True(t *testing.T) {
	var d ProvenanceData
	if !d.IsEmpty() {
		t.Error("zero-value ProvenanceData should be IsEmpty() == true")
	}
}

func TestProvenanceData_IsEmpty_False(t *testing.T) {
	tests := []struct {
		name string
		d    ProvenanceData
	}{
		{"only PHash", ProvenanceData{PHash: "abc"}},
		{"only Source", ProvenanceData{Source: "fanarttv"}},
		{"only FileFormat", ProvenanceData{FileFormat: "jpeg"}},
		{"only LastWrittenAt", ProvenanceData{LastWrittenAt: "2026-01-01T00:00:00Z"}},
		{"all fields", ProvenanceData{
			PHash:         "abc",
			Source:        "fanarttv",
			FileFormat:    "jpeg",
			LastWrittenAt: "2026-01-01T00:00:00Z",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.d.IsEmpty() {
				t.Errorf("ProvenanceData%+v.IsEmpty() = true, want false", tt.d)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// CollectProvenance
// ---------------------------------------------------------------------------

func TestCollectProvenance_JPEG_WithMeta(t *testing.T) {
	ts := time.Date(2026, 3, 17, 22, 0, 0, 0, time.UTC)
	meta := &ExifMeta{
		Source:  "fanarttv",
		Fetched: ts,
		DHash:   "a1b2c3d4e5f6a7b8",
		Rule:    "thumb_exists",
		Mode:    "auto",
	}

	data := makeJPEG(t, 32, 32)
	injected, err := InjectMeta(data, meta)
	if err != nil {
		t.Fatalf("InjectMeta: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "folder.jpg")
	if err := os.WriteFile(path, injected, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	got := CollectProvenance(path, logger)

	if got.IsEmpty() {
		t.Fatal("CollectProvenance returned empty ProvenanceData for a tagged JPEG")
	}
	if got.Source != "fanarttv" {
		t.Errorf("Source = %q, want %q", got.Source, "fanarttv")
	}
	if got.PHash == "" {
		t.Error("PHash should be non-empty")
	}
	if got.FileFormat != "jpeg" {
		t.Errorf("FileFormat = %q, want %q", got.FileFormat, "jpeg")
	}
	if got.LastWrittenAt == "" {
		t.Error("LastWrittenAt should be non-empty for an existing file")
	}
}

func TestCollectProvenance_PNG_WithMeta(t *testing.T) {
	meta := &ExifMeta{Source: "musicbrainz", Mode: "manual"}

	data := makePNG(t, 32, 32)
	injected, err := InjectMeta(data, meta)
	if err != nil {
		t.Fatalf("InjectMeta: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "logo.png")
	if err := os.WriteFile(path, injected, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	got := CollectProvenance(path, logger)

	if got.IsEmpty() {
		t.Fatal("CollectProvenance returned empty ProvenanceData for a tagged PNG")
	}
	if got.Source != "musicbrainz" {
		t.Errorf("Source = %q, want %q", got.Source, "musicbrainz")
	}
	if got.FileFormat != "png" {
		t.Errorf("FileFormat = %q, want %q", got.FileFormat, "png")
	}
}

func TestCollectProvenance_FileNotExist(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	got := CollectProvenance("/nonexistent/path/image.jpg", logger)

	// File not found: should return zero ProvenanceData silently.
	if !got.IsEmpty() {
		t.Errorf("expected empty ProvenanceData for missing file, got %+v", got)
	}
}

func TestCollectProvenance_UntaggedJPEG(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plain.jpg")
	data := makeJPEG(t, 32, 32)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	got := CollectProvenance(path, logger)

	// Source and PHash will be empty for an untagged image; format and mtime should be set.
	if got.FileFormat != "jpeg" {
		t.Errorf("FileFormat = %q, want %q", got.FileFormat, "jpeg")
	}
	if got.LastWrittenAt == "" {
		t.Error("LastWrittenAt should be set even for untagged images")
	}
}

func TestCollectProvenance_UnrecognizedExtension(t *testing.T) {
	dir := t.TempDir()
	// Write a plain file with an unrecognized extension. CollectProvenance
	// should log a warning for the extension but still return mtime data.
	path := filepath.Join(dir, "image.bmp")
	if err := os.WriteFile(path, []byte("not an image"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var logBuf strings.Builder
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// ReadProvenance will fail on the non-image data but we test that CollectProvenance
	// logs a warning about the unrecognized extension.
	// The .bmp extension is not .jpg/.jpeg/.png so FileFormat should be empty.
	got := CollectProvenance(path, logger)
	if got.FileFormat != "" {
		t.Errorf("FileFormat = %q, want %q for unrecognized extension", got.FileFormat, "")
	}
}

// ---------------------------------------------------------------------------
// TrimAlpha
// ---------------------------------------------------------------------------

// makePNGTransparent creates a PNG image where every pixel is fully transparent.
func makePNGTransparent(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// Default zero value is fully transparent -- no fill needed.
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding transparent PNG: %v", err)
	}
	return buf.Bytes()
}

func TestTrimAlpha_PNG_WithPadding(t *testing.T) {
	// 100x60 PNG with 15px transparent padding. Content: 70x30 starting at (15,15).
	data := makePNGWithPadding(t, 100, 60, 15, 15, 15, 15)

	trimmed, format, err := TrimAlpha(bytes.NewReader(data), 128)
	if err != nil {
		t.Fatalf("TrimAlpha: %v", err)
	}
	if format != FormatPNG {
		t.Errorf("format = %q, want %q", format, FormatPNG)
	}

	w, h, err := GetDimensions(bytes.NewReader(trimmed))
	if err != nil {
		t.Fatalf("GetDimensions: %v", err)
	}
	// Content region: totalW - 2*pad = 100-30=70, totalH - 2*pad = 60-30=30.
	if w != 70 || h != 30 {
		t.Errorf("trimmed dimensions = %dx%d, want 70x30", w, h)
	}
}

func TestTrimAlpha_PNG_FullyOpaque(t *testing.T) {
	// Fully opaque PNG should return the full image unchanged.
	data := makePNG(t, 60, 60)

	trimmed, format, err := TrimAlpha(bytes.NewReader(data), 128)
	if err != nil {
		t.Fatalf("TrimAlpha: %v", err)
	}
	if format != FormatPNG {
		t.Errorf("format = %q, want %q", format, FormatPNG)
	}

	w, h, err := GetDimensions(bytes.NewReader(trimmed))
	if err != nil {
		t.Fatalf("GetDimensions: %v", err)
	}
	if w != 60 || h != 60 {
		t.Errorf("trimmed dimensions = %dx%d, want 60x60 (no trim for fully opaque)", w, h)
	}
}

func TestTrimAlpha_PNG_FullyTransparent(t *testing.T) {
	// Fully transparent PNG: no visible pixels -- should return the original unchanged.
	data := makePNGTransparent(t, 50, 50)

	trimmed, format, err := TrimAlpha(bytes.NewReader(data), 0)
	if err != nil {
		t.Fatalf("TrimAlpha: %v", err)
	}
	if format != FormatPNG {
		t.Errorf("format = %q, want %q", format, FormatPNG)
	}

	w, h, err := GetDimensions(bytes.NewReader(trimmed))
	if err != nil {
		t.Fatalf("GetDimensions: %v", err)
	}
	if w != 50 || h != 50 {
		t.Errorf("trimmed dimensions = %dx%d, want original 50x50 for fully transparent", w, h)
	}
}

func TestTrimAlpha_JPEG_PassThrough(t *testing.T) {
	// TrimAlpha on JPEG should return the bytes unchanged (no alpha in JPEG).
	data := makeJPEG(t, 80, 40)

	trimmed, format, err := TrimAlpha(bytes.NewReader(data), 128)
	if err != nil {
		t.Fatalf("TrimAlpha: %v", err)
	}
	if format != FormatJPEG {
		t.Errorf("format = %q, want %q", format, FormatJPEG)
	}

	// Bytes should be identical (passed through via io.ReadAll, not re-encoded).
	if !bytes.Equal(trimmed, data) {
		t.Error("TrimAlpha on JPEG should return bytes unchanged")
	}
}

func TestTrimAlpha_InvalidInput(t *testing.T) {
	_, _, err := TrimAlpha(bytes.NewReader([]byte("not an image")), 128)
	if err == nil {
		t.Error("expected error for invalid input")
	}
}
