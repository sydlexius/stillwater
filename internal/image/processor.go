package image

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register WebP decoder
)

// RemoteImageInfo holds dimension and size metadata retrieved from a remote image URL.
type RemoteImageInfo struct {
	Width    int
	Height   int
	FileSize int64
}

// ProbeRemoteImage fetches a remote image URL and decodes its dimensions.
// It also reads Content-Length from the response for file size.
func ProbeRemoteImage(ctx context.Context, rawURL string) (*RemoteImageInfo, error) {
	client := &http.Client{Timeout: 10 * time.Second}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := client.Do(req) //nolint:gosec // URL comes from trusted provider API
	if err != nil {
		return nil, fmt.Errorf("fetching image: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var fileSize int64
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		fileSize, _ = strconv.ParseInt(cl, 10, 64)
	}

	// Limit read to 5MB to prevent excessive memory usage for probing
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
// Returns false if either dimension is zero (unknown).
func IsLowResolution(w, h int, imageType string) bool {
	if w == 0 || h == 0 {
		return false
	}
	switch imageType {
	case "banner":
		return w < 758 || h < 140
	case "fanart", "background":
		return w < 960 || h < 540
	case "logo", "hdlogo":
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

	img, _, err := image.Decode(replay)
	if err != nil {
		return nil, "", fmt.Errorf("decoding image: %w", err)
	}

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

// Optimize re-encodes the image at the given quality setting.
// For JPEG, quality controls compression (1-100). For PNG, quality is ignored.
func Optimize(src io.Reader, format string, quality int) ([]byte, error) {
	img, _, err := image.Decode(src)
	if err != nil {
		return nil, fmt.Errorf("decoding image: %w", err)
	}

	return encode(img, format, quality)
}

// ConvertToFormat decodes the source image and re-encodes it in the target format.
// Supported targets: "jpeg", "png".
func ConvertToFormat(src io.Reader, targetFormat string) ([]byte, error) {
	if targetFormat != FormatJPEG && targetFormat != FormatPNG {
		return nil, fmt.Errorf("unsupported target format: %s", targetFormat)
	}

	img, _, err := image.Decode(src)
	if err != nil {
		return nil, fmt.Errorf("decoding image: %w", err)
	}

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

// Crop extracts a sub-rectangle from the source image and returns the result.
func Crop(src io.Reader, x, y, w, h int) ([]byte, string, error) {
	format, replay, err := DetectFormat(src)
	if err != nil {
		return nil, "", fmt.Errorf("detecting format: %w", err)
	}

	img, _, err := image.Decode(replay)
	if err != nil {
		return nil, "", fmt.Errorf("decoding image: %w", err)
	}

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
