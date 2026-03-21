package image

import (
	"bytes"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Task 1: ExifMeta Marshal / ParseExifMeta
// ---------------------------------------------------------------------------

func TestExifMeta_MarshalFullFields(t *testing.T) {
	ts := time.Date(2026, 3, 17, 22, 0, 0, 0, time.UTC)
	m := ExifMeta{
		Source:  "fanarttv",
		Fetched: ts,
		URL:     "https://assets.fanart.tv/image/123.jpg",
		DHash:   "a1b2c3d4e5f6a7b8",
		Rule:    "thumb_exists",
		Mode:    "auto",
	}
	got := m.Marshal()
	want := "stillwater:v1|source=fanarttv|fetched=2026-03-17T22:00:00Z|url=https://assets.fanart.tv/image/123.jpg|dhash=a1b2c3d4e5f6a7b8|rule=thumb_exists|mode=auto"
	if got != want {
		t.Errorf("Marshal():\n  got  %s\n  want %s", got, want)
	}
}

func TestExifMeta_MarshalPartialFields(t *testing.T) {
	m := ExifMeta{
		Source: "musicbrainz",
		Mode:   "manual",
	}
	got := m.Marshal()
	want := "stillwater:v1|source=musicbrainz|mode=manual"
	if got != want {
		t.Errorf("Marshal():\n  got  %s\n  want %s", got, want)
	}
}

func TestExifMeta_MarshalEmpty(t *testing.T) {
	m := ExifMeta{}
	got := m.Marshal()
	want := "stillwater:v1"
	if got != want {
		t.Errorf("Marshal():\n  got  %s\n  want %s", got, want)
	}
}

func TestParseExifMeta_FullFields(t *testing.T) {
	input := "stillwater:v1|source=fanarttv|fetched=2026-03-17T22:00:00Z|url=https://assets.fanart.tv/image/123.jpg|dhash=a1b2c3d4e5f6a7b8|rule=thumb_exists|mode=auto"
	m, err := ParseExifMeta(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Source != "fanarttv" {
		t.Errorf("Source = %q, want %q", m.Source, "fanarttv")
	}
	wantTime := time.Date(2026, 3, 17, 22, 0, 0, 0, time.UTC)
	if !m.Fetched.Equal(wantTime) {
		t.Errorf("Fetched = %v, want %v", m.Fetched, wantTime)
	}
	if m.URL != "https://assets.fanart.tv/image/123.jpg" {
		t.Errorf("URL = %q, want full URL", m.URL)
	}
	if m.DHash != "a1b2c3d4e5f6a7b8" {
		t.Errorf("DHash = %q, want %q", m.DHash, "a1b2c3d4e5f6a7b8")
	}
	if m.Rule != "thumb_exists" {
		t.Errorf("Rule = %q, want %q", m.Rule, "thumb_exists")
	}
	if m.Mode != "auto" {
		t.Errorf("Mode = %q, want %q", m.Mode, "auto")
	}
}

func TestParseExifMeta_EmptyString(t *testing.T) {
	m, err := ParseExifMeta("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !m.isZero() {
		t.Errorf("expected zero struct for empty string, got %+v", m)
	}
}

func TestParseExifMeta_NonStillwaterString(t *testing.T) {
	m, err := ParseExifMeta("Photo taken by John Doe in 2024")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !m.isZero() {
		t.Errorf("expected zero struct for non-Stillwater string, got %+v", m)
	}
}

func TestParseExifMeta_PrefixOnly(t *testing.T) {
	m, err := ParseExifMeta("stillwater:v1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !m.isZero() {
		t.Errorf("expected zero struct for prefix-only string, got %+v", m)
	}
}

func TestParseExifMeta_URLWithEqualsSign(t *testing.T) {
	input := "stillwater:v1|source=fanarttv|url=https://example.com/img?w=800&h=600"
	m, err := ParseExifMeta(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.URL != "https://example.com/img?w=800&h=600" {
		t.Errorf("URL = %q, want URL with query params preserved", m.URL)
	}
}

func TestExifMeta_URLWithPipe(t *testing.T) {
	meta := ExifMeta{Source: "test", URL: "https://en.wikipedia.org/wiki/AC|DC"}
	serialized := meta.Marshal()
	if strings.Contains(serialized, "AC|DC") {
		t.Error("pipe in URL not escaped")
	}
	parsed, err := ParseExifMeta(serialized)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.URL != meta.URL {
		t.Errorf("URL = %q, want %q", parsed.URL, meta.URL)
	}
}

func TestExifMeta_URLWithLiteralPercent7C(t *testing.T) {
	// A URL containing a literal "%7C" must survive the round-trip without
	// being decoded into a pipe character.
	meta := ExifMeta{Source: "test", URL: "https://example.com/img%7Calt.jpg"}
	serialized := meta.Marshal()
	parsed, err := ParseExifMeta(serialized)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.URL != meta.URL {
		t.Errorf("URL = %q, want %q", parsed.URL, meta.URL)
	}
}

func TestExifMeta_URLWithLiteralPercent25(t *testing.T) {
	// A URL containing "%25" (an already-encoded percent sign) must survive
	// the round-trip without being decoded into a bare "%".
	meta := ExifMeta{Source: "test", URL: "https://example.com/img%25special.jpg"}
	serialized := meta.Marshal()
	parsed, err := ParseExifMeta(serialized)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.URL != meta.URL {
		t.Errorf("URL = %q, want %q", parsed.URL, meta.URL)
	}
}

func TestExifMeta_URLWithPipeAndPercent(t *testing.T) {
	// A URL with both pipe and percent characters exercises both escape
	// passes simultaneously.
	meta := ExifMeta{Source: "test", URL: "https://example.com/a|b%20c"}
	serialized := meta.Marshal()
	parsed, err := ParseExifMeta(serialized)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.URL != meta.URL {
		t.Errorf("URL = %q, want %q", parsed.URL, meta.URL)
	}
}

func TestParseExifMeta_MalformedField(t *testing.T) {
	// A field without "=" after a recognized prefix should return an error.
	input := "stillwater:v1|badfield"
	_, err := ParseExifMeta(input)
	if err == nil {
		t.Fatal("expected error for malformed field, got nil")
	}
}

func TestExifMeta_MarshalParseRoundTrip(t *testing.T) {
	ts := time.Date(2026, 3, 17, 22, 0, 0, 0, time.UTC)
	original := ExifMeta{
		Source:  "fanarttv",
		Fetched: ts,
		URL:     "https://assets.fanart.tv/image/123.jpg?size=large&q=90",
		DHash:   "a1b2c3d4e5f6a7b8",
		Rule:    "thumb_exists",
		Mode:    "auto",
	}

	serialized := original.Marshal()
	parsed, err := ParseExifMeta(serialized)
	if err != nil {
		t.Fatalf("ParseExifMeta after Marshal: %v", err)
	}

	if parsed.Source != original.Source {
		t.Errorf("Source: got %q, want %q", parsed.Source, original.Source)
	}
	if !parsed.Fetched.Equal(original.Fetched) {
		t.Errorf("Fetched: got %v, want %v", parsed.Fetched, original.Fetched)
	}
	if parsed.URL != original.URL {
		t.Errorf("URL: got %q, want %q", parsed.URL, original.URL)
	}
	if parsed.DHash != original.DHash {
		t.Errorf("DHash: got %q, want %q", parsed.DHash, original.DHash)
	}
	if parsed.Rule != original.Rule {
		t.Errorf("Rule: got %q, want %q", parsed.Rule, original.Rule)
	}
	if parsed.Mode != original.Mode {
		t.Errorf("Mode: got %q, want %q", parsed.Mode, original.Mode)
	}
}

// ---------------------------------------------------------------------------
// Task 2: JPEG EXIF injection and reading
// ---------------------------------------------------------------------------

func TestInjectJPEGDescription_RoundTrip(t *testing.T) {
	data := makeJPEG(t, 32, 32)
	desc := "stillwater:v1|source=fanarttv|mode=auto"

	injected, err := injectJPEGDescription(data, desc)
	if err != nil {
		t.Fatalf("injectJPEGDescription: %v", err)
	}

	got, err := readJPEGDescription(injected)
	if err != nil {
		t.Fatalf("readJPEGDescription: %v", err)
	}
	if got != desc {
		t.Errorf("round-trip mismatch:\n  got  %q\n  want %q", got, desc)
	}
}

func TestInjectJPEGDescription_StillDecodable(t *testing.T) {
	data := makeJPEG(t, 32, 32)
	desc := "stillwater:v1|source=test"

	injected, err := injectJPEGDescription(data, desc)
	if err != nil {
		t.Fatalf("injectJPEGDescription: %v", err)
	}

	// The injected data must still be a valid JPEG.
	_, err = jpeg.Decode(bytes.NewReader(injected))
	if err != nil {
		t.Fatalf("injected JPEG not decodable: %v", err)
	}
}

func TestInjectJPEGDescription_LongDescription(t *testing.T) {
	data := makeJPEG(t, 16, 16)
	desc := "stillwater:v1|source=fanarttv|fetched=2026-03-17T22:00:00Z|url=https://assets.fanart.tv/image/very-long-path/to/some-image-123456789.jpg|dhash=a1b2c3d4e5f6a7b8|rule=thumb_exists|mode=auto"

	injected, err := injectJPEGDescription(data, desc)
	if err != nil {
		t.Fatalf("injectJPEGDescription: %v", err)
	}

	got, err := readJPEGDescription(injected)
	if err != nil {
		t.Fatalf("readJPEGDescription: %v", err)
	}
	if got != desc {
		t.Errorf("round-trip mismatch for long description:\n  got  %q\n  want %q", got, desc)
	}

	// Also verify decodability.
	_, err = jpeg.Decode(bytes.NewReader(injected))
	if err != nil {
		t.Fatalf("injected JPEG with long desc not decodable: %v", err)
	}
}

func TestInjectJPEGDescription_ShortInlineValue(t *testing.T) {
	// Test a very short description (<=4 bytes including null) which is stored
	// inline in the IFD entry rather than at an offset.
	data := makeJPEG(t, 8, 8)
	desc := "abc" // 3 chars + null = 4 bytes, fits inline

	injected, err := injectJPEGDescription(data, desc)
	if err != nil {
		t.Fatalf("injectJPEGDescription: %v", err)
	}

	got, err := readJPEGDescription(injected)
	if err != nil {
		t.Fatalf("readJPEGDescription: %v", err)
	}
	if got != desc {
		t.Errorf("inline round-trip: got %q, want %q", got, desc)
	}
}

func TestReadJPEGDescription_NoEXIF(t *testing.T) {
	data := makeJPEG(t, 16, 16)
	got, err := readJPEGDescription(data)
	if err != nil {
		t.Fatalf("readJPEGDescription: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string for JPEG without EXIF, got %q", got)
	}
}

func TestInjectJPEGDescription_ReplacesExisting(t *testing.T) {
	data := makeJPEG(t, 16, 16)

	// Inject first description.
	first, err := injectJPEGDescription(data, "first-description")
	if err != nil {
		t.Fatalf("first inject: %v", err)
	}

	// Inject second description (should replace the first).
	second, err := injectJPEGDescription(first, "second-description")
	if err != nil {
		t.Fatalf("second inject: %v", err)
	}

	got, err := readJPEGDescription(second)
	if err != nil {
		t.Fatalf("readJPEGDescription: %v", err)
	}
	if got != "second-description" {
		t.Errorf("expected replaced description, got %q", got)
	}
}

func TestStripAPP1_NoAPP1(t *testing.T) {
	data := makeJPEG(t, 8, 8)
	stripped := stripAPP1(data)

	// Should be valid and decodable.
	_, err := jpeg.Decode(bytes.NewReader(stripped))
	if err != nil {
		t.Fatalf("stripped JPEG not decodable: %v", err)
	}
}

func TestInjectJPEGDescription_InvalidSOI(t *testing.T) {
	_, err := injectJPEGDescription([]byte{0x00, 0x00, 0x00}, "test")
	if err == nil {
		t.Fatal("expected error for invalid JPEG data, got nil")
	}
}

// ---------------------------------------------------------------------------
// Task 3: PNG tEXt chunk injection and reading
// ---------------------------------------------------------------------------

func TestInjectPNGDescription_RoundTrip(t *testing.T) {
	data := makePNG(t, 32, 32)
	desc := "stillwater:v1|source=musicbrainz|mode=manual"

	injected, err := injectPNGDescription(data, desc)
	if err != nil {
		t.Fatalf("injectPNGDescription: %v", err)
	}

	got, err := readPNGDescription(injected)
	if err != nil {
		t.Fatalf("readPNGDescription: %v", err)
	}
	if got != desc {
		t.Errorf("round-trip mismatch:\n  got  %q\n  want %q", got, desc)
	}
}

func TestInjectPNGDescription_StillDecodable(t *testing.T) {
	data := makePNG(t, 32, 32)
	desc := "stillwater:v1|source=test"

	injected, err := injectPNGDescription(data, desc)
	if err != nil {
		t.Fatalf("injectPNGDescription: %v", err)
	}

	_, err = png.Decode(bytes.NewReader(injected))
	if err != nil {
		t.Fatalf("injected PNG not decodable: %v", err)
	}
}

func TestReadPNGDescription_NoTextChunk(t *testing.T) {
	data := makePNG(t, 16, 16)
	got, err := readPNGDescription(data)
	if err != nil {
		t.Fatalf("readPNGDescription: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string for PNG without tEXt, got %q", got)
	}
}

func TestInjectPNGDescription_ReplacesExisting(t *testing.T) {
	data := makePNG(t, 16, 16)

	first, err := injectPNGDescription(data, "first-value")
	if err != nil {
		t.Fatalf("first inject: %v", err)
	}

	second, err := injectPNGDescription(first, "second-value")
	if err != nil {
		t.Fatalf("second inject: %v", err)
	}

	got, err := readPNGDescription(second)
	if err != nil {
		t.Fatalf("readPNGDescription: %v", err)
	}
	if got != "second-value" {
		t.Errorf("expected replaced description, got %q", got)
	}
}

func TestInjectPNGDescription_InvalidSignature(t *testing.T) {
	_, err := injectPNGDescription([]byte{0x00, 0x00, 0x00, 0x00}, "test")
	if err == nil {
		t.Fatal("expected error for invalid PNG data, got nil")
	}
}

// ---------------------------------------------------------------------------
// Task 4: Public API -- InjectMeta / ReadProvenance
// ---------------------------------------------------------------------------

func TestInjectMeta_NilMeta(t *testing.T) {
	data := makeJPEG(t, 8, 8)
	got, err := InjectMeta(data, nil)
	if err != nil {
		t.Fatalf("InjectMeta(nil): %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("InjectMeta(nil) should return data unchanged")
	}
}

func TestInjectMeta_JPEG(t *testing.T) {
	data := makeJPEG(t, 16, 16)
	meta := &ExifMeta{Source: "fanarttv", Mode: "auto"}

	injected, err := InjectMeta(data, meta)
	if err != nil {
		t.Fatalf("InjectMeta JPEG: %v", err)
	}

	// Verify it is decodable.
	_, err = jpeg.Decode(bytes.NewReader(injected))
	if err != nil {
		t.Fatalf("injected JPEG not decodable: %v", err)
	}

	// Verify we can read back the description.
	desc, err := readJPEGDescription(injected)
	if err != nil {
		t.Fatalf("readJPEGDescription: %v", err)
	}
	if desc != meta.Marshal() {
		t.Errorf("description mismatch: got %q, want %q", desc, meta.Marshal())
	}
}

func TestInjectMeta_PNG(t *testing.T) {
	data := makePNG(t, 16, 16)
	meta := &ExifMeta{Source: "musicbrainz", Mode: "manual"}

	injected, err := InjectMeta(data, meta)
	if err != nil {
		t.Fatalf("InjectMeta PNG: %v", err)
	}

	_, err = png.Decode(bytes.NewReader(injected))
	if err != nil {
		t.Fatalf("injected PNG not decodable: %v", err)
	}

	desc, err := readPNGDescription(injected)
	if err != nil {
		t.Fatalf("readPNGDescription: %v", err)
	}
	if desc != meta.Marshal() {
		t.Errorf("description mismatch: got %q, want %q", desc, meta.Marshal())
	}
}

func TestInjectMeta_UnknownFormat(t *testing.T) {
	data := []byte("not an image at all")
	meta := &ExifMeta{Source: "test"}

	got, err := InjectMeta(data, meta)
	if err != nil {
		t.Fatalf("InjectMeta unknown format: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("InjectMeta on unknown format should return data unchanged")
	}
}

func TestReadProvenance_JPEG(t *testing.T) {
	data := makeJPEG(t, 16, 16)
	ts := time.Date(2026, 3, 17, 22, 0, 0, 0, time.UTC)
	meta := &ExifMeta{
		Source:  "fanarttv",
		Fetched: ts,
		URL:     "https://example.com/img.jpg",
		DHash:   "a1b2c3d4e5f6a7b8",
		Rule:    "thumb_exists",
		Mode:    "auto",
	}

	injected, err := InjectMeta(data, meta)
	if err != nil {
		t.Fatalf("InjectMeta: %v", err)
	}

	// Write to temp file and read back.
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jpg")
	if err := os.WriteFile(path, injected, 0644); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}

	got, err := ReadProvenance(path)
	if err != nil {
		t.Fatalf("ReadProvenance: %v", err)
	}
	if got == nil {
		t.Fatal("ReadProvenance returned nil for tagged image")
	}
	if got.Source != meta.Source {
		t.Errorf("Source: got %q, want %q", got.Source, meta.Source)
	}
	if !got.Fetched.Equal(meta.Fetched) {
		t.Errorf("Fetched: got %v, want %v", got.Fetched, meta.Fetched)
	}
	if got.URL != meta.URL {
		t.Errorf("URL: got %q, want %q", got.URL, meta.URL)
	}
	if got.DHash != meta.DHash {
		t.Errorf("DHash: got %q, want %q", got.DHash, meta.DHash)
	}
	if got.Rule != meta.Rule {
		t.Errorf("Rule: got %q, want %q", got.Rule, meta.Rule)
	}
	if got.Mode != meta.Mode {
		t.Errorf("Mode: got %q, want %q", got.Mode, meta.Mode)
	}
}

func TestReadProvenance_PNG(t *testing.T) {
	data := makePNG(t, 16, 16)
	meta := &ExifMeta{Source: "musicbrainz", Mode: "manual"}

	injected, err := InjectMeta(data, meta)
	if err != nil {
		t.Fatalf("InjectMeta: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "test.png")
	if err := os.WriteFile(path, injected, 0644); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}

	got, err := ReadProvenance(path)
	if err != nil {
		t.Fatalf("ReadProvenance: %v", err)
	}
	if got == nil {
		t.Fatal("ReadProvenance returned nil for tagged image")
	}
	if got.Source != meta.Source {
		t.Errorf("Source: got %q, want %q", got.Source, meta.Source)
	}
	if got.Mode != meta.Mode {
		t.Errorf("Mode: got %q, want %q", got.Mode, meta.Mode)
	}
}

func TestReadProvenance_UntaggedImage(t *testing.T) {
	data := makeJPEG(t, 8, 8)

	dir := t.TempDir()
	path := filepath.Join(dir, "plain.jpg")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}

	got, err := ReadProvenance(path)
	if err != nil {
		t.Fatalf("ReadProvenance: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for untagged image, got %+v", got)
	}
}

func TestReadProvenance_NonStillwaterDescription(t *testing.T) {
	// An image with a non-Stillwater ImageDescription should return nil, nil.
	data := makeJPEG(t, 8, 8)
	injected, err := injectJPEGDescription(data, "Photo by John Doe")
	if err != nil {
		t.Fatalf("injectJPEGDescription: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "foreign.jpg")
	if err := os.WriteFile(path, injected, 0644); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}

	got, err := ReadProvenance(path)
	if err != nil {
		t.Fatalf("ReadProvenance: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for non-Stillwater description, got %+v", got)
	}
}

func TestReadProvenance_FileNotFound(t *testing.T) {
	_, err := ReadProvenance("/nonexistent/path/image.jpg")
	if err == nil {
		t.Fatal("expected error for nonexistent file, got nil")
	}
}
