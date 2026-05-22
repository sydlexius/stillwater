package image

// extra_coverage_test.go adds targeted tests for branches that remained
// under-covered after the main test suites were written. Each test exercises
// a specific code path identified from the coverprofile, with concrete
// assertions on the behavior.

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ---------------------------------------------------------------------------
// encode -- unsupported format (default branch)
// ---------------------------------------------------------------------------

func TestEncode_UnsupportedFormat(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	_, err := encode(img, "webp", 0)
	if err == nil {
		t.Fatal("encode with unsupported format should return an error")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("unsupported output format")) {
		t.Errorf("error should mention 'unsupported output format', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// formatToExt -- default case (not jpeg, not png)
// ---------------------------------------------------------------------------

func TestFormatToExt_Default(t *testing.T) {
	got := formatToExt("webp")
	if got != ".jpg" {
		t.Errorf("formatToExt('webp') = %q, want %q (default fallback)", got, ".jpg")
	}
}

func TestFormatToExt_JPEG(t *testing.T) {
	got := formatToExt(FormatJPEG)
	if got != ".jpg" {
		t.Errorf("formatToExt('jpeg') = %q, want .jpg", got)
	}
}

func TestFormatToExt_PNG(t *testing.T) {
	got := formatToExt(FormatPNG)
	if got != ".png" {
		t.Errorf("formatToExt('png') = %q, want .png", got)
	}
}

// ---------------------------------------------------------------------------
// GetDimensions -- error path
// ---------------------------------------------------------------------------

func TestGetDimensions_InvalidData(t *testing.T) {
	_, _, err := GetDimensions(bytes.NewReader([]byte("not image data")))
	if err == nil {
		t.Error("GetDimensions should return an error for invalid data")
	}
}

// ---------------------------------------------------------------------------
// Optimize -- error path (invalid input)
// ---------------------------------------------------------------------------

func TestOptimize_InvalidInput(t *testing.T) {
	_, err := Optimize(bytes.NewReader([]byte("not image data")), FormatJPEG, 80)
	if err == nil {
		t.Error("Optimize should return an error for invalid image data")
	}
}

// ---------------------------------------------------------------------------
// Resize -- error paths
// ---------------------------------------------------------------------------

func TestResize_InvalidInput(t *testing.T) {
	_, _, err := Resize(bytes.NewReader([]byte("not image data")), 200, 200)
	if err == nil {
		t.Error("Resize should return an error for invalid image data")
	}
}

// ---------------------------------------------------------------------------
// ConvertFormat -- error path (unrecognized format)
// ---------------------------------------------------------------------------

func TestConvertFormat_InvalidInput(t *testing.T) {
	_, _, err := ConvertFormat(bytes.NewReader([]byte("not image data")))
	if err == nil {
		t.Error("ConvertFormat should return an error for unrecognized data")
	}
}

// ---------------------------------------------------------------------------
// ContentBounds -- error path
// ---------------------------------------------------------------------------

func TestContentBounds_InvalidInput(t *testing.T) {
	_, _, err := ContentBounds(bytes.NewReader([]byte("not image data")))
	if err == nil {
		t.Error("ContentBounds should return an error for invalid data")
	}
}

// Note on Crop's subImager fallback (processor.go:555): the draw.Copy
// fallback is unreachable through the public Crop API because image.Decode
// only ever returns standard library image types, all of which implement
// SubImage. It is defensive code, deliberately left uncovered rather than
// exercised by a white-box test that would not reflect a real call path.

// ---------------------------------------------------------------------------
// stripAPP1 -- non-marker byte path (data past SOS marker contains non-0xFF)
// ---------------------------------------------------------------------------

func TestStripAPP1_WithAPP1AndSOS(t *testing.T) {
	// Inject a description then strip it. The injected JPEG has an APP1
	// followed by the original JPEG content. After stripping, no APP1
	// should remain and the image should be decodable.
	original := makeJPEG(t, 16, 16)
	injected, err := injectJPEGDescription(original, "test description")
	if err != nil {
		t.Fatalf("injectJPEGDescription: %v", err)
	}

	stripped := stripAPP1(injected)

	// Verify the description is gone.
	desc, err := readJPEGDescription(stripped)
	if err != nil {
		t.Fatalf("readJPEGDescription after strip: %v", err)
	}
	if desc != "" {
		t.Errorf("description should be empty after stripAPP1, got %q", desc)
	}

	// Verify the image is still decodable.
	_, _, err = GetDimensions(bytes.NewReader(stripped))
	if err != nil {
		t.Fatalf("GetDimensions after stripAPP1: %v", err)
	}
}

func TestStripAPP1_InvalidSOI(t *testing.T) {
	// Non-JPEG data: should be returned unchanged.
	data := []byte{0x00, 0x01, 0x02, 0x03}
	result := stripAPP1(data)
	if !bytes.Equal(result, data) {
		t.Error("stripAPP1 with invalid SOI should return data unchanged")
	}
}

// ---------------------------------------------------------------------------
// findTIFFDescription -- error paths
// ---------------------------------------------------------------------------

func TestFindTIFFDescription_TooShort(t *testing.T) {
	_, err := findTIFFDescription([]byte{0x49, 0x49, 0x2A, 0x00}) // only 4 bytes
	if err == nil {
		t.Error("expected error for too-short TIFF data")
	}
}

func TestFindTIFFDescription_UnknownByteOrder(t *testing.T) {
	// First two bytes are not II or MM.
	data := make([]byte, 16)
	data[0] = 0x58 // 'X'
	data[1] = 0x58 // 'X'
	_, err := findTIFFDescription(data)
	if err == nil {
		t.Error("expected error for unknown TIFF byte order")
	}
}

func TestFindTIFFDescription_BigEndian(t *testing.T) {
	// Build a minimal big-endian TIFF with no ImageDescription entry.
	// Header: MM + magic(42) + IFD offset(8)
	// IFD at offset 8: entry count 0 + next IFD pointer 0
	data := []byte{
		0x4D, 0x4D, // MM (big-endian)
		0x00, 0x2A, // magic 42
		0x00, 0x00, 0x00, 0x08, // IFD offset = 8
		0x00, 0x00, // entry count = 0
		0x00, 0x00, 0x00, 0x00, // next IFD offset = 0
	}

	desc, err := findTIFFDescription(data)
	if err != nil {
		t.Fatalf("findTIFFDescription big-endian: %v", err)
	}
	if desc != "" {
		t.Errorf("expected empty description, got %q", desc)
	}
}

// ---------------------------------------------------------------------------
// readJPEGDescription -- truncated data and bad marker paths
// ---------------------------------------------------------------------------

func TestReadJPEGDescription_TruncatedData(t *testing.T) {
	// A JPEG that starts with SOI but is immediately truncated should not panic.
	data := []byte{0xFF, 0xD8} // Just the SOI, nothing else.
	got, err := readJPEGDescription(data)
	if err != nil {
		t.Fatalf("unexpected error for truncated JPEG: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty for truncated JPEG, got %q", got)
	}
}

func TestReadJPEGDescription_NotJPEG(t *testing.T) {
	data := []byte{0x89, 0x50, 0x4E, 0x47} // PNG signature
	_, err := readJPEGDescription(data)
	if err == nil {
		t.Error("expected error for non-JPEG data passed to readJPEGDescription")
	}
}

// ---------------------------------------------------------------------------
// ProbeRemoteImageWithClient -- no Content-Length header
// ---------------------------------------------------------------------------

func TestProbeRemoteImage_NoContentLength(t *testing.T) {
	data := makeJPEG(t, 100, 80)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Intentionally omit Content-Length header.
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(data)
	}))
	defer ts.Close()

	info, err := ProbeRemoteImageWithClient(context.Background(), ts.URL+"/test.jpg", ts.Client())
	if err != nil {
		t.Fatalf("ProbeRemoteImage without Content-Length: %v", err)
	}
	if info.Width != 100 || info.Height != 80 {
		t.Errorf("dimensions = %dx%d, want 100x80", info.Width, info.Height)
	}
	// FileSize should be derived from actual body length when no Content-Length.
	if info.FileSize != int64(len(data)) {
		t.Errorf("FileSize = %d, want %d", info.FileSize, len(data))
	}
}

// ---------------------------------------------------------------------------
// readPNGDescription -- tEXt chunk with a non-ImageDescription keyword
// ---------------------------------------------------------------------------

// injectPNGTextChunk inserts a tEXt chunk with an arbitrary keyword directly
// after the PNG's IHDR chunk. IHDR is always the first chunk and always 25
// bytes (4-byte length + 4-byte type + 13-byte data + 4-byte CRC), so the
// insertion point is fixed. It lets a test build a PNG whose only tEXt chunk
// does not use the ImageDescription keyword.
func injectPNGTextChunk(t *testing.T, data []byte, keyword, text string) []byte {
	t.Helper()
	const ihdrEnd = 8 + 25 // PNG signature + IHDR chunk
	if len(data) < ihdrEnd {
		t.Fatalf("png too short to contain IHDR: %d bytes", len(data))
	}

	var payload bytes.Buffer
	payload.WriteString(keyword)
	payload.WriteByte(0x00)
	payload.WriteString(text)
	p := payload.Bytes()

	chunkType := []byte("tEXt")
	var chunk bytes.Buffer
	if err := binary.Write(&chunk, binary.BigEndian, uint32(len(p))); err != nil {
		t.Fatalf("write chunk length: %v", err)
	}
	chunk.Write(chunkType)
	chunk.Write(p)
	crc := crc32.ChecksumIEEE(append(append([]byte{}, chunkType...), p...))
	if err := binary.Write(&chunk, binary.BigEndian, crc); err != nil {
		t.Fatalf("write chunk crc: %v", err)
	}

	out := make([]byte, 0, len(data)+chunk.Len())
	out = append(out, data[:ihdrEnd]...)
	out = append(out, chunk.Bytes()...)
	out = append(out, data[ihdrEnd:]...)
	return out
}

func TestReadPNGDescription_SkipsNonImageDescriptionChunk(t *testing.T) {
	// A PNG carrying a tEXt chunk whose keyword is not "ImageDescription":
	// readPNGDescription must skip it and report no description.
	data := makePNG(t, 8, 8)
	withChunk := injectPNGTextChunk(t, data, "Author", "Jane Doe")

	got, err := readPNGDescription(withChunk)
	if err != nil {
		t.Fatalf("readPNGDescription: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty (a non-ImageDescription tEXt chunk must be skipped)", got)
	}
}

// Note: injectPNGDescription's "payload too large" guard (len(payload) >
// math.MaxUint32) is not covered by a test. Triggering it requires a
// description string larger than 4 GB, which is impractical in CI. The branch
// is a safety guard, not a reachable production path.

// ---------------------------------------------------------------------------
// GeneratePlaceholder -- too many pixels path
// ---------------------------------------------------------------------------

// minimalPNGHeader builds a PNG made of only the 8-byte signature and a valid
// IHDR chunk declaring the given dimensions. image.DecodeConfig reads the
// dimensions straight from IHDR, so GeneratePlaceholder's pixel-count guard
// can be exercised without ever allocating a real image of that size.
func minimalPNGHeader(t *testing.T, width, height uint32) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}) // PNG signature

	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], width)
	binary.BigEndian.PutUint32(ihdr[4:8], height)
	ihdr[8] = 8 // bit depth
	ihdr[9] = 0 // color type: grayscale
	// ihdr[10..12] (compression, filter, interlace) stay zero.

	chunk := append([]byte("IHDR"), ihdr...)
	if err := binary.Write(&buf, binary.BigEndian, uint32(len(ihdr))); err != nil {
		t.Fatalf("write IHDR length: %v", err)
	}
	buf.Write(chunk)
	if err := binary.Write(&buf, binary.BigEndian, crc32.ChecksumIEEE(chunk)); err != nil {
		t.Fatalf("write IHDR crc: %v", err)
	}
	return buf.Bytes()
}

func TestGeneratePlaceholder_TooManyPixels(t *testing.T) {
	// 20000x20000 = 400 megapixels, well over the 100-megapixel cap.
	// GeneratePlaceholder reads the dimensions from IHDR via DecodeConfig
	// and rejects the image before decoding any pixel data, so this test
	// never materializes a large buffer.
	data := minimalPNGHeader(t, 20000, 20000)

	result, err := GeneratePlaceholder(bytes.NewReader(data), "thumb")
	if err == nil {
		t.Fatal("expected error for image exceeding the pixel cap")
	}
	if result != "" {
		t.Errorf("expected empty result, got %q", result)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("too many pixels")) {
		t.Errorf("error should mention 'too many pixels', got: %v", err)
	}
}
