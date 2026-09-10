package image

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
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

// ErrSVGUnsupported reports that the sniffed content is an SVG document.
// SVG is a valid image format in general, but Stillwater's decode pipeline
// (image/jpeg, image/png, golang.org/x/image/webp) has no SVG decoder, so a
// caller that receives this sentinel should surface a specific "SVG is not
// supported" message rather than falling through to the generic
// "unrecognized image format" text. Callers that need to distinguish this
// case use errors.Is(err, ErrSVGUnsupported) (#3223 review round 2, F1):
// Google Images' ic:trans (transparent) filter, which the logo/banner deep
// links apply, returns SVG results alongside raster ones.
var ErrSVGUnsupported = errors.New("SVG images are not supported")

// svgSniffWindow is how many leading bytes DetectFormat inspects for an SVG
// signature. SVG has no fixed-offset magic number like the raster formats
// below -- it is XML, so a document can legally begin with an XML prolog
// ("<?xml version=...?>") of unbounded length before the "<svg" root element
// appears, or with a DOCTYPE, or with the root element itself. 512 mirrors
// the sniff window net/http.DetectContentType uses for the same problem and
// is comfortably larger than any prolog/DOCTYPE combination seen in
// practice, while staying small enough that reading it eagerly for every
// fetch is free.
const svgSniffWindow = 512

// looksLikeSVG reports whether buf's leading bytes are the start of an SVG
// document, identified as: the FIRST XML start element's local name is
// "svg" (case-insensitive, matching this function's existing policy --
// SVG's own casing is technically case-sensitive, but a false negative here
// is the worse failure mode for this specific error path).
//
// #3223 review round 3, R2 (CodeRabbit): the original implementation was a
// substring match on "<svg" / "<?xml" / "<!doctype" prefixes, which had two
// real bugs, both confirmed with runnable cases before this fix:
//  1. A FALSE POSITIVE: "<svg-not-image>hello</svg-not-image>" starts with
//     the literal bytes "<svg" but is not SVG at all -- any tag whose name
//     happens to start with "svg" (svgdata, svg-icon, ...) was misdetected.
//  2. A FALSE NEGATIVE: "<!-- a comment --><svg>...</svg>" is a completely
//     valid document (comments may precede the root element per the XML
//     spec) that the substring match never caught, because it checked only
//     for a LITERAL PREFIX of "<?xml" or "<!doctype", not "a comment can
//     also precede the root". That case fell through to the generic
//     "unrecognized image format" 502 instead of the specific 422.
//
// A real XML tokenizer (encoding/xml.Decoder) sidesteps both: it correctly
// skips ProcInst (the XML prolog), Directive (DOCTYPE), Comment and
// (whitespace-only) CharData tokens on the way to the first element, and
// reports that element's ACTUAL tag name rather than a prefix of the raw
// bytes -- so "<svg-not-image>" tokenizes to a StartElement named
// "svg-not-image", not "svg". Go's xml package also strips a UTF-8 BOM
// automatically, so no separate BOM-handling step is needed for that case.
//
// dec.Strict = false tolerates the common real-world laxity a fetched image
// URL's body might contain (an unescaped "&", a missing xmlns) that would
// make a strict parse fail before ever reaching the root element -- this
// function only needs the ROOT ELEMENT NAME, not a well-formed document.
//
// #3223 review round 4: the round-3 rewrite above introduced two of its own
// regressions, both reproduced with runnable/table-driven cases before this
// fix (see TestDetectFormat_SVG and TestDetectFormat_SVG_NegativeCases):
//
// H1 -- three real cases the OLD substring version caught but the round-3
// xml.Decoder version missed, all now fixed:
//  1. A non-UTF-8 encoding declaration (iso-8859-1, windows-1252, us-ascii --
//     real legacy Illustrator/browser exports) made xml.Decoder refuse to
//     tokenize at all, because no CharsetReader was configured. Fixed by
//     setting dec.CharsetReader to a passthrough: this function only reads
//     the ASCII tag name, so no actual charset transcoding is needed --
//     accepting the declared encoding without decoding it is sufficient and
//     avoids pulling in golang.org/x/text/encoding for a fact this function
//     never uses.
//  2. A root tag whose closing '>' lands past the svgSniffWindow boundary
//     (realistic: an Inkscape/Illustrator export's xmlns block alone often
//     exceeds 500 bytes) is never emitted as a StartElement token, because
//     xml.Decoder only emits one once it has seen the closing '>' -- a
//     truncated tag produces a token ERROR instead, and this function used
//     to treat every token error as "definitely not SVG".
//  3. A tag truncated mid-attribute (e.g. `<svg width="1`) hits the
//     identical failure as (2) for the same reason.
//     FIX for both (2) and (3): when the decoder errors out before any
//     StartElement has been seen, look at the UNCONSUMED bytes starting at
//     the offset immediately before the failing Token() call (tracked via
//     prevOffset, NOT dec.InputOffset() taken AFTER the error -- the two can
//     differ, since a failed token can itself consume some input before
//     erroring). If those bytes start (case-insensitively, tolerating one
//     namespace prefix like "x:svg", per SVG's practice of sometimes
//     appearing inside a compound document) with "svg" followed by
//     whitespace, '/', '>', or nothing at all (buffer ran out exactly at the
//     tag name), classify as SVG. This recovers exactly the "we can SEE
//     enough of the tag name within the bounded window" cases; a comment or
//     other content that consumes the ENTIRE window before "<svg" ever
//     appears remains correctly unclassifiable (see
//     TestDetectFormat_SVG_LongCommentExceedsWindow) -- that is a genuine
//     information-theoretic limit of any bounded sniff window, not a bug
//     this function can fix without growing svgSniffWindow itself.
//
// H2 -- a NEW false positive round 3 introduced: every CharData token was
// skipped unconditionally as "precedes the root element", so prose text
// containing an embedded "<svg/>" mention ("Moved. See <svg/>") or a binary
// header that happens to tokenize as non-strict CharData ("GIF89a<svg>...")
// were both misclassified as SVG. Fixed by only treating CharData as
// "keep reading" when it is ALL WHITESPACE (after stripping an optional
// leading BOM) -- any non-whitespace content before a root element means
// this was never a real XML/SVG document in the first place.
func looksLikeSVG(buf []byte) bool {
	dec := xml.NewDecoder(bytes.NewReader(buf))
	dec.Strict = false
	// CharsetReader is required for ANY non-UTF-8/US-ASCII encoding
	// declaration (H1a) -- without one, xml.Decoder refuses to tokenize at
	// all rather than assuming a passthrough. This function only inspects
	// the ASCII root tag name, never document content, so accepting the
	// bytes as-is (no real transcoding) is correct here regardless of what
	// the declared encoding actually is.
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }

	var prevOffset int64 // offset immediately BEFORE the current Token() call
	for {
		tok, err := dec.Token()
		if err != nil {
			// H1b/H1c: the decoder failed before reaching a StartElement --
			// either truncation cut the root tag short (crossed the sniff
			// window, or a mid-attribute cut), or this genuinely is not XML
			// at all (a raster image's binary header). Distinguish by
			// inspecting what's left, starting from BEFORE the failing call
			// (prevOffset), not dec.InputOffset() taken after the error --
			// the decoder can advance its offset partway through a token it
			// then fails to complete, so an offset read after the error can
			// point PAST the very bytes that would identify the tag.
			if prevOffset < 0 || prevOffset > int64(len(buf)) {
				return false
			}
			return startsWithSVGOpenTag(buf[prevOffset:])
		}
		if se, ok := tok.(xml.StartElement); ok {
			return strings.EqualFold(se.Name.Local, "svg")
		}
		if cd, ok := tok.(xml.CharData); ok {
			// H2: only whitespace (optionally BOM-prefixed) may precede the
			// root element and still count as "not yet at content". Real
			// prose or binary bytes that happen to tokenize as CharData mean
			// this was never XML/SVG to begin with.
			trimmed := bytes.TrimLeft(bytes.TrimPrefix([]byte(cd), []byte{0xEF, 0xBB, 0xBF}), " \t\r\n")
			if len(trimmed) > 0 {
				return false
			}
		}
		// Any other token (ProcInst, Directive, Comment, whitespace-only
		// CharData) precedes the root element in a well-formed document;
		// keep reading. prevOffset is updated only on this path, i.e. only
		// once a token was FULLY and successfully read.
		prevOffset = dec.InputOffset()
	}
}

// LooksLikeSVG is the exported entry point to looksLikeSVG, for callers
// outside this package that need to classify a COMPLETE, already-bounded
// byte slice as SVG or not -- as opposed to DetectFormat's bounded
// svgSniffWindow sniff, which only inspects a fixed-size prefix.
//
// #3223 review round 5, K1: fetchImageFromURL (internal/api/handlers_image.go)
// already holds the complete fetched body, size-bounded to maxUploadSize
// (25MB, handlers_image.go:36) by the time DetectFormat's 512-byte sniff
// window has already returned the generic "unrecognized image format"
// error. A comment or DOCTYPE longer than that window (see
// TestDetectFormat_SVG_LongCommentExceedsWindow) pushes the real "<svg" root
// element past what DetectFormat can see, so a genuinely-SVG document with
// an unusually long preamble fell through to the generic 502 instead of the
// specific SVG 422. Re-running the SAME tokenizer over the WHOLE body (this
// function, not a second implementation) recovers exactly that case,
// without touching DetectFormat's own deliberately-bounded sniff (kept as
// the FIRST, fast check for the common case: any real fetch of an actual
// image is either resolved or rejected within svgSniffWindow bytes almost
// always, so the expensive whole-body pass is reserved for the rare
// generic-error path where DetectFormat has already given up).
func LooksLikeSVG(buf []byte) bool {
	return looksLikeSVG(buf)
}

// startsWithSVGOpenTag reports whether rest begins with an SVG root
// element's opening tag, as far as a bounded byte window can tell: "<",
// optionally one namespace prefix (e.g. "x:"), then "svg" (case-insensitive,
// matching this package's existing policy), followed by whitespace, '/',
// '>', or the end of the available bytes (the window was truncated exactly
// at the tag name, which still counts as "we can see enough").
//
// This exists specifically for H1b/H1c: a tag the decoder could not fully
// tokenize because its closing '>' (or an attribute value) was cut off by
// the sniff window boundary. It is deliberately narrow -- it only looks at
// the OPENING tag shape, not attributes or document structure -- because
// its caller already established that a real tokenizer could not make
// sense of what follows; this is a best-effort recovery for the one shape
// (root tag truncated by a bounded read) that a full document is never
// going to resolve anyway.
func startsWithSVGOpenTag(rest []byte) bool {
	if len(rest) == 0 || rest[0] != '<' {
		return false
	}
	rest = rest[1:]
	// Optional namespace prefix (e.g. "x:svg"): only consume it if what
	// precedes the ':' looks like a simple XML name token, and only within
	// a short lookahead (a real prefix is short; a long run of ':'-free
	// bytes before any ':' means this isn't a prefix at all).
	const maxPrefixLookahead = 20
	if idx := bytes.IndexByte(rest, ':'); idx >= 0 && idx < maxPrefixLookahead {
		prefix := rest[:idx]
		if len(prefix) > 0 && isSimpleXMLName(prefix) {
			rest = rest[idx+1:]
		}
	}
	lower := bytes.ToLower(rest)
	if !bytes.HasPrefix(lower, []byte("svg")) {
		return false
	}
	after := lower[len("svg"):]
	if len(after) == 0 {
		// The window ran out immediately after the tag name (e.g. `<svg`
		// with nothing more captured) -- still a positive identification of
		// what we can see, not a rejection.
		return true
	}
	switch after[0] {
	case ' ', '\t', '\n', '\r', '/', '>':
		return true
	default:
		// Something else follows "svg" (e.g. "svgdata", "svg-not-image") --
		// this is a different, unrelated tag name, not the SVG root.
		return false
	}
}

// isSimpleXMLName reports whether every byte in name is a plain ASCII
// letter, digit, underscore, or hyphen -- a conservative (not fully
// XML-spec-compliant) approximation of a valid XML Name token, sufficient
// for recognizing an ordinary namespace prefix like "x" or "svg" in
// startsWithSVGOpenTag's bounded lookahead.
func isSimpleXMLName(name []byte) bool {
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

// DetectFormat reads the first bytes from r to identify the image format.
// Returns "jpeg", "png", or "webp". The returned reader replays the consumed bytes.
// An SVG document is detected separately and reported as ErrSVGUnsupported
// (distinct from the generic unrecognized-format error) so callers can
// surface a specific, actionable message.
func DetectFormat(r io.Reader) (format string, replay io.Reader, err error) {
	// Read a window large enough to both sniff SVG (which has no fixed-offset
	// magic number, see svgSniffWindow) and cover the raster magic numbers
	// checked below (12 bytes).
	buf := make([]byte, svgSniffWindow)
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
	if looksLikeSVG(buf) {
		return "", replay, ErrSVGUnsupported
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
