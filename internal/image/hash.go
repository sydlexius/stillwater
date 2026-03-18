package image

import (
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math/bits"

	"golang.org/x/image/draw"
)

// PerceptualHash computes a 64-bit dHash (difference hash) from an image.
// The image is resized to 9x8 grayscale, then each pixel is compared to
// its right neighbor. If the left pixel is brighter, the corresponding
// bit is set. This produces a hash that is robust against scaling, minor
// color adjustments, and JPEG compression artifacts.
func PerceptualHash(r io.Reader) (uint64, error) {
	src, _, err := image.Decode(r)
	if err != nil {
		return 0, fmt.Errorf("decoding image for hash: %w", err)
	}

	return PerceptualHashFromImage(src), nil
}

// PerceptualHashFromImage computes a dHash from an already-decoded image.
func PerceptualHashFromImage(src image.Image) uint64 {
	// Resize to 9 wide x 8 tall (9 columns so we get 8 column diffs per row).
	resized := image.NewGray(image.Rect(0, 0, 9, 8))
	draw.CatmullRom.Scale(resized, resized.Bounds(), src, src.Bounds(), draw.Over, nil)

	var hash uint64
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			left := resized.GrayAt(x, y).Y
			right := resized.GrayAt(x+1, y).Y
			if left > right {
				hash |= 1 << uint(y*8+x)
			}
		}
	}
	return hash
}

// HammingDistance returns the number of differing bits between two hashes.
func HammingDistance(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}

// Similarity returns a similarity score between 0.0 and 1.0 for two hashes.
// 1.0 means identical, 0.0 means completely different.
func Similarity(a, b uint64) float64 {
	distance := HammingDistance(a, b)
	return 1.0 - float64(distance)/64.0
}

// HashHex formats a perceptual hash as a zero-padded 16-character hex string.
func HashHex(h uint64) string {
	return fmt.Sprintf("%016x", h)
}

// ParseHashHex parses a hex-encoded perceptual hash string.
func ParseHashHex(s string) (uint64, error) {
	var h uint64
	_, err := fmt.Sscanf(s, "%x", &h)
	return h, err
}

// GrayscaleLuminance converts an RGBA color to its grayscale luminance value.
func GrayscaleLuminance(c color.Color) uint8 {
	r, g, b, _ := c.RGBA()
	// Standard luminance formula (ITU-R BT.601)
	lum := (19595*r + 38470*g + 7471*b + 1<<15) >> 24
	return uint8(lum)
}
