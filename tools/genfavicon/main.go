// Command genfavicon generates PNG favicon files from the master favicon SVG.
//
// The SVG at web/static/img/favicon.svg is the single source of truth for the
// app icon (an Oleo Script "S" glyph on a superellipse squircle). This tool
// rasterizes that SVG at each required size using a pure-Go SVG rasterizer
// (oksvg + rasterx, no CGO), so the committed PNG files never drift from the SVG.
//
// Run from the repository root: go run ./tools/genfavicon
package main

import (
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"

	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
)

// svgName is the master SVG filename, resolved inside the output directory.
const svgName = "favicon.svg"

// encodePNG encodes an image to w. It is a package var (not a direct png.Encode
// call) so tests can exercise the encode-failure path, which png.Encode itself
// does not reach for valid images.
var encodePNG func(w io.Writer, img image.Image) error = png.Encode

// target describes one PNG to emit and its square pixel size.
type target struct {
	name string
	size int
}

// targets lists every PNG rasterized from the master SVG.
var targets = []target{
	{"favicon-16x16.png", 16},
	{"favicon-32x32.png", 32},
	{"apple-touch-icon.png", 180},
	{"android-chrome-192x192.png", 192},
	{"android-chrome-512x512.png", 512},
}

func main() {
	if err := run(filepath.Join("web", "static", "img")); err != nil {
		fmt.Fprintf(os.Stderr, "genfavicon: %v\n", err)
		os.Exit(1)
	}
}

// run rasterizes the master SVG in dir into every target PNG in dir.
func run(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}

	svgPath := filepath.Join(dir, svgName)
	icon, err := oksvg.ReadIcon(svgPath, oksvg.WarnErrorMode)
	if err != nil {
		return fmt.Errorf("read %s: %w", svgPath, err)
	}

	for _, t := range targets {
		p := filepath.Join(dir, t.name)
		if err := writePNG(renderIcon(icon, t.size), p); err != nil {
			return err
		}
		fmt.Printf("generated %s (%dx%d)\n", p, t.size, t.size)
	}
	return nil
}

// renderIcon rasterizes icon into a square RGBA image of the given size.
func renderIcon(icon *oksvg.SvgIcon, size int) *image.RGBA {
	// Scale the icon's viewBox to fill the target size exactly.
	icon.SetTarget(0, 0, float64(size), float64(size))

	img := image.NewRGBA(image.Rect(0, 0, size, size))
	scanner := rasterx.NewScannerGV(size, size, img, img.Bounds())
	raster := rasterx.NewDasher(size, size, scanner)
	icon.Draw(raster, 1.0)
	return img
}

// writePNG encodes img as a PNG at path.
func writePNG(img image.Image, path string) error {
	f, err := os.Create(path) //nolint:gosec // G304: path is constructed from a hardcoded directory + filename
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if err := encodePNG(f, img); err != nil {
		_ = f.Close()
		return fmt.Errorf("encode %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}
