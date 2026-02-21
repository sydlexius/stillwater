// Command genloginbg generates a blurred login background image.
// It reads a source PNG, resizes it, applies a Gaussian-approximation blur,
// and saves it as a compressed JPEG.
//
// Usage: go run ./tools/genloginbg <source.png>
//
// Output: web/static/img/login-bg.jpg
package main

import (
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"

	"golang.org/x/image/draw"
)

const (
	targetWidth = 800
	blurRadius  = 18
	blurPasses  = 3
	jpegQuality = 80
	outputFile  = "web/static/img/login-bg.jpg"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: genloginbg <source.png>\n")
		os.Exit(1)
	}

	srcPath := os.Args[1]
	src, err := loadPNG(srcPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load source: %v\n", err)
		os.Exit(1)
	}

	// Resize to target width, preserving aspect ratio.
	bounds := src.Bounds()
	ratio := float64(targetWidth) / float64(bounds.Dx())
	targetHeight := int(float64(bounds.Dy()) * ratio)

	resized := image.NewNRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	draw.CatmullRom.Scale(resized, resized.Bounds(), src, bounds, draw.Over, nil)
	fmt.Printf("resized %dx%d -> %dx%d\n", bounds.Dx(), bounds.Dy(), targetWidth, targetHeight)

	// Apply multi-pass box blur (3 passes approximates Gaussian blur).
	blurred := resized
	for i := 0; i < blurPasses; i++ {
		blurred = boxBlur(blurred, blurRadius)
	}

	// Save as JPEG.
	if err := os.MkdirAll(filepath.Dir(outputFile), 0o750); err != nil {
		fmt.Fprintf(os.Stderr, "create dir: %v\n", err)
		os.Exit(1)
	}
	f, err := os.Create(outputFile) //nolint:gosec // G304: outputFile is a compile-time constant
	if err != nil {
		fmt.Fprintf(os.Stderr, "create output: %v\n", err)
		os.Exit(1)
	}
	if err := jpeg.Encode(f, blurred, &jpeg.Options{Quality: jpegQuality}); err != nil {
		_ = f.Close()
		fmt.Fprintf(os.Stderr, "encode jpeg: %v\n", err)
		os.Exit(1)
	}
	_ = f.Close()

	info, _ := os.Stat(outputFile)
	fmt.Printf("generated %s (%dx%d, %dKB)\n", outputFile, targetWidth, targetHeight, info.Size()/1024)
}

func loadPNG(path string) (image.Image, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is a CLI argument for this tool
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // best-effort close on read-only file
	return png.Decode(f)
}

// boxBlur applies a box blur with the given radius to an NRGBA image.
// Uses a two-pass (horizontal then vertical) sliding window for efficiency.
func boxBlur(src *image.NRGBA, radius int) *image.NRGBA {
	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	tmp := image.NewNRGBA(image.Rect(0, 0, w, h))
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))

	diameter := 2*radius + 1

	// Horizontal pass: src -> tmp
	for y := 0; y < h; y++ {
		var rSum, gSum, bSum, aSum int
		// Seed the window with the leftmost pixel repeated.
		for i := -radius; i <= radius; i++ {
			xi := clamp(i, w-1)
			c := src.NRGBAAt(xi, y)
			rSum += int(c.R)
			gSum += int(c.G)
			bSum += int(c.B)
			aSum += int(c.A)
		}
		for x := 0; x < w; x++ {
			tmp.Pix[(y*w+x)*4+0] = uint8(rSum / diameter) //nolint:gosec // G115: result is always 0-255
			tmp.Pix[(y*w+x)*4+1] = uint8(gSum / diameter) //nolint:gosec // G115: result is always 0-255
			tmp.Pix[(y*w+x)*4+2] = uint8(bSum / diameter) //nolint:gosec // G115: result is always 0-255
			tmp.Pix[(y*w+x)*4+3] = uint8(aSum / diameter)

			// Slide the window: remove left edge, add right edge.
			oldX := clamp(x-radius, w-1)
			newX := clamp(x+radius+1, w-1)
			oldC := src.NRGBAAt(oldX, y)
			newC := src.NRGBAAt(newX, y)
			rSum += int(newC.R) - int(oldC.R)
			gSum += int(newC.G) - int(oldC.G)
			bSum += int(newC.B) - int(oldC.B)
			aSum += int(newC.A) - int(oldC.A)
		}
	}

	// Vertical pass: tmp -> dst
	for x := 0; x < w; x++ {
		var rSum, gSum, bSum, aSum int
		for i := -radius; i <= radius; i++ {
			yi := clamp(i, h-1)
			c := tmp.NRGBAAt(x, yi)
			rSum += int(c.R)
			gSum += int(c.G)
			bSum += int(c.B)
			aSum += int(c.A)
		}
		for y := 0; y < h; y++ {
			dst.Pix[(y*w+x)*4+0] = uint8(rSum / diameter) //nolint:gosec // G115: result is always 0-255
			dst.Pix[(y*w+x)*4+1] = uint8(gSum / diameter) //nolint:gosec // G115: result is always 0-255
			dst.Pix[(y*w+x)*4+2] = uint8(bSum / diameter) //nolint:gosec // G115: result is always 0-255
			dst.Pix[(y*w+x)*4+3] = uint8(aSum / diameter)

			oldY := clamp(y-radius, h-1)
			newY := clamp(y+radius+1, h-1)
			oldC := tmp.NRGBAAt(x, oldY)
			newC := tmp.NRGBAAt(x, newY)
			rSum += int(newC.R) - int(oldC.R)
			gSum += int(newC.G) - int(oldC.G)
			bSum += int(newC.B) - int(oldC.B)
			aSum += int(newC.A) - int(oldC.A)
		}
	}

	return dst
}

func clamp(v, hi int) int {
	if v < 0 {
		return 0
	}
	if v > hi {
		return hi
	}
	return v
}
