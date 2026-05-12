package image

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
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

// FuzzExifMetaParse feeds arbitrary strings to ParseExifMeta. It must never
// panic -- returning an error for malformed input is expected and correct.
func FuzzExifMetaParse(f *testing.F) {
	// Known-good Stillwater provenance strings.
	f.Add("stillwater:v1|source=fanarttv|fetched=2024-01-15T12:00:00Z|url=https://example.com/img.jpg|dhash=abcdef0123456789|rule=artist-thumb|mode=auto")
	f.Add("stillwater:v1|source=musicbrainz|fetched=2025-06-01T00:00:00Z|url=https://mb.org/a/b?x=1&y=2|dhash=0000000000000000|rule=artist-fanart|mode=manual")
	f.Add("stillwater:v1|source=user|fetched=2024-03-20T08:30:00Z|url=https://site.example/path%7Cwith%25pipe|dhash=ffffffffffffffff|rule=|mode=user")
	// Prefix only (no fields).
	f.Add("stillwater:v1")
	// Empty string (valid; returns zero struct).
	f.Add("")
	// Non-Stillwater strings (should be ignored, no error).
	f.Add("not a stillwater string")
	f.Add("ImageDescription=some other app")
	// Truncated EXIF strings.
	f.Add("stillwater:v1|source=")
	f.Add("stillwater:v1|")
	f.Add("stillwater:v1|fetched=2024-01-15T12:00:00Z|source")
	// Fields with no '=' separator.
	f.Add("stillwater:v1|noequalssign|source=x")
	// Mismatched delimiters.
	f.Add("stillwater:v1||source=x||")
	f.Add("stillwater:v1|source=a|b|c=d")
	// Binary-ish bytes in values (high-bit chars, NULs, etc.).
	f.Add("stillwater:v1|source=\x00\xff\xfe\x01|mode=auto")
	f.Add("stillwater:v1|url=https://host/\xff\xfe\x80path")
	// UTF-16 BOMs in input.
	f.Add("\xff\xfestillwater:v1|source=x")
	f.Add("\xfe\xffstillwater:v1|source=x")
	// Very long strings.
	f.Add("stillwater:v1|source=" + string(make([]byte, 1<<20)))
	// Malformed fetched timestamp.
	f.Add("stillwater:v1|fetched=not-a-time")
	f.Add("stillwater:v1|fetched=2024-99-99T99:99:99Z")

	f.Fuzz(func(t *testing.T, s string) {
		// Must not panic. Errors on invalid input are expected.
		_, _ = ParseExifMeta(s)
	})
}

// FuzzDetectFormat feeds arbitrary byte slices to DetectFormat. It must never
// panic. For inputs that succeed (nil error), it verifies that the returned
// replay reader reproduces the original bytes exactly.
func FuzzDetectFormat(f *testing.F) {
	// Valid JPEG magic bytes.
	f.Add(encodeJPEG(f, solidImage(10, 10, color.White)))
	f.Add(encodeJPEG(f, solidImage(100, 100, color.RGBA{R: 200, G: 100, B: 50, A: 255})))
	// Valid PNG magic bytes.
	f.Add(encodePNG(f, solidImage(10, 10, color.Black)))
	f.Add(encodePNG(f, solidImage(50, 50, color.RGBA{R: 0, G: 128, B: 255, A: 255})))
	// Valid WebP magic bytes: RIFF....WEBP header with minimal content.
	f.Add([]byte{'R', 'I', 'F', 'F', 0x1C, 0x00, 0x00, 0x00, 'W', 'E', 'B', 'P', 'V', 'P', '8', 'L'})
	// Valid GIF magic bytes (DetectFormat doesn't return "gif" but the unrecognized-format branch must not panic).
	f.Add([]byte{'G', 'I', 'F', '8', '7', 'a'})
	f.Add([]byte{'G', 'I', 'F', '8', '9', 'a'})
	// Truncated headers: 0, 1, 3, 8 bytes.
	f.Add([]byte{})
	f.Add([]byte{0xFF})
	f.Add([]byte{0xFF, 0xD8, 0xFF})
	f.Add([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})
	// Polyglot: valid PNG signature followed by JPEG content.
	f.Add(append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, []byte{0xFF, 0xD8, 0xFF, 0xE0}...))
	// Valid magic but truncated body (just the first 3 bytes of a JPEG).
	f.Add([]byte{0xFF, 0xD8, 0xFF})
	// Single-byte stream.
	f.Add([]byte{0x00})
	// Random garbage data.
	f.Add([]byte("not an image"))
	f.Add([]byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B})

	f.Fuzz(func(t *testing.T, data []byte) {
		r := bytes.NewReader(data)
		_, replay, err := DetectFormat(r)
		if err != nil {
			// Unrecognized format is expected -- no panic is the goal.
			return
		}

		// For successful detections, the replay reader must reproduce all
		// original bytes so that subsequent decoders see the full image data.
		replayed, readErr := io.ReadAll(replay)
		if readErr != nil {
			t.Fatalf("reading replay reader failed: %v", readErr)
		}
		if !bytes.Equal(replayed, data) {
			t.Errorf("replay reader mismatch: got %d bytes, want %d bytes", len(replayed), len(data))
		}
	})
}

// FuzzImageHashHexParse feeds arbitrary strings to ParseHashHex. It must never
// panic. For successful parses, it round-trips through HashHex and verifies
// that re-parsing yields the same value.
func FuzzImageHashHexParse(f *testing.F) {
	// Valid 16-char lowercase hex strings.
	f.Add("0000000000000000")
	f.Add("ffffffffffffffff")
	f.Add("abcdef0123456789")
	f.Add("deadbeefcafebabe")
	// Valid mixed-case hex.
	f.Add("ABCDEF0123456789")
	f.Add("AbCdEf0123456789")
	f.Add("DeAdBeEfCaFe1234")
	// Zero-length input.
	f.Add("")
	// Oversize inputs.
	f.Add("0000000000000000f")
	f.Add("00000000000000000000000000000000")
	// Hex with leading "0x" prefix (not valid for ParseUint base-16).
	f.Add("0xabcdef01234567")
	f.Add("0XABCDEF01234567")
	// Hex with internal whitespace.
	f.Add("abcdef01 23456789")
	f.Add("abcdef01\t23456789")
	f.Add("abcdef01\n23456789")
	// Unicode lookalike characters (e.g. fullwidth hex digits, Cyrillic).
	f.Add("０１２３４５６７８９０１２３４５") // fullwidth 0-9 * 16
	f.Add("аbcdef0123456789") // Cyrillic 'а' instead of Latin 'a'
	// Exactly 15 chars (too short) and 17 chars (too long).
	f.Add("000000000000000")
	f.Add("00000000000000000")

	f.Fuzz(func(t *testing.T, s string) {
		val, err := ParseHashHex(s)
		if err != nil {
			// Invalid input is expected -- no panic is the goal.
			return
		}

		// Round-trip: HashHex then ParseHashHex must recover the same value.
		hexStr := HashHex(val)
		val2, err2 := ParseHashHex(hexStr)
		if err2 != nil {
			t.Fatalf("ParseHashHex(%q) succeeded but round-trip ParseHashHex(%q) failed: %v", s, hexStr, err2)
		}
		if val != val2 {
			t.Errorf("round-trip mismatch: original=%016x, after HashHex+ParseHashHex=%016x", val, val2)
		}
	})
}
