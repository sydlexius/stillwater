package image

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register WebP decoder

	"github.com/sydlexius/stillwater/internal/httpsafe"
	"github.com/sydlexius/stillwater/internal/version"
)

// RemoteImageInfo holds dimension and size metadata retrieved from a remote image URL.
type RemoteImageInfo struct {
	Width    int
	Height   int
	FileSize int64
}

// ProbeRemoteImage fetches a remote image URL and decodes its dimensions.
// It also reads Content-Length from the response for file size. The HTTP
// client uses httpsafe.SafeClient to block SSRF targets (loopback, link-local,
// RFC 1918 private addresses).
func ProbeRemoteImage(ctx context.Context, rawURL string) (*RemoteImageInfo, error) {
	return ProbeRemoteImageWithClient(ctx, rawURL, httpsafe.SafeClient(10*time.Second))
}

// ProbeRemoteImageWithClient is the testable core of ProbeRemoteImage with an
// injectable HTTP client. Production code should use ProbeRemoteImage; this
// variant exists so that callers that already hold a test-safe *http.Client
// (e.g. httptest.Server.Client()) can bypass the SSRF-safe transport in tests.
func ProbeRemoteImageWithClient(ctx context.Context, rawURL string, client *http.Client) (*RemoteImageInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	// Wikimedia Commons blocks requests without a proper User-Agent.
	req.Header.Set("User-Agent", version.UserAgent("Stillwater", "https://github.com/sydlexius/stillwater"))

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching image: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body) // drain body to allow connection reuse
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var fileSize int64
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		fileSize, _ = strconv.ParseInt(cl, 10, 64)
	}

	// Limit read to 5MB to prevent excessive memory usage for probing.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if fileSize == 0 {
		fileSize = int64(len(data))
	}

	w, h, err := GetDimensions(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decoding dimensions: %w", err)
	}

	return &RemoteImageInfo{Width: w, Height: h, FileSize: fileSize}, nil
}

// Supported image format names.
const (
	FormatJPEG = "jpeg"
	FormatPNG  = "png"
	FormatWebP = "webp"
)

// DetectFormat reads the first bytes from r to identify the image format.
// Returns "jpeg", "png", or "webp". The returned reader replays the consumed bytes.
func DetectFormat(r io.Reader) (format string, replay io.Reader, err error) {
	// Read enough bytes for magic number detection (12 bytes covers all formats)
	buf := make([]byte, 12)
	n, err := io.ReadFull(r, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return "", nil, fmt.Errorf("reading header: %w", err)
	}
	buf = buf[:n]

	replay = io.MultiReader(bytes.NewReader(buf), r)

	if n >= 3 && buf[0] == 0xFF && buf[1] == 0xD8 && buf[2] == 0xFF {
		return FormatJPEG, replay, nil
	}
	if n >= 8 && string(buf[:8]) == "\x89PNG\r\n\x1a\n" {
		return FormatPNG, replay, nil
	}
	if n >= 12 && string(buf[:4]) == "RIFF" && string(buf[8:12]) == "WEBP" {
		return FormatWebP, replay, nil
	}

	return "", replay, fmt.Errorf("unrecognized image format")
}

// GetDimensions decodes only the image header to read width and height.
func GetDimensions(r io.Reader) (width, height int, err error) {
	cfg, _, err := image.DecodeConfig(r)
	if err != nil {
		return 0, 0, fmt.Errorf("decoding image config: %w", err)
	}
	return cfg.Width, cfg.Height, nil
}

// IsLowResolution reports whether the image dimensions fall below the minimum
// acceptable resolution for the given image type.
//
//   - banner:           758 x 140
//   - fanart/background: 960 x 540
//   - logo/hdlogo:      400 x 155
//   - default:          500 x 500 (thumb, poster, folder)
//
// Provider-specific aliases (hdlogo, background, widethumb) are normalized to
// their base types before the threshold is applied.
// Returns false if either dimension is zero (unknown).
func IsLowResolution(w, h int, imageType string) bool {
	if w == 0 || h == 0 {
		return false
	}
	// Normalize provider-specific aliases to base types.
	switch imageType {
	case "hdlogo":
		imageType = "logo"
	case "background":
		imageType = "fanart"
	case "widethumb":
		imageType = "thumb"
	}
	switch imageType {
	case "banner":
		return w < 758 || h < 140
	case "fanart":
		return w < 960 || h < 540
	case "logo":
		return w < 400 || h < 155
	default: // thumb, poster, folder
		return w < 500 || h < 500
	}
}

// Resize decodes the image from src, scales it to fit within maxWidth x maxHeight
// while maintaining aspect ratio, and encodes the result. Returns the image bytes
// and the output format. If the image already fits, it is re-encoded without scaling.
func Resize(src io.Reader, maxWidth, maxHeight int) ([]byte, string, error) {
	format, replay, err := DetectFormat(src)
	if err != nil {
		return nil, "", fmt.Errorf("detecting format: %w", err)
	}

	img, release, err := decodeWithLimit(replay)
	if err != nil {
		return nil, "", err
	}
	defer release()

	bounds := img.Bounds()
	origW := bounds.Dx()
	origH := bounds.Dy()

	newW, newH := fitDimensions(origW, origH, maxWidth, maxHeight)

	if newW != origW || newH != origH {
		dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, draw.Over, nil)
		img = dst
	}

	// WebP input is converted to PNG (no WebP encoder available)
	outFormat := format
	if outFormat == FormatWebP {
		outFormat = FormatPNG
	}

	data, err := encode(img, outFormat, 85)
	if err != nil {
		return nil, "", err
	}

	return data, outFormat, nil
}

// ConvertFormat decodes src and re-encodes it in a storage-safe format.
// JPEG and PNG are returned as-is (bytes are passed through without re-encoding).
// WebP is converted to PNG because no WebP encoder is available.
// Use this instead of Resize when no dimension cap is desired.
func ConvertFormat(src io.Reader) ([]byte, string, error) {
	format, replay, err := DetectFormat(src)
	if err != nil {
		return nil, "", fmt.Errorf("detecting format: %w", err)
	}

	if format != FormatWebP {
		data, readErr := io.ReadAll(replay)
		if readErr != nil {
			return nil, "", fmt.Errorf("reading image: %w", readErr)
		}
		return data, format, nil
	}

	// WebP: decode and re-encode as PNG.
	decoded, release, err := decodeWithLimit(replay)
	if err != nil {
		return nil, "", err
	}
	defer release()
	data, err := encode(decoded, FormatPNG, 85)
	if err != nil {
		return nil, "", err
	}
	return data, FormatPNG, nil
}

// Optimize re-encodes the image at the given quality setting.
// For JPEG, quality controls compression (1-100). For PNG, quality is ignored.
func Optimize(src io.Reader, format string, quality int) ([]byte, error) {
	img, release, err := decodeWithLimit(src)
	if err != nil {
		return nil, err
	}
	defer release()

	return encode(img, format, quality)
}

// ConvertToFormat decodes the source image and re-encodes it in the target format.
// Supported targets: "jpeg", "png".
func ConvertToFormat(src io.Reader, targetFormat string) ([]byte, error) {
	if targetFormat != FormatJPEG && targetFormat != FormatPNG {
		return nil, fmt.Errorf("unsupported target format: %s", targetFormat)
	}

	img, release, err := decodeWithLimit(src)
	if err != nil {
		return nil, err
	}
	defer release()

	return encode(img, targetFormat, 85)
}

// ValidateAspectRatio checks whether the given dimensions match the expected
// aspect ratio within the specified tolerance (e.g., 0.1 for 10%).
func ValidateAspectRatio(width, height int, expected, tolerance float64) bool {
	if height == 0 || expected == 0 {
		return false
	}
	actual := float64(width) / float64(height)
	return math.Abs(actual-expected)/expected <= tolerance
}

// TrimAlphaBounds returns the tight content bounding box of a PNG image,
// excluding pixels with alpha <= threshold. Returns the content rect and
// original bounds. Non-PNG images return the full image bounds unchanged.
// If no visible pixels are found, content equals original.
func TrimAlphaBounds(src io.Reader, threshold uint8) (content, original image.Rectangle, err error) {
	format, replay, detectErr := DetectFormat(src)
	if detectErr != nil {
		return image.Rectangle{}, image.Rectangle{}, fmt.Errorf("detecting format: %w", detectErr)
	}

	if format != FormatPNG {
		cfg, _, cfgErr := image.DecodeConfig(replay)
		if cfgErr != nil {
			return image.Rectangle{}, image.Rectangle{}, fmt.Errorf("decoding image config: %w", cfgErr)
		}
		bounds := image.Rect(0, 0, cfg.Width, cfg.Height)
		return bounds, bounds, nil
	}

	decoded, release, decodeErr := decodeWithLimit(replay)
	if decodeErr != nil {
		return image.Rectangle{}, image.Rectangle{}, decodeErr
	}
	defer release()

	bounds := decoded.Bounds()

	minX, minY := bounds.Max.X, bounds.Max.Y
	maxX, maxY := bounds.Min.X-1, bounds.Min.Y-1

	thresh := uint32(threshold) << 8
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			_, _, _, a := decoded.At(x, y).RGBA()
			if a > thresh {
				if x < minX {
					minX = x
				}
				if x > maxX {
					maxX = x
				}
				if y < minY {
					minY = y
				}
				if y > maxY {
					maxY = y
				}
			}
		}
	}

	// No visible pixels found -- content equals original.
	if maxX < minX || maxY < minY {
		return bounds, bounds, nil
	}

	// maxX/maxY are inclusive, so add 1 for the rectangle's exclusive bound.
	content = image.Rect(minX, minY, maxX+1, maxY+1)
	return content, bounds, nil
}

// contentBoundsFromImage scans a decoded image to find the bounding box of
// "content" pixels. For PNG (isPNG=true), content has alpha above half-opaque.
// For non-PNG, content is any pixel that is not near-white (all RGB > 240).
// If no content pixels are found, returns original bounds unchanged.
func contentBoundsFromImage(decoded image.Image, isPNG bool) image.Rectangle {
	bounds := decoded.Bounds()
	minX, minY := bounds.Max.X, bounds.Max.Y
	maxX, maxY := bounds.Min.X-1, bounds.Min.Y-1

	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			var isContent bool
			if isPNG {
				_, _, _, a := decoded.At(x, y).RGBA()
				isContent = a > (128 << 8)
			} else {
				r, g, b, _ := decoded.At(x, y).RGBA()
				r8, g8, b8 := r>>8, g>>8, b>>8
				nearWhite := r8 > 240 && g8 > 240 && b8 > 240
				isContent = !nearWhite
			}
			if isContent {
				if x < minX {
					minX = x
				}
				if x > maxX {
					maxX = x
				}
				if y < minY {
					minY = y
				}
				if y > maxY {
					maxY = y
				}
			}
		}
	}

	if maxX < minX || maxY < minY {
		return bounds
	}
	return image.Rect(minX, minY, maxX+1, maxY+1)
}

// ContentBounds returns the bounding box of "content" pixels in any image.
// For PNG: non-content pixels have alpha below the threshold (same as TrimAlphaBounds).
// For non-PNG (JPG etc.): non-content pixels are near-white (all RGB > 240),
// which detects whitespace borders.
// If no content pixels are found, content equals original.
func ContentBounds(src io.Reader) (content, original image.Rectangle, err error) {
	format, replay, detectErr := DetectFormat(src)
	if detectErr != nil {
		return image.Rectangle{}, image.Rectangle{}, fmt.Errorf("detecting format: %w", detectErr)
	}

	decoded, release, decodeErr := decodeWithLimit(replay)
	if decodeErr != nil {
		return image.Rectangle{}, image.Rectangle{}, decodeErr
	}
	defer release()

	bounds := decoded.Bounds()
	content = contentBoundsFromImage(decoded, format == FormatPNG)
	return content, bounds, nil
}

// TrimWithMargin crops an image to its content bounds (determined by
// contentBoundsFromImage) plus a configurable margin in pixels on each side.
// The margin is clamped to the original image bounds.
func TrimWithMargin(src io.Reader, margin int) ([]byte, string, error) {
	if margin < 0 {
		margin = 0
	}

	format, replay, err := DetectFormat(src)
	if err != nil {
		return nil, "", fmt.Errorf("detecting format: %w", err)
	}

	decoded, release, err := decodeWithLimit(replay)
	if err != nil {
		return nil, "", err
	}
	defer release()

	bounds := decoded.Bounds()
	content := contentBoundsFromImage(decoded, format == FormatPNG)

	// No content found -- return original unchanged.
	if content == bounds {
		data, encErr := encode(decoded, format, 0)
		return data, format, encErr
	}

	// Expand content rect by margin, clamped to image bounds.
	cropMinX := content.Min.X - margin
	cropMinY := content.Min.Y - margin
	cropMaxX := content.Max.X + margin
	cropMaxY := content.Max.Y + margin
	if cropMinX < bounds.Min.X {
		cropMinX = bounds.Min.X
	}
	if cropMinY < bounds.Min.Y {
		cropMinY = bounds.Min.Y
	}
	if cropMaxX > bounds.Max.X {
		cropMaxX = bounds.Max.X
	}
	if cropMaxY > bounds.Max.Y {
		cropMaxY = bounds.Max.Y
	}

	rect := image.Rect(cropMinX, cropMinY, cropMaxX, cropMaxY)
	if rect == bounds {
		data, encErr := encode(decoded, format, 0)
		return data, format, encErr
	}

	type subImager interface {
		SubImage(r image.Rectangle) image.Image
	}
	var cropped image.Image
	if si, ok := decoded.(subImager); ok {
		cropped = si.SubImage(rect)
	} else {
		dst := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
		draw.Copy(dst, image.Point{}, decoded, rect, draw.Src, nil)
		cropped = dst
	}

	data, err := encode(cropped, format, 0)
	return data, format, err
}

// TrimAlpha crops the transparent border from a PNG image by finding the
// tightest bounding box that contains all pixels with alpha > threshold (0-255).
// Non-PNG images are returned as-is. If no visible pixels are found, the
// original image is returned unchanged.
func TrimAlpha(src io.Reader, threshold uint8) ([]byte, string, error) {
	format, replay, err := DetectFormat(src)
	if err != nil {
		return nil, "", fmt.Errorf("detecting format: %w", err)
	}
	if format != FormatPNG {
		data, readErr := io.ReadAll(replay)
		return data, format, readErr
	}

	decoded, release, err := decodeWithLimit(replay)
	if err != nil {
		return nil, "", err
	}
	defer release()

	bounds := decoded.Bounds()

	// Reuse TrimAlphaBounds logic inline to avoid re-decoding the image.
	minX, minY := bounds.Max.X, bounds.Max.Y
	maxX, maxY := bounds.Min.X-1, bounds.Min.Y-1

	thresh := uint32(threshold) << 8
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			_, _, _, a := decoded.At(x, y).RGBA()
			if a > thresh {
				if x < minX {
					minX = x
				}
				if x > maxX {
					maxX = x
				}
				if y < minY {
					minY = y
				}
				if y > maxY {
					maxY = y
				}
			}
		}
	}

	// No visible pixels found -- return original unchanged.
	if maxX < minX || maxY < minY {
		data, err := encode(decoded, FormatPNG, 0)
		return data, FormatPNG, err
	}

	// maxX/maxY are inclusive, so add 1 for the rectangle's exclusive bound.
	rect := image.Rect(minX, minY, maxX+1, maxY+1)

	type subImager interface {
		SubImage(r image.Rectangle) image.Image
	}
	var cropped image.Image
	if si, ok := decoded.(subImager); ok {
		cropped = si.SubImage(rect)
	} else {
		dst := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
		draw.Copy(dst, image.Point{}, decoded, rect, draw.Src, nil)
		cropped = dst
	}

	data, err := encode(cropped, FormatPNG, 0)
	return data, FormatPNG, err
}

// Crop extracts a sub-rectangle from the source image and returns the result.
func Crop(src io.Reader, x, y, w, h int) ([]byte, string, error) {
	format, replay, err := DetectFormat(src)
	if err != nil {
		return nil, "", fmt.Errorf("detecting format: %w", err)
	}

	img, release, err := decodeWithLimit(replay)
	if err != nil {
		return nil, "", err
	}
	defer release()

	rect := image.Rect(x, y, x+w, y+h)
	bounds := img.Bounds()
	if !rect.In(bounds) {
		return nil, "", fmt.Errorf("crop rectangle %v outside image bounds %v", rect, bounds)
	}

	// SubImage is supported by all standard image types
	type subImager interface {
		SubImage(r image.Rectangle) image.Image
	}
	si, ok := img.(subImager)
	if !ok {
		// Fallback: draw into new RGBA
		dst := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Copy(dst, image.Point{}, img, rect, draw.Src, nil)
		img = dst
	} else {
		img = si.SubImage(rect)
	}

	outFormat := format
	if outFormat == FormatWebP {
		outFormat = FormatPNG
	}

	data, err := encode(img, outFormat, 85)
	if err != nil {
		return nil, "", err
	}

	return data, outFormat, nil
}

// Size limits for image decoding to prevent OOM on huge or maliciously
// crafted images (decompression-bomb style: a tiny file that declares an
// enormous pixel count). Applied uniformly via decodeWithLimit.
const (
	// MaxDecodeBytes is exported because callers OUTSIDE this package must be
	// able to bound their own reads at the same number. A caller that reads a
	// file into memory before handing it to TrimAlpha/decodeWithLimit has
	// already made the allocation this constant exists to prevent, so the
	// bound has to be applied at that read -- and it has to be THIS bound, not
	// a copy, so the read limit and the decode limit cannot drift apart.
	MaxDecodeBytes  int64 = 25 << 20    // 25 MB (matches upload limit)
	maxDecodePixels int64 = 100_000_000 // 100 megapixels

	// maxDecodedBytes bounds the DECODED footprint, which is the quantity that
	// actually allocates (#2929). The two constants above are proxies for
	// memory, not memory itself: MaxDecodeBytes bounds the COMPRESSED bytes
	// and the compression ratio is attacker-controllable, while
	// maxDecodePixels bounds the pixel COUNT and says nothing about the bytes
	// each pixel costs. image.Decode returns whatever concrete type the
	// decoder chooses, and a 16-bit-per-channel PNG decodes to
	// *image.NRGBA64/RGBA64 at 8 B/px rather than the 4 B/px an 8-bit image
	// uses -- so a measured 808 KB, 10000x10000 16-bit PNG passed BOTH guards
	// and allocated 763 MB.
	//
	// 400 MB is maxDecodePixels * 4, i.e. the 8-bit worst case operators
	// already run today. Choosing the current implicit worst case instead
	// (100 MP * 8 B/px = 800 MB) would change nothing and leave the 763 MB
	// case passing. At 400 MB, every 8-bit image that decodes today still
	// decodes -- the 4 B/px ceiling is exactly where the pixel cap already put
	// it -- and the only inputs newly rejected are >4 B/px ones above about
	// 50 megapixels (16-bit RGBA), which no legitimate artist artwork
	// approaches. This makes the worst-case decoded footprint independent of
	// bit depth, which is what lets docker-compose.yml size the container on
	// one number.
	maxDecodedBytes int64 = maxDecodePixels * 4 // ~400 MB
)

// bytesPerPixel maps the color model image.DecodeConfig reports to a
// conservative UPPER BOUND on the bytes the decoded image will occupy per
// pixel.
//
// THE GUARANTEE: for every model listed explicitly below, the estimate is an
// over-estimate or exact for EVERY concrete type any registered decoder can
// produce from a header reporting that model. Everything else -- including
// models whose header report does not determine the decoded type -- falls
// through to the maximum. A wrong-LOW estimate is the entire bug this guard
// exists to fix, so an unknown decoder (a future stdlib type, a new
// third-party format registered via image.RegisterFormat) must be treated as
// expensive rather than cheap. The cost of guessing high is rejecting an
// image that would have fit; the cost of guessing low is the 763 MB
// allocation.
//
// THE GREYSCALE AND ALPHA MODELS ARE DELIBERATELY ABSENT. They look like the
// cheapest rows in the table (1-2 B/px) and were the most dangerous, because
// image.DecodeConfig cannot see what image.Decode will allocate for them.
// image/png derives the reported ColorModel from the IHDR header ALONE, but
// the decoder ALSO consults the tRNS (transparency) chunk, which DecodeConfig
// never reaches: with tRNS present, cbG16 allocates *image.NRGBA64 (8 B/px)
// instead of *image.Gray16 (2 B/px), and cbG8 allocates *image.NRGBA
// (4 B/px) instead of *image.Gray (1 B/px). See
// $(go env GOROOT)/src/image/png/reader.go -- the model table maps IHDR to
// Gray16Model/GrayModel, while the allocation switch branches on
// d.useTransparent, which only a tRNS chunk sets.
//
// That made a 124 KB greyscale-plus-tRNS PNG project 2 B/px, pass the guard,
// and allocate at 8 B/px -- a deterministic 4x under-estimate reproducing the
// exact #2929 failure the guard was written to stop. There is no cheap
// header-only fix (detecting it would require a format-specific chunk scan
// per decoder), so these models take the fail-large default instead.
//
// THIS DOES REGRESS ONE REAL CASE, stated plainly rather than waved past.
// Greyscale is not PNG-only: image/jpeg's DecodeConfig switches on component
// count and reports GrayModel for a single-component JPEG, so greyscale
// artwork in EITHER format is now estimated at 8 B/px and rejected between
// ~50 MP and the 100 MP pixel cap -- roughly above 7000x7000. For scale, a 4K
// backdrop is 8.3 MP and this package's own low-resolution floors are
// 960x540 (fanart), 758x140 (banner) and 400x155 (logo), three orders of
// magnitude below the new threshold. Nothing this application handles sits in
// that band, and the alternative is the 800 MB allocation above.
func bytesPerPixel(m color.Model) int64 {
	switch m {
	case color.RGBA64Model, color.NRGBA64Model:
		return 8 // image.RGBA64 / image.NRGBA64: 4 channels x 16 bits.
	case color.RGBAModel, color.NRGBAModel:
		return 4 // image.RGBA / image.NRGBA: 4 channels x 8 bits.
	case color.CMYKModel:
		return 4 // image.CMYK: 4 channels x 8 bits.
	case color.YCbCrModel, color.NYCbCrAModel:
		// image.YCbCr is 1-3 B/px depending on chroma subsampling and
		// image.NYCbCrA adds one alpha byte, so 4 is an over-estimate that
		// covers 4:4:4 plus alpha, the densest of the family.
		return 4
	default:
		// Everything else fails large. Two populations land here:
		//
		//   * Greyscale and alpha-only models (Gray16, Gray, Alpha16, Alpha),
		//     excluded on purpose -- see the tRNS mechanism in the doc above.
		//   * Paletted images, which report a color.Palette (not one of the
		//     singleton models) and decode to image.Paletted at 1 B/px.
		//
		// Both are over-estimated rather than under-estimated, which is the
		// correct direction, and 8 B/px only rejects them above 50 MP.
		return 8
	}
}

// noopRelease is the release closure handed back on every decodeWithLimit
// error path. Returning a callable no-op rather than nil means a caller can
// write the `img, release, err := ...; if err != nil { return }; defer
// release()` shape without a nil check, and a caller that defers before
// checking the error still cannot panic.
func noopRelease() {}

// decodeWithLimit reads up to MaxDecodeBytes from r, checks the declared
// pixel dimensions and the projected DECODED footprint via image.DecodeConfig
// (before any pixel buffer is allocated), acquires a process-wide decode slot,
// and only then fully decodes the image. This rejects decompression-bomb style
// inputs (a small file declaring huge dimensions, or declaring a bit depth
// whose decoded cost dwarfs its compressed size) before the expensive
// allocation happens, and bounds how many such allocations can be live at once.
//
// THE CALLER OWNS THE SLOT AND MUST `defer release()`. The slot is NOT freed
// when this function returns, because what consumes memory is not the act of
// decoding -- it is the decoded buffer, which outlives the decode and stays
// live for the whole of the caller's work (the trim paths then allocate a
// SECOND full-size buffer on top of it). Releasing on return bounded
// concurrent DECODES while leaving concurrent decoded IMAGES unbounded, which
// is a bound on the wrong quantity: at limit 1, sixteen decoded images were
// measured live simultaneously. Holding the slot for the buffer's lifetime is
// what makes the documented `per-decode cost x concurrency` peak true.
//
// This is only expressible because the decoded buffer never escapes this
// package: no exported function in internal/image returns an image.Image, so
// every one of the eleven call sites can scope the release to its own frame.
//
// release is never nil and is safe to call more than once.
func decodeWithLimit(r io.Reader) (image.Image, func(), error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxDecodeBytes+1))
	if err != nil {
		return nil, noopRelease, fmt.Errorf("reading image data: %w", err)
	}
	if int64(len(data)) > MaxDecodeBytes {
		return nil, noopRelease, fmt.Errorf("image too large (%d bytes, max %d)", len(data), MaxDecodeBytes)
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, noopRelease, fmt.Errorf("decoding image config: %w", err)
	}
	w := int64(cfg.Width)
	h := int64(cfg.Height)
	if w <= 0 || h <= 0 {
		return nil, noopRelease, fmt.Errorf("invalid image dimensions (%dx%d)", cfg.Width, cfg.Height)
	}
	if h > maxDecodePixels || w > maxDecodePixels/h {
		return nil, noopRelease, fmt.Errorf("image too many pixels (%dx%d, max %d)", cfg.Width, cfg.Height, maxDecodePixels)
	}

	// Staged division rather than w*h*bpp, so the comparison itself cannot
	// overflow int64 on a hostile header (same shape as the pixel check above).
	bpp := bytesPerPixel(cfg.ColorModel)
	if bpp > 0 && h > maxDecodedBytes/bpp/w {
		return nil, noopRelease, fmt.Errorf("image too large decoded (%dx%d at %d bytes/pixel = %d bytes, max %d)",
			cfg.Width, cfg.Height, bpp, w*h*bpp, maxDecodedBytes)
	}

	// Bound how many decoded buffers are live at once (#2928). Acquired only
	// after the cheap header probe above, so a rejected input never consumes a
	// slot.
	rawRelease, err := acquireDecodeSlot()
	if err != nil {
		return nil, noopRelease, err
	}

	// sync.Once rather than an audit of every caller's control flow. The
	// contract handed out here ("release is safe to call more than once")
	// makes a double release harmless at ELEVEN call sites plus every future
	// one, where a bare closure would make correctness depend on each of them
	// getting its branches right forever.
	//
	// Both failure modes are fatal, in different ways. A MISSED release leaks
	// a permit permanently, shrinking the effective bound until the process
	// wedges. A DOUBLE release is worse and less obvious: the raw release is
	// a receive on a buffered channel, so the second one finds the channel
	// empty and BLOCKS THE CALLING GOROUTINE FOREVER -- a request handler
	// hung with no error, no timeout and no log line. (Verified by mutation:
	// dropping this Once makes TestDecodeWithLimit_ReleaseIsIdempotent hang
	// until the test binary's own timeout kills it.) Once() removes that
	// mode outright and leaves the first to be enforced by the single
	// `defer release()` line each caller writes.
	var once sync.Once
	release := func() { once.Do(rawRelease) }

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		// The caller gets an error and will not defer release, so this frame
		// owns the slot and must free it here.
		release()
		return nil, noopRelease, fmt.Errorf("decoding image: %w", err)
	}
	return img, release, nil
}

// GeneratePlaceholder creates a tiny 16x16 base64-encoded data URI from the
// source image. Logos are encoded as PNG (to preserve alpha); all other types
// use JPEG at quality 20. Returns an empty string and an error on decode failure.
// Images exceeding 25 MB or 100 megapixels are rejected to prevent OOM.
func GeneratePlaceholder(src io.Reader, imageType string) (string, error) {
	_, replay, err := DetectFormat(src)
	if err != nil {
		return "", fmt.Errorf("detecting format: %w", err)
	}

	decoded, release, err := decodeWithLimit(replay)
	if err != nil {
		return "", err
	}
	defer release()

	dst := image.NewRGBA(image.Rect(0, 0, 16, 16))
	draw.CatmullRom.Scale(dst, dst.Bounds(), decoded, decoded.Bounds(), draw.Over, nil)

	var buf bytes.Buffer
	var mimeType string
	if imageType == "logo" {
		if err := png.Encode(&buf, dst); err != nil {
			return "", fmt.Errorf("encoding placeholder png: %w", err)
		}
		mimeType = "image/png"
	} else {
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 20}); err != nil {
			return "", fmt.Errorf("encoding placeholder jpeg: %w", err)
		}
		mimeType = "image/jpeg"
	}

	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())
	return "data:" + mimeType + ";base64," + encoded, nil
}

// fitDimensions calculates the scaled dimensions that fit within maxW x maxH
// while preserving the aspect ratio. If the image already fits, returns original dimensions.
func fitDimensions(origW, origH, maxW, maxH int) (int, int) {
	if origW <= maxW && origH <= maxH {
		return origW, origH
	}

	ratioW := float64(maxW) / float64(origW)
	ratioH := float64(maxH) / float64(origH)
	ratio := ratioW
	if ratioH < ratioW {
		ratio = ratioH
	}

	newW := int(math.Round(float64(origW) * ratio))
	newH := int(math.Round(float64(origH) * ratio))

	if newW < 1 {
		newW = 1
	}
	if newH < 1 {
		newH = 1
	}

	return newW, newH
}

// encode writes an image in the specified format to a byte slice.
func encode(img image.Image, format string, quality int) ([]byte, error) {
	var buf bytes.Buffer

	switch format {
	case FormatJPEG:
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
			return nil, fmt.Errorf("encoding jpeg: %w", err)
		}
	case FormatPNG:
		if err := png.Encode(&buf, img); err != nil {
			return nil, fmt.Errorf("encoding png: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported output format: %s", format)
	}

	return buf.Bytes(), nil
}
