package image

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
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
// runnable on a small CI runner. (bit depth 16 + color type 6 is exactly what
// the measured file carried; image/png maps it to color.NRGBA64Model, the
// 8 B/px model the real file decoded to.)
func pngHeaderWithDepth(t *testing.T, width, height uint32, bitDepth, colorType byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})

	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], width)
	binary.BigEndian.PutUint32(ihdr[4:8], height)
	ihdr[8] = bitDepth
	ihdr[9] = colorType
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
	data := pngHeaderWithDepth(t, 10_000, 10_000, 16, 6)

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

	_, err := decodeWithLimit(bytes.NewReader(data))
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
	data := pngHeaderWithDepth(t, 10_000, 10_000, 8, 6)

	_, err := decodeWithLimit(bytes.NewReader(data))
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

	decoded, err := decodeWithLimit(bytes.NewReader(buf.Bytes()))
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

// TestBytesPerPixel_NeverUnderestimates walks the concrete image types the
// stdlib and x/image decoders actually return and asserts the estimate is
// greater than or equal to the type's true per-pixel cost. A wrong-LOW
// estimate is the bug being fixed, so the assertion is one-directional.
func TestBytesPerPixel_NeverUnderestimates(t *testing.T) {
	cases := []struct {
		name   string
		model  color.Model
		actual int64 // true bytes per pixel of the concrete decoded type
	}{
		{"RGBA64", color.RGBA64Model, 8},
		{"NRGBA64", color.NRGBA64Model, 8},
		{"RGBA", color.RGBAModel, 4},
		{"NRGBA", color.NRGBAModel, 4},
		{"CMYK", color.CMYKModel, 4},
		{"YCbCr 4:4:4", color.YCbCrModel, 3},
		{"NYCbCrA 4:4:4", color.NYCbCrAModel, 4},
		{"Gray16", color.Gray16Model, 2},
		{"Gray", color.GrayModel, 1},
		{"Alpha16", color.Alpha16Model, 2},
		{"Alpha", color.AlphaModel, 1},
		{"paletted (falls through to the unknown branch)", color.Palette{color.Black, color.White}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := bytesPerPixel(tc.model)
			if got < tc.actual {
				t.Errorf("bytesPerPixel(%s) = %d, UNDER the real cost of %d; an under-estimate is the defect this guard exists to prevent", tc.name, got, tc.actual)
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
	if _, err := decodeWithLimit(bytes.NewReader(data)); err != nil {
		t.Fatalf("fixture failed to decode with a slot free: %v", err)
	}

	release, err := acquireDecodeSlot()
	if err != nil {
		t.Fatalf("holding the only slot: %v", err)
	}
	defer release()

	_, err = decodeWithLimit(bytes.NewReader(data))
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
	over := pngHeaderWithDepth(t, 10_000, 10_000, 16, 6)
	_, err = decodeWithLimit(bytes.NewReader(over))
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
			if _, err := decodeWithLimit(bytes.NewReader(data)); err != nil {
				errs[idx] = fmt.Errorf("goroutine %d: %w", idx, err)
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
