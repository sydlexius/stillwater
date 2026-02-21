// Command genfavicon generates PNG favicon files from the Stillwater S-glyph.
// The glyph is rendered as white on a blue rounded-rect background.
// Run from the repository root: go run ./tools/genfavicon
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"

	"golang.org/x/image/vector"
)

const (
	viewboxSz = 32.0
	cornerR   = 6.0

	// Original SVG viewBox dimensions (after potrace transform).
	glyphVBW = 615.713299
	glyphVBH = 862.499485
)

var bgColor = color.NRGBA{37, 99, 235, 255} // #2563EB (blue-600)

// glyphCubics are the relative cubic bezier segments from the potrace S-glyph.
// Each entry is {dx1, dy1, dx2, dy2, dx, dy} relative to the current point.
// Start point: M3890,8880 in potrace coordinate space.
var glyphCubics = [][6]float64{
	{-699, -53, -1231, -226, -1644, -536},
	{-476, -358, -730, -892, -703, -1479},
	{11, -243, 57, -424, 157, -622},
	{182, -362, 529, -642, 1104, -892},
	{163, -71, 321, -131, 671, -256},
	{606, -215, 703, -252, 925, -355},
	{638, -293, 933, -628, 991, -1123},
	{50, -427, -116, -865, -424, -1125},
	{-233, -196, -469, -297, -777, -333},
	{-315, -36, -623, 47, -798, 214},
	{-129, 123, -202, 312, -189, 486},
	{13, 172, 81, 256, 219, 268},
	{113, 11, 178, -31, 288, -187},
	{109, -153, 179, -204, 327, -235},
	{272, -58, 577, 53, 738, 267},
	{202, 269, 247, 655, 115, 981},
	{-56, 136, -110, 217, -220, 328},
	{-116, 117, -186, 166, -335, 239},
	{-477, 230, -1256, 253, -1855, 53},
	{-641, -213, -1090, -663, -1281, -1281},
	{-143, -466, -127, -1040, 42, -1492},
	{245, -652, 783, -1134, 1534, -1373},
	{734, -234, 1632, -220, 2406, 36},
	{702, 233, 1258, 643, 1631, 1202},
	{96, 144, 231, 416, 287, 578},
	{145, 420, 194, 910, 136, 1355},
	{-20, 151, -80, 393, -130, 527},
	{-239, 640, -750, 1130, -1587, 1520},
	{-224, 105, -418, 184, -904, 365},
	{-782, 293, -969, 384, -1139, 555},
	{-182, 183, -193, 411, -28, 595},
	{134, 149, 374, 242, 663, 256},
	{447, 23, 877, -142, 998, -382},
	{77, -153, 67, -286, -39, -517},
	{-80, -174, -94, -281, -49, -383},
	{41, -95, 161, -169, 336, -206},
	{135, -30, 454, -32, 587, -5},
	{473, 96, 804, 394, 912, 821},
	{64, 253, 56, 551, -21, 798},
	{-208, 665, -875, 1130, -1847, 1288},
	{-69, 11, -190, 27, -269, 36},
	{-170, 18, -666, 26, -828, 14},
}

func main() {
	outDir := filepath.Join("web", "static", "img")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "create dir: %v\n", err)
		os.Exit(1)
	}

	targets := []struct {
		name string
		size int
	}{
		{"favicon-16x16.png", 16},
		{"favicon-32x32.png", 32},
		{"apple-touch-icon.png", 180},
		{"android-chrome-192x192.png", 192},
		{"android-chrome-512x512.png", 512},
	}

	for _, t := range targets {
		img := renderIcon(t.size)
		p := filepath.Join(outDir, t.name)
		f, err := os.Create(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "create %s: %v\n", p, err)
			os.Exit(1)
		}
		if err := png.Encode(f, img); err != nil {
			f.Close()
			fmt.Fprintf(os.Stderr, "encode %s: %v\n", p, err)
			os.Exit(1)
		}
		f.Close()
		fmt.Printf("generated %s (%dx%d)\n", p, t.size, t.size)
	}
}

func renderIcon(size int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	s := float64(size) / viewboxSz

	// Rounded rectangle background.
	half := float64(size) / 2.0
	cr := cornerR * s
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			d := roundedBoxSDF(float64(x)+0.5-half, float64(y)+0.5-half, half, half, cr)
			if d <= -0.5 {
				img.SetNRGBA(x, y, bgColor)
			} else if d < 0.5 {
				blend(img, x, y, bgColor, 0.5-d)
			}
		}
	}

	// Rasterize the S glyph on top.
	rasterizeGlyph(img, size)
	return img
}

// rasterizeGlyph draws the S-glyph path as white onto img using vector rasterization.
func rasterizeGlyph(img *image.NRGBA, size int) {
	// Compute transform from potrace path coords to pixel coords.
	// The potrace transform maps path coords to a glyphVBW x glyphVBH viewBox:
	//   vx = 0.1*px - 110.176503
	//   vy = -0.1*py + 888.564766
	// Then we scale the viewBox to fit in the icon with padding.
	padding := float64(size) * 3.0 / 32.0
	avail := float64(size) - 2*padding
	scale := math.Min(avail/glyphVBW, avail/glyphVBH) // height-limited
	glyphW := glyphVBW * scale
	glyphH := glyphVBH * scale
	offsetX := (float64(size) - glyphW) / 2
	offsetY := (float64(size) - glyphH) / 2

	// Combined: fx = (0.1*px - 110.176503)*scale + offsetX
	//           fy = (-0.1*py + 888.564766)*scale + offsetY
	tx := func(px, py float64) (float32, float32) {
		vx := 0.1*px - 110.176503
		vy := -0.1*py + 888.564766
		return float32(vx*scale + offsetX), float32(vy*scale + offsetY)
	}

	var r vector.Rasterizer
	r.Reset(size, size)

	// M3890,8880
	curX, curY := 3890.0, 8880.0
	mx, my := tx(curX, curY)
	r.MoveTo(mx, my)

	// Relative cubic bezier segments.
	for _, c := range glyphCubics {
		x1, y1 := curX+c[0], curY+c[1]
		x2, y2 := curX+c[2], curY+c[3]
		ex, ey := curX+c[4], curY+c[5]

		px1, py1 := tx(x1, y1)
		px2, py2 := tx(x2, y2)
		pex, pey := tx(ex, ey)

		r.CubeTo(px1, py1, px2, py2, pex, pey)
		curX, curY = ex, ey
	}

	r.ClosePath()

	// Draw glyph as white through the rasterized mask.
	r.Draw(img, img.Bounds(), image.White, image.Point{})
}

// roundedBoxSDF returns the signed distance from (px, py) to a rounded rect
// centered at the origin. Negative = inside, positive = outside.
func roundedBoxSDF(px, py, bx, by, r float64) float64 {
	qx := math.Abs(px) - bx + r
	qy := math.Abs(py) - by + r
	return math.Sqrt(math.Max(qx, 0)*math.Max(qx, 0)+math.Max(qy, 0)*math.Max(qy, 0)) +
		math.Min(math.Max(qx, qy), 0) - r
}

// blend alpha-composites color c at the given alpha over the existing pixel.
func blend(img *image.NRGBA, x, y int, c color.NRGBA, alpha float64) {
	if alpha <= 0 {
		return
	}
	alpha = math.Min(alpha, 1)

	dst := img.NRGBAAt(x, y)
	sa := float64(c.A) / 255.0 * alpha
	da := float64(dst.A) / 255.0
	oa := sa + da*(1-sa)
	if oa == 0 {
		return
	}

	img.SetNRGBA(x, y, color.NRGBA{
		R: uint8(math.Round((float64(c.R)*sa + float64(dst.R)*da*(1-sa)) / oa)),
		G: uint8(math.Round((float64(c.G)*sa + float64(dst.G)*da*(1-sa)) / oa)),
		B: uint8(math.Round((float64(c.B)*sa + float64(dst.B)*da*(1-sa)) / oa)),
		A: uint8(math.Round(oa * 255)),
	})
}
