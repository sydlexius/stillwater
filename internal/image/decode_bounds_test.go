package image

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pngHeaderWithDepth builds a PNG stream consisting of the signature and a
// single IHDR chunk declaring width x height at the given bit depth and color
// type. image/png's DecodeConfig reads only IHDR, so this is enough to drive
// decodeWithLimit's pre-decode guards with a ~50-byte file.
//
// This is the same decompression-bomb shape as the existing
// oversizedPNGHeader helper, extended with a bit-depth knob, and it is used
// here in preference to a real encoded image for the over-budget cases: the
// issue's measured reproduction is a 10000x10000 16-bit PNG, and MATERIALIZING
// one in the test costs the very 763 MB allocation the guard exists to
// prevent. The guard sees nothing but the image.Config, so a header that
// declares those dimensions exercises it identically while keeping the test
// runnable on a small CI runner.
//
// The color type is fixed at 6 (truecolor with alpha), which is what the
// measured file carried: image/png maps bit depth 16 + color type 6 to
// color.NRGBA64Model, the 8 B/px model the real file decoded to. It is not a
// parameter because every caller wants that one shape -- the greyscale
// fixtures are built by grayPNGWithTRNS below, which needs a real encoded
// image rather than a bare header.
func pngHeaderWithDepth(t *testing.T, width, height uint32, bitDepth byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})

	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], width)
	binary.BigEndian.PutUint32(ihdr[4:8], height)
	ihdr[8] = bitDepth
	ihdr[9] = 6  // truecolor with alpha
	ihdr[10] = 0 // compression
	ihdr[11] = 0 // filter
	ihdr[12] = 0 // interlace

	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(ihdr)))
	buf.Write(lenBuf[:])

	chunkType := []byte("IHDR")
	buf.Write(chunkType)
	buf.Write(ihdr)

	crc := crc32.NewIEEE()
	crc.Write(chunkType)
	crc.Write(ihdr)
	var crcBuf [4]byte
	binary.BigEndian.PutUint32(crcBuf[:], crc.Sum32())
	buf.Write(crcBuf[:])

	return buf.Bytes()
}

const tooLargeDecodedMsg = "too large decoded"

// TestDecodeWithLimit_Rejects16BitOverFootprintBudget is the #2929 regression.
// The fixture is the measured hostile case: 10000x10000 at 16 bits per
// channel. It passes BOTH pre-existing guards -- the encoded stream is far
// under MaxDecodeBytes (25 MB), and 100,000,000 pixels is exactly AT
// maxDecodePixels, not over it -- yet decodes to *image.NRGBA64 at 8 B/px for
// 800 MB. The assertions below pin all three facts, because a test that only
// asserted "err != nil" would pass even if the footprint check were deleted
// (the truncated fixture fails an unguarded image.Decode too, for the wrong
// reason).
func TestDecodeWithLimit_Rejects16BitOverFootprintBudget(t *testing.T) {
	data := pngHeaderWithDepth(t, 10_000, 10_000, 16)

	// Precondition 1: under the compressed-bytes bound.
	if int64(len(data)) > MaxDecodeBytes {
		t.Fatalf("fixture is %d bytes, over MaxDecodeBytes (%d) -- it would be rejected by the wrong guard", len(data), MaxDecodeBytes)
	}

	// Precondition 2: NOT over the pixel-count cap, and reported as a 16-bit
	// model. If either changes the test stops being a test of this bug.
	cfg, _, cfgErr := image.DecodeConfig(bytes.NewReader(data))
	if cfgErr != nil {
		t.Fatalf("fixture header does not parse: %v", cfgErr)
	}
	if pixels := int64(cfg.Width) * int64(cfg.Height); pixels > maxDecodePixels {
		t.Fatalf("fixture declares %d pixels, over maxDecodePixels (%d) -- it would be rejected by the wrong guard", pixels, maxDecodePixels)
	}
	if got := bytesPerPixel(cfg.ColorModel); got != 8 {
		t.Fatalf("fixture color model estimates %d bytes/pixel, want 8 (the 16-bit case)", got)
	}

	_, release, err := decodeWithLimit(bytes.NewReader(data))
	defer release()
	if err == nil {
		t.Fatal("expected rejection of a 10000x10000 16-bit image (800 MB decoded), got nil")
	}
	if !strings.Contains(err.Error(), tooLargeDecodedMsg) {
		t.Errorf("error = %q, want it to mention %q (i.e. rejected by the decoded-footprint guard, not an incidental decode failure)", err.Error(), tooLargeDecodedMsg)
	}
}

// TestDecodeWithLimit_Accepts8BitAtSameDimensions proves the fix is targeted
// rather than a blanket tightening: the identical dimensions at 8 bits per
// channel project to 400 MB, exactly the budget, and must still get past the
// footprint guard. The fixture is header-only so the decode itself fails on
// missing pixel data -- which is the point: the assertion is that the failure
// is NOT the footprint guard.
func TestDecodeWithLimit_Accepts8BitAtSameDimensions(t *testing.T) {
	data := pngHeaderWithDepth(t, 10_000, 10_000, 8)

	_, release, err := decodeWithLimit(bytes.NewReader(data))
	defer release()
	if err == nil {
		t.Fatal("header-only fixture unexpectedly decoded; test can no longer distinguish the guards")
	}
	if strings.Contains(err.Error(), tooLargeDecodedMsg) {
		t.Errorf("8-bit image at 10000x10000 was rejected by the footprint guard (%q); the guard must not tighten behavior for images that process today", err.Error())
	}
	if !strings.Contains(err.Error(), "decoding image") {
		t.Errorf("error = %q, want the incidental truncated-stream failure (proving the input reached image.Decode)", err.Error())
	}
}

// TestDecodeWithLimit_Accepts16BitUnderBudget pins the other half of
// "targeted": high bit depth is not itself disqualifying. A real, small
// 16-bit image must decode all the way through to a *image.NRGBA64.
func TestDecodeWithLimit_Accepts16BitUnderBudget(t *testing.T) {
	src := image.NewNRGBA64(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			src.SetNRGBA64(x, y, color.NRGBA64{R: 0x1234, G: 0x5678, B: 0x9abc, A: 0xffff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatalf("encoding 16-bit fixture: %v", err)
	}

	decoded, release, err := decodeWithLimit(bytes.NewReader(buf.Bytes()))
	defer release()
	if err != nil {
		t.Fatalf("a 64x64 16-bit image is far under the %d byte budget but was rejected: %v", maxDecodedBytes, err)
	}
	// Either 16-bit concrete type is correct here (image/png picks RGBA64 for
	// a fully-opaque source and NRGBA64 otherwise); what matters is that the
	// fixture really is on the 8 B/px path.
	switch decoded.(type) {
	case *image.NRGBA64, *image.RGBA64:
	default:
		t.Fatalf("decoded to %T, want *image.NRGBA64 or *image.RGBA64 -- the fixture is not exercising the 16-bit path", decoded)
	}
}

// actualBytesPerPixel reports the TRUE per-pixel footprint of a concrete
// decoded image type, read off the type itself rather than off a table. A type
// this switch does not know about is a hard failure, not a default: a silent
// fallback would let a new decoder type slip past the comparison below, which
// is the exact shape of the defect this file exists to catch.
func actualBytesPerPixel(t *testing.T, img image.Image) int64 {
	t.Helper()
	switch img.(type) {
	case *image.RGBA64, *image.NRGBA64:
		return 8
	case *image.RGBA, *image.NRGBA, *image.CMYK, *image.NYCbCrA:
		return 4
	case *image.YCbCr:
		return 3 // worst case, 4:4:4 (no chroma subsampling).
	case *image.Gray16, *image.Alpha16:
		return 2
	case *image.Gray, *image.Alpha, *image.Paletted:
		return 1
	default:
		t.Fatalf("actualBytesPerPixel does not know the concrete type %T; add it rather than defaulting, or this test loses its teeth", img)
		return 0
	}
}

// grayPNGWithTRNS builds a REAL, fully decodable greyscale PNG carrying a tRNS
// (transparency) chunk. image/png has no encoder path that produces this
// shape, so the chunk is spliced into an encoded greyscale image by hand.
//
// This fixture is the whole point of the test below. image/png derives the
// ColorModel that DecodeConfig reports from IHDR ALONE, but the DECODER also
// consults tRNS: with it present, cbG16 allocates *image.NRGBA64 (8 B/px)
// rather than *image.Gray16 (2 B/px), and cbG8 allocates *image.NRGBA
// (4 B/px) rather than *image.Gray (1 B/px). A header-only estimate therefore
// under-estimates such a file by 4x, and no assertion driven by color.Model
// constants can see it -- only a real encode/decode round trip can.
//
// bitDepth is 8 or 16; the tRNS payload is the two-byte grey sample the
// decoder treats as transparent (PNG stores the 8-bit case in the low byte).
func grayPNGWithTRNS(t *testing.T, width, height int, bitDepth int) []byte {
	t.Helper()

	var buf bytes.Buffer
	var err error
	if bitDepth == 16 {
		src := image.NewGray16(image.Rect(0, 0, width, height))
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				src.SetGray16(x, y, color.Gray16{Y: uint16(x*257 + y)})
			}
		}
		err = png.Encode(&buf, src)
	} else {
		src := image.NewGray(image.Rect(0, 0, width, height))
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				src.SetGray(x, y, color.Gray{Y: uint8(x + y)})
			}
		}
		err = png.Encode(&buf, src)
	}
	if err != nil {
		t.Fatalf("encoding %d-bit grey fixture: %v", bitDepth, err)
	}

	// Splice tRNS in immediately after IHDR. The PNG spec requires tRNS
	// before IDAT, and image/png's reader only honors it when it arrives
	// before the image data.
	const sigLen = 8
	// IHDR: 4-byte length + 4-byte type + 13-byte payload + 4-byte CRC.
	const ihdrEnd = sigLen + 4 + 4 + 13 + 4

	raw := buf.Bytes()
	if len(raw) < ihdrEnd {
		t.Fatalf("encoded fixture is only %d bytes, too short to contain an IHDR", len(raw))
	}

	var out bytes.Buffer
	out.Write(raw[:ihdrEnd])
	out.Write(pngChunk(t, "tRNS", []byte{0x00, 0x01}))
	out.Write(raw[ihdrEnd:])
	return out.Bytes()
}

// pngChunk assembles one length-prefixed, CRC-suffixed PNG chunk.
func pngChunk(t *testing.T, chunkType string, payload []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	out.Write(lenBuf[:])
	out.WriteString(chunkType)
	out.Write(payload)

	crc := crc32.NewIEEE()
	crc.Write([]byte(chunkType))
	crc.Write(payload)
	var crcBuf [4]byte
	binary.BigEndian.PutUint32(crcBuf[:], crc.Sum32())
	out.Write(crcBuf[:])
	return out.Bytes()
}

// TestBytesPerPixel_NeverUnderestimatesRealDecodes is the teeth of the
// footprint guard. It drives REAL ENCODED FIXTURES through the SAME two steps
// decodeWithLimit takes -- image.DecodeConfig for the estimate, image.Decode
// for the allocation -- and asserts the estimate is never below what the
// decoder actually allocated.
//
// It replaces an earlier table-driven test that fed color.Model constants in
// and compared numbers out. That test could only ever confirm the lookup table
// matched itself, which is precisely why 403 lines of tests missed the tRNS
// bug: the header-reported model and the allocated type DISAGREE for greyscale
// PNG files carrying tRNS, and no assertion phrased in terms of models can
// observe a disagreement between a model and a type.
func TestBytesPerPixel_NeverUnderestimatesRealDecodes(t *testing.T) {
	// 64x64 is deliberate. The model/type mismatch is a property of the
	// FORMAT, not of the size, so it reproduces identically at 64x64 and at
	// the 8000x8000 the reviewer measured -- without the 512 MB allocation.
	const dim = 64

	opaque16 := image.NewNRGBA64(image.Rect(0, 0, dim, dim))
	for y := 0; y < dim; y++ {
		for x := 0; x < dim; x++ {
			opaque16.SetNRGBA64(x, y, color.NRGBA64{R: uint16(x * 1024), G: uint16(y * 1024), B: 0x4444, A: 0xffff})
		}
	}

	translucent8 := image.NewNRGBA(image.Rect(0, 0, dim, dim))
	for y := 0; y < dim; y++ {
		for x := 0; x < dim; x++ {
			translucent8.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 4), G: uint8(y * 4), B: 90, A: uint8(x * 3)})
		}
	}

	paletted := image.NewPaletted(image.Rect(0, 0, dim, dim), color.Palette{color.Black, color.White, color.Opaque})
	for y := 0; y < dim; y++ {
		for x := 0; x < dim; x++ {
			paletted.SetColorIndex(x, y, uint8((x+y)%3))
		}
	}

	gray16 := image.NewGray16(image.Rect(0, 0, dim, dim))
	gray8 := image.NewGray(image.Rect(0, 0, dim, dim))
	for y := 0; y < dim; y++ {
		for x := 0; x < dim; x++ {
			gray16.SetGray16(x, y, color.Gray16{Y: uint16(x*257 + y)})
			gray8.SetGray(x, y, color.Gray{Y: uint8(x + y)})
		}
	}

	rgbaJPEG := image.NewRGBA(image.Rect(0, 0, dim, dim))
	for y := 0; y < dim; y++ {
		for x := 0; x < dim; x++ {
			rgbaJPEG.SetRGBA(x, y, color.RGBA{R: uint8(x * 4), G: uint8(y * 4), B: 200, A: 255})
		}
	}
	var jpegBuf bytes.Buffer
	if err := jpeg.Encode(&jpegBuf, rgbaJPEG, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encoding JPEG fixture: %v", err)
	}

	cases := []struct {
		name string
		data []byte
	}{
		// THE REGRESSIONS. Header says Gray16Model/GrayModel; the decoder,
		// having seen tRNS, allocates NRGBA64/NRGBA.
		{"PNG grey 16-bit WITH tRNS (header says Gray16, decodes NRGBA64)", grayPNGWithTRNS(t, dim, dim, 16)},
		{"PNG grey 8-bit WITH tRNS (header says Gray, decodes NRGBA)", grayPNGWithTRNS(t, dim, dim, 8)},

		// The same formats WITHOUT tRNS, where header and type agree. These
		// are what makes the pair above meaningful rather than incidental.
		{"PNG grey 16-bit no tRNS", encodePNGFixture(t, gray16)},
		{"PNG grey 8-bit no tRNS", encodePNGFixture(t, gray8)},

		{"PNG 16-bit RGBA", encodePNGFixture(t, opaque16)},
		{"PNG 8-bit NRGBA (alpha channel)", encodePNGFixture(t, translucent8)},
		{"PNG paletted", encodePNGFixture(t, paletted)},
		{"JPEG (decodes to YCbCr)", jpegBuf.Bytes()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Step 1, exactly as decodeWithLimit does it: estimate from the
			// header alone.
			cfg, _, err := image.DecodeConfig(bytes.NewReader(tc.data))
			if err != nil {
				t.Fatalf("fixture header does not parse: %v", err)
			}
			estimate := bytesPerPixel(cfg.ColorModel)

			// Step 2: decode for real and read the footprint off the
			// concrete type the decoder actually chose.
			img, _, err := image.Decode(bytes.NewReader(tc.data))
			if err != nil {
				t.Fatalf("fixture does not decode: %v", err)
			}
			actual := actualBytesPerPixel(t, img)

			if estimate < actual {
				t.Errorf("header-derived estimate is %d B/px but the decoder allocated %T at %d B/px -- a %.1fx UNDER-estimate, which is exactly the defect the footprint guard exists to prevent",
					estimate, img, actual, float64(actual)/float64(estimate))
			}
		})
	}
}

// encodePNGFixture is a small helper so the fixture table above stays readable.
func encodePNGFixture(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding PNG fixture: %v", err)
	}
	return buf.Bytes()
}

// TestGrayWithTRNS_DecodesWiderThanItsHeaderSuggests pins the PRECONDITION the
// test above depends on. If a future Go release made image/png report the
// post-tRNS model from DecodeConfig, the fixtures would stop exercising the
// mismatch and the guard test would pass vacuously forever. This fails loudly
// in that case instead, so the reason for dropping the cheap greyscale rows
// stays visible.
func TestGrayWithTRNS_DecodesWiderThanItsHeaderSuggests(t *testing.T) {
	cases := []struct {
		name       string
		bitDepth   int
		wantHeader color.Model
		wantType   string
	}{
		{"16-bit", 16, color.Gray16Model, "*image.NRGBA64"},
		{"8-bit", 8, color.GrayModel, "*image.NRGBA"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := grayPNGWithTRNS(t, 32, 32, tc.bitDepth)

			cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("fixture header does not parse: %v", err)
			}
			if cfg.ColorModel != tc.wantHeader {
				t.Fatalf("DecodeConfig reports %v, want the greyscale model -- the fixture no longer reproduces the header/decoder disagreement", cfg.ColorModel)
			}

			img, _, err := image.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("fixture does not decode: %v", err)
			}
			if got := fmt.Sprintf("%T", img); got != tc.wantType {
				t.Fatalf("decoded to %s, want %s -- the tRNS chunk is not being honored, so the fixture proves nothing", got, tc.wantType)
			}
		})
	}
}

// unknownColorModel stands in for a decoder registered after this code was
// written (a future stdlib type, or a third-party format via
// image.RegisterFormat) whose model bytesPerPixel has never seen.
type unknownColorModel struct{}

func (unknownColorModel) Convert(c color.Color) color.Color { return c }

// TestBytesPerPixel_UnknownModelFailsLarge pins the fail-toward-expensive
// direction explicitly. An unrecognized model must estimate the MAXIMUM, so a
// decoder this table does not know about cannot slip an 8 B/px allocation past
// a 4 B/px estimate.
func TestBytesPerPixel_UnknownModelFailsLarge(t *testing.T) {
	if got := bytesPerPixel(unknownColorModel{}); got != 8 {
		t.Errorf("bytesPerPixel(unknown model) = %d, want 8 (the conservative maximum)", got)
	}
	if got := bytesPerPixel(nil); got != 8 {
		t.Errorf("bytesPerPixel(nil) = %d, want 8 (the conservative maximum)", got)
	}
}

// --- #2928: concurrency bound ---

// withDecodeLimit installs a decode-concurrency bound for the duration of a
// test and restores the previous one.
//
// NOT SAFE UNDER t.Parallel(), IN EITHER DIRECTION. It mutates PACKAGE-GLOBAL
// state (decodeSem via SetMaxConcurrentDecodes), so a test calling it must run
// serially: with a parallel neighbor, its Cleanup can restore the bound while
// that neighbor is mid-decode, and -- the worse direction -- a bound-1 test
// here would hand ErrDecodeBusy to any parallel test in this package that
// decodes, failing an unrelated test for a reason nothing in that test
// mentions.
//
// internal/image already has several parallel tests that decode, so the moment
// a decode-bounds test gains t.Parallel() this becomes live cross-test
// pollution. There is none today only because every caller below is serial.
// This repo just shipped a fix for exactly that shape (#2908) -- keep these
// serial.
func withDecodeLimit(t *testing.T, n int) {
	t.Helper()
	prev := MaxConcurrentDecodes()
	SetMaxConcurrentDecodes(n)
	t.Cleanup(func() { SetMaxConcurrentDecodes(prev) })
}

// TestDecodeSlot_PeakConcurrencyRespectsLimit is the #2928 regression. It
// observes the PEAK number of simultaneously-held slots across many more
// contenders than the limit permits, which is the property that actually
// bounds memory -- calling the acquire path N times proves nothing on its own.
//
// Both bounds are asserted: peak must never exceed the limit (the guard
// works), and peak must REACH the limit (the guard is not accidentally
// serializing everything, which would pass an upper-bound-only assertion
// while destroying throughput).
func TestDecodeSlot_PeakConcurrencyRespectsLimit(t *testing.T) {
	const (
		limit      = 3
		contenders = 24
	)
	withDecodeLimit(t, limit)

	var (
		inFlight atomic.Int64
		peak     atomic.Int64
		wg       sync.WaitGroup
	)

	start := make(chan struct{})
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release everyone at once, maximizing contention
			release, err := acquireDecodeSlot()
			if err != nil {
				t.Errorf("acquireDecodeSlot: %v", err)
				return
			}
			defer release()

			now := inFlight.Add(1)
			for {
				old := peak.Load()
				if now <= old || peak.CompareAndSwap(old, now) {
					break
				}
			}
			// Hold the slot long enough that a broken bound would let
			// several contenders overlap observably.
			time.Sleep(2 * time.Millisecond)
			inFlight.Add(-1)
		}()
	}
	close(start)
	wg.Wait()

	if got := peak.Load(); got > limit {
		t.Errorf("peak concurrent decode slots = %d, over the limit of %d -- the bound does not bound", got, limit)
	} else if got != limit {
		t.Errorf("peak concurrent decode slots = %d, want exactly %d; a peak below the limit means the bound is over-serializing", got, limit)
	}
	if got := inFlight.Load(); got != 0 {
		t.Errorf("in-flight count settled at %d, want 0 -- a slot leaked", got)
	}
}

// TestDecodeWithLimit_AcquiresASlot proves the bound is actually WIRED into
// the decode chokepoint, not merely present as a primitive. With the sole slot
// held elsewhere, a decode of a perfectly valid image must fail with
// ErrDecodeBusy rather than proceeding to allocate.
func TestDecodeWithLimit_AcquiresASlot(t *testing.T) {
	withDecodeLimit(t, 1)
	prevTimeout := decodeAcquireTimeout
	decodeAcquireTimeout = 50 * time.Millisecond
	t.Cleanup(func() { decodeAcquireTimeout = prevTimeout })

	data := encodeImage(t, solidImage(32, 32, color.RGBA{R: 10, G: 20, B: 30, A: 255}))

	// Sanity: the fixture decodes fine when a slot is available. Without this
	// the test could pass on a malformed fixture.
	// The release is immediate and NOT deferred: the caller now owns the slot
	// for the decoded image's lifetime, so holding it to the end of the test
	// would starve the acquire below and turn this into a test of itself.
	if _, rel, err := decodeWithLimit(bytes.NewReader(data)); err != nil {
		t.Fatalf("fixture failed to decode with a slot free: %v", err)
	} else {
		rel()
	}

	release, err := acquireDecodeSlot()
	if err != nil {
		t.Fatalf("holding the only slot: %v", err)
	}
	defer release()

	_, _, err = decodeWithLimit(bytes.NewReader(data))
	if err == nil {
		t.Fatal("decode succeeded while the only decode slot was held; decodeWithLimit is not acquiring a slot")
	}
	if !errors.Is(err, ErrDecodeBusy) {
		t.Errorf("error = %v, want ErrDecodeBusy", err)
	}
}

// TestDecodeWithLimit_RejectedInputConsumesNoSlot pins the placement of the
// acquire: the cheap header guards run FIRST, so a hostile input is rejected
// without ever occupying a slot. Were the acquire hoisted above them, an
// attacker could saturate the bound with 50-byte files.
func TestDecodeWithLimit_RejectedInputConsumesNoSlot(t *testing.T) {
	withDecodeLimit(t, 1)
	prevTimeout := decodeAcquireTimeout
	decodeAcquireTimeout = 50 * time.Millisecond
	t.Cleanup(func() { decodeAcquireTimeout = prevTimeout })

	release, err := acquireDecodeSlot()
	if err != nil {
		t.Fatalf("holding the only slot: %v", err)
	}
	defer release()

	// Over-footprint input, with every slot busy. It must come back with the
	// footprint error (rejected before the acquire), never ErrDecodeBusy.
	over := pngHeaderWithDepth(t, 10_000, 10_000, 16)
	_, _, err = decodeWithLimit(bytes.NewReader(over))
	if err == nil {
		t.Fatal("expected the footprint rejection, got nil")
	}
	if errors.Is(err, ErrDecodeBusy) {
		t.Fatal("a header-only hostile input waited for a decode slot; the acquire must sit AFTER the cheap header guards")
	}
	if !strings.Contains(err.Error(), tooLargeDecodedMsg) {
		t.Errorf("error = %q, want the footprint rejection", err.Error())
	}
}

// TestDecodeWithLimit_ConcurrentDecodesAllComplete is the anti-wedge check.
// The rules-pass path already runs decodes under an errgroup with its own
// SetLimit; this bound sits UNDERNEATH that one, process-wide by design. Two
// nested bounds deadlock only if a slot holder can block on acquiring a
// second, so the property to prove is that a decode releases its slot
// unconditionally and a group of decoders far wider than the bound still all
// finish. Run at limit 1 -- the tightest possible bound, and the shape most
// likely to expose a missed release.
func TestDecodeWithLimit_ConcurrentDecodesAllComplete(t *testing.T) {
	withDecodeLimit(t, 1)

	data := encodeImage(t, solidImage(48, 48, color.RGBA{R: 5, G: 6, B: 7, A: 255}))

	const goroutines = 16
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	done := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if _, rel, err := decodeWithLimit(bytes.NewReader(data)); err != nil {
				errs[idx] = fmt.Errorf("goroutine %d: %w", idx, err)
			} else {
				rel()
			}
		}(i)
	}
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("concurrent decodes did not all finish; the bound has wedged")
	}
	for _, err := range errs {
		if err != nil {
			t.Errorf("decode failed under contention: %v", err)
		}
	}
}

// TestSetMaxConcurrentDecodes_RoundTripAndNormalization pins the accessor pair
// and the non-positive normalization, which the settings/boot wiring relies on
// (a stored 0 must not silently disable decoding).
func TestSetMaxConcurrentDecodes_RoundTripAndNormalization(t *testing.T) {
	withDecodeLimit(t, DefaultDecodeConcurrency)

	SetMaxConcurrentDecodes(7)
	if got := MaxConcurrentDecodes(); got != 7 {
		t.Errorf("after SetMaxConcurrentDecodes(7), MaxConcurrentDecodes() = %d, want 7", got)
	}
	for _, n := range []int{0, -1, -100} {
		SetMaxConcurrentDecodes(n)
		if got := MaxConcurrentDecodes(); got != DefaultDecodeConcurrency {
			t.Errorf("after SetMaxConcurrentDecodes(%d), MaxConcurrentDecodes() = %d, want the default %d", n, got, DefaultDecodeConcurrency)
		}
	}
}

// TestDecodeWithLimit_LiveDecodedImagesRespectLimit is the #2928 defect the
// first round MISSED, inverted into a guard.
//
// The bound originally released its slot when decodeWithLimit RETURNED -- and
// it returns the decoded image. Every caller then held that buffer for the
// rest of its work (the trim paths allocating a SECOND full-size buffer on top
// of it) with no slot held at all. That bounded concurrent DECODES while
// leaving concurrent decoded IMAGES unbounded, which is a bound on the wrong
// quantity: measured at limit 1, SIXTEEN decoded images were live at once and
// the heap grew 256 MB.
//
// So this measures the quantity that actually costs memory: how many decoded
// images are simultaneously LIVE in caller frames. The counter is incremented
// on the caller side after decodeWithLimit returns and decremented before
// release(), i.e. across exactly the window in which a real caller is holding
// a full-size buffer. A counter incremented INSIDE the acquire path would go
// green against the broken code, because the semaphore was always entered
// correctly -- it was released too early.
func TestDecodeWithLimit_LiveDecodedImagesRespectLimit(t *testing.T) {
	const (
		limit      = 2
		contenders = 16
	)
	withDecodeLimit(t, limit)

	data := encodeImage(t, solidImage(64, 64, color.RGBA{R: 9, G: 8, B: 7, A: 255}))

	var (
		live atomic.Int64
		peak atomic.Int64
		wg   sync.WaitGroup
	)

	start := make(chan struct{})
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start // release everyone at once, maximizing overlap

			img, release, err := decodeWithLimit(bytes.NewReader(data))
			if err != nil {
				t.Errorf("goroutine %d: decode failed: %v", idx, err)
				return
			}

			now := live.Add(1)
			for {
				old := peak.Load()
				if now <= old || peak.CompareAndSwap(old, now) {
					break
				}
			}
			// Hold the decoded buffer, as every real caller does. Touching it
			// keeps the reference genuinely alive rather than merely in scope.
			_ = img.Bounds()
			time.Sleep(3 * time.Millisecond)

			live.Add(-1)
			release()
		}(i)
	}
	close(start)
	wg.Wait()

	if got := peak.Load(); got > limit {
		t.Errorf("peak SIMULTANEOUSLY-LIVE decoded images = %d, over the limit of %d -- the slot is being released while the caller still holds the buffer, so the bound does not bound memory", got, limit)
	} else if got != limit {
		t.Errorf("peak simultaneously-live decoded images = %d, want exactly %d; a peak below the limit means the bound is over-serializing", got, limit)
	}
	if got := live.Load(); got != 0 {
		t.Errorf("live count settled at %d, want 0", got)
	}
}

// TestDecodeWithLimit_ReleaseIsIdempotent pins the contract decodeWithLimit's
// doc comment hands to eleven call sites: release may be called more than once
// without corrupting the semaphore.
//
// The failure mode is a HANG, not a wrong number. The raw release is a receive
// on a buffered channel, so a second release finds it empty and blocks its
// goroutine forever -- in production, a request handler wedged with no error
// and no log line. Mutation-verified: removing the sync.Once makes this test
// block at the repeated release below until the test binary's own timeout
// kills it, which is exactly the shape the production hang would take.
//
// The assertions past the repeated release cover the other direction, an
// implementation that absorbed the extra release into the channel and thereby
// raised the effective bound.
func TestDecodeWithLimit_ReleaseIsIdempotent(t *testing.T) {
	withDecodeLimit(t, 1)
	prevTimeout := decodeAcquireTimeout
	decodeAcquireTimeout = 50 * time.Millisecond
	t.Cleanup(func() { decodeAcquireTimeout = prevTimeout })

	data := encodeImage(t, solidImage(32, 32, color.RGBA{R: 1, G: 2, B: 3, A: 255}))

	_, release, err := decodeWithLimit(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	release()
	release()
	release() // extra releases must be absorbed, not returned to the channel

	// With the sole slot free and no phantom permits, exactly one acquire must
	// succeed and a second must time out. A double release would let both in.
	first, err := acquireDecodeSlot()
	if err != nil {
		t.Fatalf("first acquire after release: %v", err)
	}
	defer first()

	if _, err := acquireDecodeSlot(); !errors.Is(err, ErrDecodeBusy) {
		t.Fatalf("a second acquire succeeded (err = %v) while the only slot was held; the repeated release returned extra permits and silently raised the bound", err)
	}
}

// TestDecodeWithLimit_FailedDecodeReleasesItsSlot covers the one path where
// decodeWithLimit itself owns the release: the slot is acquired, image.Decode
// then fails, and the caller -- receiving an error -- will not defer anything.
// A missed release here leaks a slot permanently and eventually wedges the
// process, so it is asserted directly rather than inferred.
func TestDecodeWithLimit_FailedDecodeReleasesItsSlot(t *testing.T) {
	withDecodeLimit(t, 1)
	prevTimeout := decodeAcquireTimeout
	decodeAcquireTimeout = 50 * time.Millisecond
	t.Cleanup(func() { decodeAcquireTimeout = prevTimeout })

	// A header-only PNG: it passes every pre-decode guard (so a slot IS
	// acquired) and then fails inside image.Decode on the missing pixel data.
	truncated := pngHeaderWithDepth(t, 64, 64, 8)
	if _, _, err := decodeWithLimit(bytes.NewReader(truncated)); err == nil {
		t.Fatal("header-only fixture unexpectedly decoded; the test can no longer reach the failed-decode path")
	}

	// The sole slot must be free again.
	release, err := acquireDecodeSlot()
	if err != nil {
		t.Fatalf("could not acquire after a FAILED decode (%v) -- the failure path leaked its slot", err)
	}
	release()
}
