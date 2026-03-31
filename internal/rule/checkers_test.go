package rule

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

func TestCheckNFOExists(t *testing.T) {
	tests := []struct {
		name    string
		artist  artist.Artist
		wantNil bool
	}{
		{
			name:    "has NFO",
			artist:  artist.Artist{Name: "Test", NFOExists: true},
			wantNil: true,
		},
		{
			name:    "no NFO",
			artist:  artist.Artist{Name: "Test", NFOExists: false},
			wantNil: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := checkNFOExists(&tt.artist, RuleConfig{})
			if (v == nil) != tt.wantNil {
				t.Errorf("checkNFOExists = %v, wantNil = %v", v, tt.wantNil)
			}
			if v != nil {
				if v.RuleID != RuleNFOExists {
					t.Errorf("RuleID = %q, want %q", v.RuleID, RuleNFOExists)
				}
				if !v.Fixable {
					t.Error("expected Fixable to be true")
				}
			}
		})
	}
}

func TestCheckNFOHasMBID(t *testing.T) {
	tests := []struct {
		name    string
		artist  artist.Artist
		wantNil bool
	}{
		{
			name:    "has MBID",
			artist:  artist.Artist{Name: "Test", MusicBrainzID: "abc-123"},
			wantNil: true,
		},
		{
			name:    "no MBID",
			artist:  artist.Artist{Name: "Test", MusicBrainzID: ""},
			wantNil: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := checkNFOHasMBID(&tt.artist, RuleConfig{})
			if (v == nil) != tt.wantNil {
				t.Errorf("checkNFOHasMBID = %v, wantNil = %v", v, tt.wantNil)
			}
		})
	}
}

func TestCheckThumbExists(t *testing.T) {
	a := artist.Artist{Name: "Test", ThumbExists: true}
	if v := checkThumbExists(&a, RuleConfig{}); v != nil {
		t.Errorf("expected nil for artist with thumb, got %v", v)
	}

	a.ThumbExists = false
	if v := checkThumbExists(&a, RuleConfig{}); v == nil {
		t.Error("expected violation for artist without thumb")
	}
}

func TestCheckFanartExists(t *testing.T) {
	a := artist.Artist{Name: "Test", FanartExists: true}
	if v := checkFanartExists(&a, RuleConfig{}); v != nil {
		t.Errorf("expected nil for artist with fanart, got %v", v)
	}

	a.FanartExists = false
	v := checkFanartExists(&a, RuleConfig{})
	if v == nil {
		t.Fatal("expected violation for artist without fanart")
	}
	if v.Severity != "warning" {
		t.Errorf("Severity = %q, want %q", v.Severity, "warning")
	}
}

func TestCheckLogoExists(t *testing.T) {
	a := artist.Artist{Name: "Test", LogoExists: true}
	if v := checkLogoExists(&a, RuleConfig{}); v != nil {
		t.Errorf("expected nil for artist with logo, got %v", v)
	}

	a.LogoExists = false
	v := checkLogoExists(&a, RuleConfig{})
	if v == nil {
		t.Fatal("expected violation for artist without logo")
	}
	if v.Severity != "info" {
		t.Errorf("Severity = %q, want %q", v.Severity, "info")
	}
}

func TestCheckBioExists(t *testing.T) {
	tests := []struct {
		name    string
		bio     string
		minLen  int
		wantNil bool
	}{
		{
			name:    "has biography",
			bio:     "A lengthy biography about this artist.",
			minLen:  10,
			wantNil: true,
		},
		{
			name:    "empty biography",
			bio:     "",
			minLen:  10,
			wantNil: false,
		},
		{
			name:    "too short biography",
			bio:     "Short",
			minLen:  10,
			wantNil: false,
		},
		{
			name:    "default min length",
			bio:     "Exactly 10",
			minLen:  0, // should default to 10
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := artist.Artist{Name: "Test", Biography: tt.bio}
			cfg := RuleConfig{MinLength: tt.minLen}
			v := checkBioExists(&a, cfg)
			if (v == nil) != tt.wantNil {
				t.Errorf("checkBioExists = %v, wantNil = %v", v, tt.wantNil)
			}
		})
	}
}

func TestCheckThumbSquare(t *testing.T) {
	e := &Engine{logger: slog.Default()}
	checker := e.makeThumbSquareChecker()

	// Create a temp dir with a square image
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "folder.jpg"), 500, 500)

	a := artist.Artist{Name: "Test", ThumbExists: true, Path: dir}
	v := checker(&a, RuleConfig{AspectRatio: 1.0, Tolerance: 0.1})
	if v != nil {
		t.Errorf("expected nil for square thumbnail, got %v", v)
	}

	// Create a non-square image
	dir2 := t.TempDir()
	createTestJPEG(t, filepath.Join(dir2, "folder.jpg"), 800, 400)

	a2 := artist.Artist{Name: "Test2", ThumbExists: true, Path: dir2}
	v2 := checker(&a2, RuleConfig{AspectRatio: 1.0, Tolerance: 0.1})
	if v2 == nil {
		t.Error("expected violation for non-square thumbnail")
	}
}

func TestCheckThumbSquare_NoThumb(t *testing.T) {
	e := &Engine{logger: slog.Default()}
	checker := e.makeThumbSquareChecker()

	// When thumb does not exist, checker should return nil (thumb_exists handles it)
	a := artist.Artist{Name: "Test", ThumbExists: false}
	v := checker(&a, RuleConfig{AspectRatio: 1.0, Tolerance: 0.1})
	if v != nil {
		t.Errorf("expected nil when ThumbExists is false, got %v", v)
	}
}

func TestCheckThumbMinRes(t *testing.T) {
	e := &Engine{logger: slog.Default()}
	checker := e.makeThumbMinResChecker()

	// Create a high-res image
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "folder.jpg"), 1000, 1000)

	a := artist.Artist{Name: "Test", ThumbExists: true, Path: dir}
	v := checker(&a, RuleConfig{MinWidth: 500, MinHeight: 500})
	if v != nil {
		t.Errorf("expected nil for high-res thumbnail, got %v", v)
	}

	// Create a low-res image
	dir2 := t.TempDir()
	createTestJPEG(t, filepath.Join(dir2, "folder.jpg"), 200, 200)

	a2 := artist.Artist{Name: "Test2", ThumbExists: true, Path: dir2}
	v2 := checker(&a2, RuleConfig{MinWidth: 500, MinHeight: 500})
	if v2 == nil {
		t.Error("expected violation for low-res thumbnail")
	}
}

func TestCheckThumbMinRes_DefaultValues(t *testing.T) {
	e := &Engine{logger: slog.Default()}
	checker := e.makeThumbMinResChecker()

	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "folder.jpg"), 600, 600)

	a := artist.Artist{Name: "Test", ThumbExists: true, Path: dir}
	// Zero config should default to 500x500
	v := checker(&a, RuleConfig{})
	if v != nil {
		t.Errorf("expected nil for 600x600 with default min 500, got %v", v)
	}
}

// createTestJPEG creates a solid-color JPEG test image at the given path.
func createTestJPEG(t *testing.T, path string, width, height int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: 100, G: 150, B: 200, A: 255})
		}
	}

	f, err := os.Create(path) //nolint:gosec
	if err != nil {
		t.Fatalf("creating test image: %v", err)
	}
	defer f.Close() //nolint:errcheck

	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatalf("encoding jpeg: %v", err)
	}
}

func TestCheckFanartMinRes(t *testing.T) {
	e := &Engine{logger: slog.Default()}
	checker := e.makeFanartMinResChecker()

	// Missing fanart: skip check
	a := artist.Artist{Name: "Test", FanartExists: false}
	if v := checker(&a, RuleConfig{}); v != nil {
		t.Errorf("expected nil when FanartExists is false, got %v", v)
	}

	// High-res fanart: pass
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)
	a = artist.Artist{Name: "Test", FanartExists: true, Path: dir}
	if v := checker(&a, RuleConfig{MinWidth: 1920, MinHeight: 1080}); v != nil {
		t.Errorf("expected nil for 1920x1080 fanart, got %v", v)
	}

	// Low-res fanart: violation
	dir2 := t.TempDir()
	createTestJPEG(t, filepath.Join(dir2, "fanart.jpg"), 800, 450)
	a2 := artist.Artist{Name: "Test2", FanartExists: true, Path: dir2}
	v := checker(&a2, RuleConfig{MinWidth: 1920, MinHeight: 1080})
	if v == nil {
		t.Error("expected violation for low-res fanart")
	}
	if v != nil && v.RuleID != RuleFanartMinRes {
		t.Errorf("RuleID = %q, want %q", v.RuleID, RuleFanartMinRes)
	}

	// Zero config uses defaults
	dir3 := t.TempDir()
	createTestJPEG(t, filepath.Join(dir3, "fanart.jpg"), 2000, 1200)
	a3 := artist.Artist{Name: "Test3", FanartExists: true, Path: dir3}
	if v := checker(&a3, RuleConfig{}); v != nil {
		t.Errorf("expected nil for 2000x1200 with default 1920x1080, got %v", v)
	}
}

func TestCheckFanartAspect(t *testing.T) {
	e := &Engine{logger: slog.Default()}
	checker := e.makeFanartAspectChecker()

	// Missing fanart: skip
	a := artist.Artist{Name: "Test", FanartExists: false}
	if v := checker(&a, RuleConfig{}); v != nil {
		t.Errorf("expected nil when FanartExists is false, got %v", v)
	}

	// Correct 16:9 fanart: pass
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)
	a = artist.Artist{Name: "Test", FanartExists: true, Path: dir}
	if v := checker(&a, RuleConfig{AspectRatio: 16.0 / 9.0, Tolerance: 0.1}); v != nil {
		t.Errorf("expected nil for 16:9 fanart, got %v", v)
	}

	// Square fanart: violation
	dir2 := t.TempDir()
	createTestJPEG(t, filepath.Join(dir2, "fanart.jpg"), 1000, 1000)
	a2 := artist.Artist{Name: "Test2", FanartExists: true, Path: dir2}
	v := checker(&a2, RuleConfig{AspectRatio: 16.0 / 9.0, Tolerance: 0.1})
	if v == nil {
		t.Error("expected violation for square fanart with 16:9 check")
	}
	if v != nil && v.RuleID != RuleFanartAspect {
		t.Errorf("RuleID = %q, want %q", v.RuleID, RuleFanartAspect)
	}
}

func TestCheckLogoMinRes(t *testing.T) {
	e := &Engine{logger: slog.Default()}
	checker := e.makeLogoMinResChecker()

	// Missing logo: skip
	a := artist.Artist{Name: "Test", LogoExists: false}
	if v := checker(&a, RuleConfig{}); v != nil {
		t.Errorf("expected nil when LogoExists is false, got %v", v)
	}

	// Wide-enough logo: pass
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "logo.png"), 500, 200)
	a = artist.Artist{Name: "Test", LogoExists: true, Path: dir}
	if v := checker(&a, RuleConfig{MinWidth: 400}); v != nil {
		t.Errorf("expected nil for 500px logo, got %v", v)
	}

	// Narrow logo: violation
	dir2 := t.TempDir()
	createTestJPEG(t, filepath.Join(dir2, "logo.png"), 200, 100)
	a2 := artist.Artist{Name: "Test2", LogoExists: true, Path: dir2}
	v := checker(&a2, RuleConfig{MinWidth: 400})
	if v == nil {
		t.Error("expected violation for 200px logo with 400px minimum")
	}
	if v != nil && v.RuleID != RuleLogoMinRes {
		t.Errorf("RuleID = %q, want %q", v.RuleID, RuleLogoMinRes)
	}

	// Zero config defaults to 400px
	dir3 := t.TempDir()
	createTestJPEG(t, filepath.Join(dir3, "logo.png"), 500, 200)
	a3 := artist.Artist{Name: "Test3", LogoExists: true, Path: dir3}
	if v := checker(&a3, RuleConfig{}); v != nil {
		t.Errorf("expected nil for 500px with default 400px minimum, got %v", v)
	}
}

func TestCheckBannerExists(t *testing.T) {
	a := artist.Artist{Name: "Test", BannerExists: true}
	if v := checkBannerExists(&a, RuleConfig{}); v != nil {
		t.Errorf("expected nil for artist with banner, got %v", v)
	}

	a.BannerExists = false
	v := checkBannerExists(&a, RuleConfig{})
	if v == nil {
		t.Fatal("expected violation for artist without banner")
	}
	if v.RuleID != RuleBannerExists {
		t.Errorf("RuleID = %q, want %q", v.RuleID, RuleBannerExists)
	}
	if v.Severity != "info" {
		t.Errorf("Severity = %q, want info", v.Severity)
	}
}

func TestCheckBannerMinRes(t *testing.T) {
	e := &Engine{logger: slog.Default()}
	checker := e.makeBannerMinResChecker()

	// Missing banner: skip
	a := artist.Artist{Name: "Test", BannerExists: false}
	if v := checker(&a, RuleConfig{}); v != nil {
		t.Errorf("expected nil when BannerExists is false, got %v", v)
	}

	// Large-enough banner: pass
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "banner.jpg"), 1000, 185)
	a = artist.Artist{Name: "Test", BannerExists: true, Path: dir}
	if v := checker(&a, RuleConfig{MinWidth: 1000, MinHeight: 185}); v != nil {
		t.Errorf("expected nil for 1000x185 banner, got %v", v)
	}

	// Small banner: violation
	dir2 := t.TempDir()
	createTestJPEG(t, filepath.Join(dir2, "banner.jpg"), 500, 100)
	a2 := artist.Artist{Name: "Test2", BannerExists: true, Path: dir2}
	v := checker(&a2, RuleConfig{MinWidth: 1000, MinHeight: 185})
	if v == nil {
		t.Error("expected violation for small banner")
	}
	if v != nil && v.RuleID != RuleBannerMinRes {
		t.Errorf("RuleID = %q, want %q", v.RuleID, RuleBannerMinRes)
	}

	// Zero config uses defaults
	dir3 := t.TempDir()
	createTestJPEG(t, filepath.Join(dir3, "banner.jpg"), 1200, 200)
	a3 := artist.Artist{Name: "Test3", BannerExists: true, Path: dir3}
	if v := checker(&a3, RuleConfig{}); v != nil {
		t.Errorf("expected nil for 1200x200 with default 1000x185, got %v", v)
	}
}

func TestCheckExtraneousImages(t *testing.T) {
	// Create a temp dir with canonical + extraneous files.
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "folder.jpg"), 500, 500)   // canonical thumb
	createTestJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080) // canonical fanart
	createTestJPEG(t, filepath.Join(dir, "random.jpg"), 500, 500)   // extraneous
	createTestJPEG(t, filepath.Join(dir, "cover.png"), 500, 500)    // extraneous

	// Engine with nil platformService uses DefaultFileNames (full arrays).
	e := &Engine{platformService: nil}
	checker := e.makeExtraneousImagesChecker()

	a := artist.Artist{Name: "Test", Path: dir}
	v := checker(&a, RuleConfig{Severity: "warning"})
	if v == nil {
		t.Fatal("expected violation for extraneous images")
	}
	if v.RuleID != RuleExtraneousImages {
		t.Errorf("RuleID = %q, want %q", v.RuleID, RuleExtraneousImages)
	}
	if !v.Fixable {
		t.Error("expected Fixable to be true")
	}
}

func TestCheckExtraneousImages_NoExtraneous(t *testing.T) {
	// Only canonical files present.
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "folder.jpg"), 500, 500)
	createTestJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)

	e := &Engine{platformService: nil}
	checker := e.makeExtraneousImagesChecker()

	a := artist.Artist{Name: "Test", Path: dir}
	v := checker(&a, RuleConfig{})
	if v != nil {
		t.Errorf("expected nil for directory with only canonical files, got %v", v)
	}
}

func TestCheckExtraneousImages_NumberedFanart(t *testing.T) {
	// Numbered fanart files (fanart.jpg, fanart1.jpg, fanart2.jpg) should be
	// whitelisted and not flagged as extraneous.
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "folder.jpg"), 500, 500)
	createTestJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)
	createTestJPEG(t, filepath.Join(dir, "fanart1.jpg"), 1920, 1080)
	createTestJPEG(t, filepath.Join(dir, "fanart2.jpg"), 1920, 1080)

	e := &Engine{platformService: nil}
	checker := e.makeExtraneousImagesChecker()

	a := artist.Artist{Name: "Test", Path: dir}
	v := checker(&a, RuleConfig{})
	if v != nil {
		t.Errorf("expected nil (numbered fanart should be whitelisted), got: %s", v.Message)
	}
}

func TestCheckExtraneousImages_NumberedFanartWithGaps(t *testing.T) {
	// Numbered fanart with gaps (fanart.jpg + fanart3.jpg, missing 1 and 2)
	// should still be whitelisted. Gap detection is the backdrop_sequencing
	// rule's responsibility, not extraneous_images.
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "folder.jpg"), 500, 500)
	createTestJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)
	createTestJPEG(t, filepath.Join(dir, "fanart3.jpg"), 1920, 1080)

	e := &Engine{platformService: nil}
	checker := e.makeExtraneousImagesChecker()

	a := artist.Artist{Name: "Test", Path: dir}
	v := checker(&a, RuleConfig{})
	if v != nil {
		t.Errorf("expected nil (numbered fanart with gaps should be whitelisted), got: %s", v.Message)
	}
}

func TestCheckExtraneousImages_BackdropNaming(t *testing.T) {
	// Backdrop naming convention (used by Emby/Jellyfin) should also be
	// whitelisted for numbered variants.
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "folder.jpg"), 500, 500)
	createTestJPEG(t, filepath.Join(dir, "backdrop.jpg"), 1920, 1080)
	createTestJPEG(t, filepath.Join(dir, "backdrop2.jpg"), 1920, 1080)
	createTestJPEG(t, filepath.Join(dir, "backdrop3.jpg"), 1920, 1080)

	e := &Engine{platformService: nil}
	checker := e.makeExtraneousImagesChecker()

	a := artist.Artist{Name: "Test", Path: dir}
	v := checker(&a, RuleConfig{})
	if v != nil {
		t.Errorf("expected nil (backdrop numbered variants should be whitelisted), got: %s", v.Message)
	}
}

func TestCheckExtraneousImages_NonStandardNameFlagged(t *testing.T) {
	// Non-standard names like "backdrop_old.jpg" should be flagged even
	// when valid numbered backdrops are present.
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "folder.jpg"), 500, 500)
	createTestJPEG(t, filepath.Join(dir, "backdrop.jpg"), 1920, 1080)
	createTestJPEG(t, filepath.Join(dir, "backdrop_old.jpg"), 1920, 1080)

	e := &Engine{platformService: nil}
	checker := e.makeExtraneousImagesChecker()

	a := artist.Artist{Name: "Test", Path: dir}
	v := checker(&a, RuleConfig{})
	if v == nil {
		t.Fatal("expected violation for non-standard 'backdrop_old.jpg'")
	}
	if !strings.Contains(v.Message, "backdrop_old.jpg") {
		t.Errorf("expected message to mention backdrop_old.jpg, got: %s", v.Message)
	}
}

func TestCheckExtraneousImages_EmptyPath(t *testing.T) {
	e := &Engine{platformService: nil}
	checker := e.makeExtraneousImagesChecker()

	a := artist.Artist{Name: "Test", Path: ""}
	v := checker(&a, RuleConfig{})
	if v != nil {
		t.Errorf("expected nil for empty path, got %v", v)
	}
}

func TestGetImageDimensionsCached_NoMatch(t *testing.T) {
	dir := t.TempDir()
	// Use an Engine with no FSCache (nil) to exercise the fallback path.
	engine := &Engine{logger: slog.Default()}
	_, _, err := engine.getImageDimensionsCached(dir, []string{"fanart.jpg", "fanart.png"})
	if err == nil {
		t.Error("expected error when no matching images exist")
	}
}

func TestEffectiveSeverity(t *testing.T) {
	if s := effectiveSeverity(RuleConfig{Severity: "error"}); s != "error" {
		t.Errorf("expected error, got %q", s)
	}
	if s := effectiveSeverity(RuleConfig{}); s != "warning" {
		t.Errorf("expected warning, got %q", s)
	}
}

func TestNormalizeArtistName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"The Beatles", "beatles"},
		{"the beatles", "beatles"},
		{"AC/DC", "acdc"},
		{"Guns N' Roses", "guns n roses"},
		{"Beyonce (Deluxe)", "beyonce"},
		{"  Tool  ", "tool"},
		{"Motley Crue", "motley crue"},
		{"", ""},
		{"The The", "the"},
		{"A Perfect Circle (Live)", "a perfect circle"},
		{"Beatles, The", "beatles"},
		{"Beatles, The (Live)", "beatles"},
		{"Smashing Pumpkins, The", "smashing pumpkins"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := normalizeArtistName(tt.input)
			if got != tt.want {
				t.Errorf("normalizeArtistName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestLevenshteinDistance(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "", 3},
		{"", "abc", 3},
		{"kitten", "sitting", 3},
		{"saturday", "sunday", 3},
		{"abc", "abc", 0},
	}
	for _, tt := range tests {
		t.Run(tt.a+"_"+tt.b, func(t *testing.T) {
			got := levenshteinDistance(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("levenshteinDistance(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestNameSimilarity(t *testing.T) {
	tests := []struct {
		a, b    string
		wantMin float64
		wantMax float64
	}{
		{"abc", "abc", 1.0, 1.0},
		{"", "", 1.0, 1.0},
		{"abc", "xyz", 0.0, 0.01},
		{"kitten", "sitting", 0.5, 0.6},
	}
	for _, tt := range tests {
		t.Run(tt.a+"_"+tt.b, func(t *testing.T) {
			got := nameSimilarity(tt.a, tt.b)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("nameSimilarity(%q, %q) = %.4f, want [%.2f, %.2f]", tt.a, tt.b, got, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestCheckArtistIDMismatch(t *testing.T) {
	tests := []struct {
		name    string
		artist  artist.Artist
		cfg     RuleConfig
		wantNil bool
	}{
		{
			name:    "empty path skips check",
			artist:  artist.Artist{Name: "Nirvana", Path: ""},
			cfg:     RuleConfig{Tolerance: 0.8},
			wantNil: true,
		},
		{
			name:    "exact match",
			artist:  artist.Artist{Name: "Nirvana", Path: filepath.Join(t.TempDir(), "Nirvana")},
			cfg:     RuleConfig{Tolerance: 0.8},
			wantNil: true,
		},
		{
			name:    "case difference only",
			artist:  artist.Artist{Name: "Nirvana", Path: filepath.Join(t.TempDir(), "nirvana")},
			cfg:     RuleConfig{Tolerance: 0.8},
			wantNil: true,
		},
		{
			name:    "The prefix difference",
			artist:  artist.Artist{Name: "The Beatles", Path: filepath.Join(t.TempDir(), "Beatles")},
			cfg:     RuleConfig{Tolerance: 0.8},
			wantNil: true,
		},
		{
			name:    "completely different names",
			artist:  artist.Artist{Name: "Nirvana", Path: filepath.Join(t.TempDir(), "Metallica")},
			cfg:     RuleConfig{Tolerance: 0.8},
			wantNil: false,
		},
		{
			name:    "similar names above threshold",
			artist:  artist.Artist{Name: "Radiohead", Path: filepath.Join(t.TempDir(), "Radio Head")},
			cfg:     RuleConfig{Tolerance: 0.8},
			wantNil: true,
		},
		{
			name:    "default tolerance when zero",
			artist:  artist.Artist{Name: "Nirvana", Path: filepath.Join(t.TempDir(), "Metallica")},
			cfg:     RuleConfig{},
			wantNil: false,
		},
		{
			name:    "sort name folder matches via SortName field",
			artist:  artist.Artist{Name: "The Beatles", SortName: "Beatles, The", Path: filepath.Join(t.TempDir(), "Beatles, The")},
			cfg:     RuleConfig{Tolerance: 0.8},
			wantNil: true,
		},
		{
			name:    "inverted sort name folder matches via normalizer",
			artist:  artist.Artist{Name: "The Beatles", Path: filepath.Join(t.TempDir(), "Beatles, The")},
			cfg:     RuleConfig{Tolerance: 0.8},
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := checkArtistIDMismatch(&tt.artist, tt.cfg)
			if (v == nil) != tt.wantNil {
				t.Errorf("checkArtistIDMismatch = %v, wantNil = %v", v, tt.wantNil)
			}
			if v != nil {
				if v.Fixable {
					t.Error("expected Fixable to be false for artist ID mismatch")
				}
				if v.RuleID != RuleArtistIDMismatch {
					t.Errorf("RuleID = %q, want %q", v.RuleID, RuleArtistIDMismatch)
				}
			}
		})
	}
}

// createTestPNGWithPadding creates a PNG file with opaque content in the center
// and transparent padding around the edges.
func createTestPNGWithPadding(t *testing.T, path string, totalW, totalH, padLeft, padRight, padTop, padBottom int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, totalW, totalH))
	for y := padTop; y < totalH-padBottom; y++ {
		for x := padLeft; x < totalW-padRight; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}

	f, err := os.Create(path) //nolint:gosec
	if err != nil {
		t.Fatalf("creating test png: %v", err)
	}
	defer f.Close() //nolint:errcheck

	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encoding png: %v", err)
	}
}

// createTestPNG creates a fully opaque PNG file (no transparent padding).
func createTestPNG(t *testing.T, path string, w, h int) {
	t.Helper()
	createTestPNGWithPadding(t, path, w, h, 0, 0, 0, 0)
}

// newTestEngine returns a minimal Engine suitable for unit-testing individual
// checkers. Uses the default logger for debug output during test runs.
func newTestEngine() *Engine {
	return &Engine{logger: slog.Default()}
}

func TestCheckBackdropSequencing_Contiguous(t *testing.T) {
	// Non-Kodi convention: index 0 = fanart.jpg, index 1 = fanart2.jpg,
	// index 2 = fanart3.jpg (suffix = index + 1).
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)
	createTestJPEG(t, filepath.Join(dir, "fanart2.jpg"), 1920, 1080)
	createTestJPEG(t, filepath.Join(dir, "fanart3.jpg"), 1920, 1080)

	e := &Engine{platformService: nil}
	checker := e.makeBackdropSequencingChecker()

	a := artist.Artist{Name: "Test", Path: dir}
	v := checker(&a, RuleConfig{})
	if v != nil {
		t.Errorf("expected nil for contiguous sequence, got: %s", v.Message)
	}
}

func TestCheckBackdropSequencing_WithGap(t *testing.T) {
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)
	createTestJPEG(t, filepath.Join(dir, "fanart3.jpg"), 1920, 1080)
	// Gap at indices 1 and 2

	e := &Engine{platformService: nil}
	checker := e.makeBackdropSequencingChecker()

	a := artist.Artist{Name: "Test", Path: dir}
	v := checker(&a, RuleConfig{})
	if v == nil {
		t.Fatal("expected violation for gap in sequence")
	}
	if v.RuleID != RuleBackdropSequencing {
		t.Errorf("RuleID = %q, want %q", v.RuleID, RuleBackdropSequencing)
	}
	if !v.Fixable {
		t.Error("expected Fixable to be true")
	}
}

func TestCheckBackdropSequencing_NumberedOnlyNoPrimary(t *testing.T) {
	// Only numbered variants exist without the primary (fanart.jpg missing).
	// fanart2.jpg + fanart3.jpg should trigger a violation since index 0 is absent.
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "fanart2.jpg"), 1920, 1080)
	createTestJPEG(t, filepath.Join(dir, "fanart3.jpg"), 1920, 1080)

	e := &Engine{platformService: nil}
	checker := e.makeBackdropSequencingChecker()

	a := artist.Artist{Name: "Test", Path: dir}
	v := checker(&a, RuleConfig{})
	if v == nil {
		t.Fatal("expected violation when primary file is missing and only numbered variants exist")
	}
	if v.RuleID != RuleBackdropSequencing {
		t.Errorf("RuleID = %q, want %q", v.RuleID, RuleBackdropSequencing)
	}
}

func TestCheckBackdropSequencing_SingleNumberedOnly(t *testing.T) {
	// A single numbered file without the primary (e.g., only fanart2.jpg)
	// should be flagged since it doesn't start at index 0.
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "fanart2.jpg"), 1920, 1080)

	e := &Engine{platformService: nil}
	checker := e.makeBackdropSequencingChecker()

	a := artist.Artist{Name: "Test", Path: dir}
	v := checker(&a, RuleConfig{})
	if v == nil {
		t.Fatal("expected violation for single numbered file without primary")
	}
}

func TestCheckBackdropSequencing_SingleFile(t *testing.T) {
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)

	e := &Engine{platformService: nil}
	checker := e.makeBackdropSequencingChecker()

	a := artist.Artist{Name: "Test", Path: dir}
	v := checker(&a, RuleConfig{})
	if v != nil {
		t.Errorf("expected nil for single fanart file, got: %s", v.Message)
	}
}

// --- countBackdrops tests ---

func TestCountBackdrops_Empty(t *testing.T) {
	dir := t.TempDir()
	e := &Engine{platformService: nil}
	count := e.countBackdrops(dir)
	if count != 0 {
		t.Errorf("countBackdrops empty dir = %d, want 0", count)
	}
}

func TestCountBackdrops_SingleFanart(t *testing.T) {
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)

	e := &Engine{platformService: nil}
	count := e.countBackdrops(dir)
	if count != 1 {
		t.Errorf("countBackdrops single = %d, want 1", count)
	}
}

func TestCountBackdrops_MultipleFanart(t *testing.T) {
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)
	createTestJPEG(t, filepath.Join(dir, "fanart2.jpg"), 1920, 1080)
	createTestJPEG(t, filepath.Join(dir, "fanart3.jpg"), 1920, 1080)

	e := &Engine{platformService: nil}
	count := e.countBackdrops(dir)
	if count < 3 {
		t.Errorf("countBackdrops multiple = %d, want >= 3", count)
	}
}

func TestCountBackdrops_IgnoresNonFanart(t *testing.T) {
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)
	createTestJPEG(t, filepath.Join(dir, "folder.jpg"), 500, 500)
	createTestJPEG(t, filepath.Join(dir, "logo.png"), 400, 200)

	e := &Engine{platformService: nil}
	count := e.countBackdrops(dir)
	if count != 1 {
		t.Errorf("countBackdrops with non-fanart = %d, want 1", count)
	}
}

// --- makeBackdropMinCountChecker tests ---

func TestCheckBackdropMinCount_Satisfied(t *testing.T) {
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)
	createTestJPEG(t, filepath.Join(dir, "fanart2.jpg"), 1920, 1080)

	e := &Engine{platformService: nil}
	checker := e.makeBackdropMinCountChecker()

	a := artist.Artist{Name: "Test", Path: dir}
	v := checker(&a, RuleConfig{MinCount: 2})
	if v != nil {
		t.Errorf("expected nil when count meets minimum, got: %s", v.Message)
	}
}

func TestCheckBackdropMinCount_BelowMinimum(t *testing.T) {
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)

	e := &Engine{platformService: nil}
	checker := e.makeBackdropMinCountChecker()

	a := artist.Artist{Name: "Test", Path: dir}
	v := checker(&a, RuleConfig{MinCount: 3})
	if v == nil {
		t.Fatal("expected violation when count is below minimum")
	}
	if v.RuleID != RuleBackdropMinCount {
		t.Errorf("RuleID = %q, want %q", v.RuleID, RuleBackdropMinCount)
	}
	if v.Fixable {
		t.Error("expected Fixable to be false for detection-only rule")
	}
}

func TestCheckBackdropMinCount_NoBackdrops(t *testing.T) {
	dir := t.TempDir()

	e := &Engine{platformService: nil}
	checker := e.makeBackdropMinCountChecker()

	a := artist.Artist{Name: "Test", Path: dir}
	v := checker(&a, RuleConfig{MinCount: 1})
	if v == nil {
		t.Fatal("expected violation when no backdrops exist")
	}
	if v.RuleID != RuleBackdropMinCount {
		t.Errorf("RuleID = %q, want %q", v.RuleID, RuleBackdropMinCount)
	}
}

func TestCheckBackdropMinCount_DefaultMinCount(t *testing.T) {
	// MinCount 0 should default to 1
	dir := t.TempDir()

	e := &Engine{platformService: nil}
	checker := e.makeBackdropMinCountChecker()

	a := artist.Artist{Name: "Test", Path: dir}
	v := checker(&a, RuleConfig{MinCount: 0})
	if v == nil {
		t.Fatal("expected violation with default min count and no backdrops")
	}
}

func TestCheckBackdropMinCount_ExactlyAtMinimum(t *testing.T) {
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)
	createTestJPEG(t, filepath.Join(dir, "fanart2.jpg"), 1920, 1080)
	createTestJPEG(t, filepath.Join(dir, "fanart3.jpg"), 1920, 1080)

	e := &Engine{platformService: nil}
	checker := e.makeBackdropMinCountChecker()

	a := artist.Artist{Name: "Test", Path: dir}
	v := checker(&a, RuleConfig{MinCount: 3})
	if v != nil {
		t.Errorf("expected nil when count equals minimum, got: %s", v.Message)
	}
}

// --- checkLogoPadding tests ---

func TestCheckLogoPadding_ExcessPadding(t *testing.T) {
	dir := t.TempDir()
	// 200x100 logo with 30px padding on each side. Content = 140x40 = 5600.
	// Total = 200*100 = 20000. Padding ratio = 1 - 5600/20000 = 72%.
	createTestPNGWithPadding(t, filepath.Join(dir, "logo.png"), 200, 100, 30, 30, 30, 30)

	e := newTestEngine()
	checker := e.makeLogoPaddingChecker()
	a := artist.Artist{Name: "Test", LogoExists: true, Path: dir}
	v := checker(&a, RuleConfig{ThresholdPercent: 15})
	if v == nil {
		t.Fatal("expected violation for logo with 72% padding")
	}
	if v.RuleID != RuleLogoPadding {
		t.Errorf("RuleID = %q, want %q", v.RuleID, RuleLogoPadding)
	}
	if !v.Fixable {
		t.Error("expected Fixable to be true")
	}
}

func TestCheckLogoPadding_BelowThreshold(t *testing.T) {
	dir := t.TempDir()
	// 200x100 logo with 5px padding on each side. Content = 190x90 = 17100.
	// Total = 20000. Padding ratio = 1 - 17100/20000 = 14.5%, below 15%.
	createTestPNGWithPadding(t, filepath.Join(dir, "logo.png"), 200, 100, 5, 5, 5, 5)

	e := newTestEngine()
	checker := e.makeLogoPaddingChecker()
	a := artist.Artist{Name: "Test", LogoExists: true, Path: dir}
	v := checker(&a, RuleConfig{ThresholdPercent: 15})
	if v != nil {
		t.Errorf("expected nil for logo with 14.5%% padding, got %v", v)
	}
}

func TestCheckLogoPadding_NoPadding(t *testing.T) {
	dir := t.TempDir()
	createTestPNG(t, filepath.Join(dir, "logo.png"), 200, 100)

	e := newTestEngine()
	checker := e.makeLogoPaddingChecker()
	a := artist.Artist{Name: "Test", LogoExists: true, Path: dir}
	v := checker(&a, RuleConfig{ThresholdPercent: 15})
	if v != nil {
		t.Errorf("expected nil for logo with no padding, got %v", v)
	}
}

func TestCheckLogoPadding_NoLogo(t *testing.T) {
	e := newTestEngine()
	checker := e.makeLogoPaddingChecker()
	a := artist.Artist{Name: "Test", LogoExists: false, Path: t.TempDir()}
	v := checker(&a, RuleConfig{ThresholdPercent: 15})
	if v != nil {
		t.Errorf("expected nil when logo does not exist, got %v", v)
	}
}

func TestCheckLogoPadding_NoPath(t *testing.T) {
	e := newTestEngine()
	checker := e.makeLogoPaddingChecker()
	a := artist.Artist{Name: "Test", LogoExists: true, Path: ""}
	v := checker(&a, RuleConfig{ThresholdPercent: 15})
	if v != nil {
		t.Errorf("expected nil when path is empty, got %v", v)
	}
}

func TestCheckLogoPadding_DefaultThreshold(t *testing.T) {
	dir := t.TempDir()
	// 200x100 logo with 20px padding on each side. Content = 160x60 = 9600.
	// Total = 20000. Padding ratio = 1 - 9600/20000 = 52%.
	// Default threshold is 15%, so this should be flagged.
	createTestPNGWithPadding(t, filepath.Join(dir, "logo.png"), 200, 100, 20, 20, 20, 20)

	e := newTestEngine()
	checker := e.makeLogoPaddingChecker()
	a := artist.Artist{Name: "Test", LogoExists: true, Path: dir}
	v := checker(&a, RuleConfig{})
	if v == nil {
		t.Fatal("expected violation with default threshold (15%)")
	}
}

// createTestJPEGWithWhitespace creates a JPEG with white borders around colored content.
func createTestJPEGWithWhitespace(t *testing.T, path string, totalW, totalH, padLeft, padRight, padTop, padBottom int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, totalW, totalH))
	// Fill with white.
	for y := 0; y < totalH; y++ {
		for x := 0; x < totalW; x++ {
			img.Set(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
		}
	}
	// Draw colored content in the center.
	for y := padTop; y < totalH-padBottom; y++ {
		for x := padLeft; x < totalW-padRight; x++ {
			img.Set(x, y, color.RGBA{R: 100, G: 50, B: 150, A: 255})
		}
	}

	f, err := os.Create(path) //nolint:gosec
	if err != nil {
		t.Fatalf("creating test jpeg: %v", err)
	}
	defer f.Close() //nolint:errcheck

	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encoding jpeg: %v", err)
	}
}

func TestCheckLogoPadding_JPEGWhitespace(t *testing.T) {
	dir := t.TempDir()
	// 200x100 JPEG with 30px white borders on each side.
	// Content = 140x40 = 5600. Total = 20000. Padding = 72%.
	createTestJPEGWithWhitespace(t, filepath.Join(dir, "logo.png"), 200, 100, 30, 30, 30, 30)

	e := newTestEngine()
	checker := e.makeLogoPaddingChecker()
	a := artist.Artist{Name: "Test", LogoExists: true, Path: dir}
	v := checker(&a, RuleConfig{ThresholdPercent: 15})
	if v == nil {
		t.Fatal("expected violation for JPEG logo with whitespace padding")
	}
}

// --- logo bounds cache tests ---

// TestLogoBoundsCache_HitAfterFirstEval verifies that calling the logo padding
// checker twice with the same file results in a cache hit on the second call,
// meaning the image was only decoded once.
func TestLogoBoundsCache_HitAfterFirstEval(t *testing.T) {
	dir := t.TempDir()
	logoPath := filepath.Join(dir, "logo.png")
	createTestPNGWithPadding(t, logoPath, 200, 100, 30, 30, 30, 30)

	e := newTestEngine()
	checker := e.makeLogoPaddingChecker()
	a := artist.Artist{Name: "Test", LogoExists: true, Path: dir}

	// First call: cache miss, image is decoded and result stored.
	v := checker(&a, RuleConfig{ThresholdPercent: 15})
	if v == nil {
		t.Fatal("expected violation on first call")
	}

	// Verify the cache now contains an entry for the logo.
	fi, err := os.Stat(logoPath)
	if err != nil {
		t.Fatalf("stat logo: %v", err)
	}
	cached, hit := e.lookupLogoBounds(logoPath, fi.ModTime())
	if !hit {
		t.Fatal("expected cache hit after first evaluation")
	}
	if cached.original.Dx() == 0 || cached.original.Dy() == 0 {
		t.Errorf("cached original bounds are zero: %v", cached.original)
	}

	// Second call: must use cached result (same violation, no re-decode).
	v2 := checker(&a, RuleConfig{ThresholdPercent: 15})
	if v2 == nil {
		t.Fatal("expected violation on second call (from cache)")
	}
	if v.Message != v2.Message {
		t.Errorf("violation message changed between calls: %q vs %q", v.Message, v2.Message)
	}

	// Prove the cache was hit: the entry count must still be 1. If the second
	// call re-decoded the image and stored a new entry the count would grow.
	e.logoBoundsCacheMu.Lock()
	cacheLen := len(e.logoBoundsCache)
	e.logoBoundsCacheMu.Unlock()
	if cacheLen != 1 {
		t.Errorf("logoBoundsCache len = %d after second call, want 1 (no new entry on cache hit)", cacheLen)
	}
}

// TestLogoBoundsCache_MtimeInvalidation verifies that changing the logo file's
// mtime causes a cache miss and a fresh re-decode. The cache uses
// (filePath, modTime) as the key; replacing the file and advancing its mtime
// produces a different key, so the checker re-decodes the updated content and
// returns the correct result for the new file.
func TestLogoBoundsCache_MtimeInvalidation(t *testing.T) {
	dir := t.TempDir()
	logoPath := filepath.Join(dir, "logo.png")
	createTestPNGWithPadding(t, logoPath, 200, 100, 30, 30, 30, 30)

	e := newTestEngine()
	checker := e.makeLogoPaddingChecker()
	a := artist.Artist{Name: "Test", LogoExists: true, Path: dir}

	// First call: populates the cache.
	v1 := checker(&a, RuleConfig{ThresholdPercent: 15})
	if v1 == nil {
		t.Fatal("expected violation on first call")
	}

	fi1, err := os.Stat(logoPath)
	if err != nil {
		t.Fatalf("stat logo after first write: %v", err)
	}
	if _, hit := e.lookupLogoBounds(logoPath, fi1.ModTime()); !hit {
		t.Fatal("expected cache hit after first evaluation")
	}

	// Replace the file with a fully opaque PNG (no padding) and advance its
	// mtime by at least 1 second to guarantee a different cache key.
	createTestPNG(t, logoPath, 200, 100)
	future := fi1.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(logoPath, future, future); err != nil {
		t.Fatalf("advancing mtime: %v", err)
	}

	// The old mtime key must still be present in the cache (lazy eviction),
	// but the checker must use the new mtime and therefore miss the cache,
	// re-decode the updated file, and return no violation.
	if _, hit := e.lookupLogoBounds(logoPath, fi1.ModTime()); !hit {
		t.Log("old mtime entry was already evicted (acceptable)")
	}

	v2 := checker(&a, RuleConfig{ThresholdPercent: 15})
	if v2 != nil {
		t.Errorf("expected nil violation after replacing with padding-free PNG, got: %s", v2.Message)
	}

	// The new mtime must now be in the cache.
	fi2, err := os.Stat(logoPath)
	if err != nil {
		t.Fatalf("stat logo after replacement: %v", err)
	}
	if _, hit := e.lookupLogoBounds(logoPath, fi2.ModTime()); !hit {
		t.Error("expected cache hit for new mtime after second evaluation")
	}
}

// TestLogoBoundsCache_Eviction verifies that the cache stays within the
// maxLogoBoundsCacheSize limit by evicting the oldest entry when full.
// It also asserts FIFO order: the first-inserted key is evicted before the
// second-inserted key.
func TestLogoBoundsCache_Eviction(t *testing.T) {
	e := newTestEngine()

	// Record the first two keys inserted so we can verify FIFO eviction order.
	firstPath := filepath.Join("/fake", string(rune('a')), "logo.png")
	secondPath := filepath.Join("/fake", string(rune('b')), "logo.png")

	// Fill the cache to capacity using artificial keys.
	for i := 0; i < maxLogoBoundsCacheSize; i++ {
		e.storeLogoBounds(
			filepath.Join("/fake", string(rune('a'+i%26)), "logo.png"),
			testTime(i),
			logoBoundsCacheEntry{},
		)
	}

	e.logoBoundsCacheMu.Lock()
	sizeAtCapacity := len(e.logoBoundsCache)
	e.logoBoundsCacheMu.Unlock()

	if sizeAtCapacity != maxLogoBoundsCacheSize {
		t.Fatalf("cache size = %d, want %d", sizeAtCapacity, maxLogoBoundsCacheSize)
	}

	// Store one more entry: the oldest must be evicted.
	extraPath := "/fake/extra/logo.png"
	e.storeLogoBounds(extraPath, testTime(maxLogoBoundsCacheSize), logoBoundsCacheEntry{})

	e.logoBoundsCacheMu.Lock()
	sizeAfterEvict := len(e.logoBoundsCache)
	e.logoBoundsCacheMu.Unlock()

	if sizeAfterEvict != maxLogoBoundsCacheSize {
		t.Errorf("cache size after eviction = %d, want %d", sizeAfterEvict, maxLogoBoundsCacheSize)
	}

	// The new entry must be present.
	if _, hit := e.lookupLogoBounds(extraPath, testTime(maxLogoBoundsCacheSize)); !hit {
		t.Error("newly inserted entry should be in cache after eviction")
	}

	// FIFO order: the first-inserted key must have been evicted.
	if _, hit := e.lookupLogoBounds(firstPath, testTime(0)); hit {
		t.Error("first-inserted entry should have been evicted (FIFO), but it is still present")
	}

	// The second-inserted key must still be present.
	if _, hit := e.lookupLogoBounds(secondPath, testTime(1)); !hit {
		t.Error("second-inserted entry should still be in cache after one eviction")
	}
}

// testTime is a helper that returns a deterministic time.Time for test keys.
func testTime(n int) time.Time {
	return time.Unix(int64(n), 0)
}

// TestLogoBoundsCache_ConcurrentAccess verifies that concurrent storeLogoBounds
// and lookupLogoBounds calls from multiple goroutines do not cause data races or
// panics, and that the cache does not exceed maxLogoBoundsCacheSize.
func TestLogoBoundsCache_ConcurrentAccess(t *testing.T) {
	const numGoroutines = 20

	e := newTestEngine()

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			path := filepath.Join("/fake", string(rune('a'+i%26)), "logo.png")
			mt := testTime(i)
			e.storeLogoBounds(path, mt, logoBoundsCacheEntry{})
			e.lookupLogoBounds(path, mt)
		}()
	}

	wg.Wait()

	// Cache must not exceed the configured maximum.
	e.logoBoundsCacheMu.Lock()
	size := len(e.logoBoundsCache)
	e.logoBoundsCacheMu.Unlock()

	if size > maxLogoBoundsCacheSize {
		t.Errorf("cache size %d exceeds maximum %d after concurrent access", size, maxLogoBoundsCacheSize)
	}
}

// --- DB-backed dimension resolution tests ---

// seedImageDimensions inserts an artist_images row with the given dimensions
// for use in tests that exercise getImageDimensionsFromDB.
func seedImageDimensions(t *testing.T, db *sql.DB, artistID, imageType string, w, h int) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, low_res, placeholder, width, height, phash, file_format, source, last_written_at)
		VALUES (?, ?, ?, 0, 1, 0, '', ?, ?, '', '', '', '')
		ON CONFLICT(artist_id, image_type, slot_index) DO UPDATE SET exists_flag = 1, width = excluded.width, height = excluded.height`,
		artistID+"-"+imageType, artistID, imageType, w, h,
	)
	if err != nil {
		t.Fatalf("seeding image dimensions: %v", err)
	}
}

func TestGetImageDimensionsFromDB(t *testing.T) {
	db := setupTestDB(t)

	e := &Engine{db: db, logger: slog.Default()}

	// No row: returns (0, 0, nil).
	w, h, err := e.getImageDimensionsFromDB("nonexistent", "thumb")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w != 0 || h != 0 {
		t.Errorf("expected (0,0), got (%d,%d)", w, h)
	}

	// Insert a row with real dimensions.
	seedImageDimensions(t, db, "artist-1", "thumb", 600, 600)

	w, h, err = e.getImageDimensionsFromDB("artist-1", "thumb")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w != 600 || h != 600 {
		t.Errorf("expected (600,600), got (%d,%d)", w, h)
	}

	// Different image type returns (0, 0) when not seeded.
	w, h, err = e.getImageDimensionsFromDB("artist-1", "fanart")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w != 0 || h != 0 {
		t.Errorf("expected (0,0) for unseeded type, got (%d,%d)", w, h)
	}
}

func TestGetImageDimensionsFromDB_NilDB(t *testing.T) {
	e := &Engine{db: nil, logger: slog.Default()}

	w, h, err := e.getImageDimensionsFromDB("artist-1", "thumb")
	if err != nil {
		t.Fatalf("unexpected error with nil db: %v", err)
	}
	if w != 0 || h != 0 {
		t.Errorf("expected (0,0) with nil db, got (%d,%d)", w, h)
	}
}

func TestGetImageDimensionsResolved_DBFirst(t *testing.T) {
	db := setupTestDB(t)
	e := &Engine{db: db, logger: slog.Default()}

	seedImageDimensions(t, db, "artist-2", "fanart", 1920, 1080)

	// DB has dimensions: should return them even with empty Path.
	w, h, err := e.getImageDimensionsResolved("artist-2", "", "fanart", fanartPatterns)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w != 1920 || h != 1080 {
		t.Errorf("expected (1920,1080), got (%d,%d)", w, h)
	}
}

func TestGetImageDimensionsResolved_FallbackToFS(t *testing.T) {
	db := setupTestDB(t)
	e := &Engine{db: db, logger: slog.Default()}

	// DB has (0,0) dimensions for this artist (no row at all).
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)

	w, h, err := e.getImageDimensionsResolved("artist-3", dir, "fanart", fanartPatterns)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w != 1920 || h != 1080 {
		t.Errorf("expected (1920,1080), got (%d,%d)", w, h)
	}
}

func TestGetImageDimensionsResolved_NoDBNoFS(t *testing.T) {
	db := setupTestDB(t)
	e := &Engine{db: db, logger: slog.Default()}

	// No DB dimensions and empty path: should return an error.
	_, _, err := e.getImageDimensionsResolved("artist-4", "", "thumb", thumbPatterns)
	if err == nil {
		t.Error("expected error when neither DB nor filesystem can provide dimensions")
	}
}

// TestCheckers_EmptyPath_DBDimensions verifies that all 6 dimension-based
// checkers produce violations for API-imported artists (Path == "") when the
// artist_images table contains dimensions that fail the rule. This is the core
// bug scenario from issue #726.
func TestCheckers_EmptyPath_DBDimensions(t *testing.T) {
	db := setupTestDB(t)

	tests := []struct {
		name      string
		imageType string
		w, h      int
		checker   func(*Engine) Checker
		cfg       RuleConfig
		artistFn  func(id string) *artist.Artist
		ruleID    string
	}{
		{
			name:      "thumb_square with non-square DB dimensions",
			imageType: "thumb",
			w:         800, h: 400,
			checker: func(e *Engine) Checker { return e.makeThumbSquareChecker() },
			cfg:     RuleConfig{AspectRatio: 1.0, Tolerance: 0.1},
			artistFn: func(id string) *artist.Artist {
				return &artist.Artist{ID: id, Name: "Test", ThumbExists: true, Path: ""}
			},
			ruleID: RuleThumbSquare,
		},
		{
			name:      "thumb_min_res with low-res DB dimensions",
			imageType: "thumb",
			w:         200, h: 200,
			checker: func(e *Engine) Checker { return e.makeThumbMinResChecker() },
			cfg:     RuleConfig{MinWidth: 500, MinHeight: 500},
			artistFn: func(id string) *artist.Artist {
				return &artist.Artist{ID: id, Name: "Test", ThumbExists: true, Path: ""}
			},
			ruleID: RuleThumbMinRes,
		},
		{
			name:      "fanart_min_res with low-res DB dimensions",
			imageType: "fanart",
			w:         800, h: 450,
			checker: func(e *Engine) Checker { return e.makeFanartMinResChecker() },
			cfg:     RuleConfig{MinWidth: 1920, MinHeight: 1080},
			artistFn: func(id string) *artist.Artist {
				return &artist.Artist{ID: id, Name: "Test", FanartExists: true, Path: ""}
			},
			ruleID: RuleFanartMinRes,
		},
		{
			name:      "fanart_aspect with square DB dimensions",
			imageType: "fanart",
			w:         1000, h: 1000,
			checker: func(e *Engine) Checker { return e.makeFanartAspectChecker() },
			cfg:     RuleConfig{AspectRatio: 16.0 / 9.0, Tolerance: 0.1},
			artistFn: func(id string) *artist.Artist {
				return &artist.Artist{ID: id, Name: "Test", FanartExists: true, Path: ""}
			},
			ruleID: RuleFanartAspect,
		},
		{
			name:      "logo_min_res with narrow DB dimensions",
			imageType: "logo",
			w:         200, h: 100,
			checker: func(e *Engine) Checker { return e.makeLogoMinResChecker() },
			cfg:     RuleConfig{MinWidth: 400},
			artistFn: func(id string) *artist.Artist {
				return &artist.Artist{ID: id, Name: "Test", LogoExists: true, Path: ""}
			},
			ruleID: RuleLogoMinRes,
		},
		{
			name:      "banner_min_res with small DB dimensions",
			imageType: "banner",
			w:         500, h: 100,
			checker: func(e *Engine) Checker { return e.makeBannerMinResChecker() },
			cfg:     RuleConfig{MinWidth: 1000, MinHeight: 185},
			artistFn: func(id string) *artist.Artist {
				return &artist.Artist{ID: id, Name: "Test", BannerExists: true, Path: ""}
			},
			ruleID: RuleBannerMinRes,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			artistID := "api-artist-" + tt.name
			seedImageDimensions(t, db, artistID, tt.imageType, tt.w, tt.h)

			e := &Engine{db: db, logger: slog.Default()}
			checker := tt.checker(e)
			a := tt.artistFn(artistID)

			v := checker(a, tt.cfg)
			if v == nil {
				t.Fatalf("expected violation for %s with DB dimensions %dx%d and empty Path", tt.ruleID, tt.w, tt.h)
			}
			if v.RuleID != tt.ruleID {
				t.Errorf("RuleID = %q, want %q", v.RuleID, tt.ruleID)
			}
		})
	}
}

// TestCheckers_EmptyPath_DBDimensions_Pass verifies that checkers pass (return
// nil) when DB dimensions satisfy the rule, even with empty Path.
func TestCheckers_EmptyPath_DBDimensions_Pass(t *testing.T) {
	db := setupTestDB(t)

	tests := []struct {
		name      string
		imageType string
		w, h      int
		checker   func(*Engine) Checker
		cfg       RuleConfig
		artistFn  func(id string) *artist.Artist
	}{
		{
			name:      "thumb_square passes with square DB dimensions",
			imageType: "thumb",
			w:         500, h: 500,
			checker: func(e *Engine) Checker { return e.makeThumbSquareChecker() },
			cfg:     RuleConfig{AspectRatio: 1.0, Tolerance: 0.1},
			artistFn: func(id string) *artist.Artist {
				return &artist.Artist{ID: id, Name: "Test", ThumbExists: true, Path: ""}
			},
		},
		{
			name:      "thumb_min_res passes with high-res DB dimensions",
			imageType: "thumb",
			w:         1000, h: 1000,
			checker: func(e *Engine) Checker { return e.makeThumbMinResChecker() },
			cfg:     RuleConfig{MinWidth: 500, MinHeight: 500},
			artistFn: func(id string) *artist.Artist {
				return &artist.Artist{ID: id, Name: "Test", ThumbExists: true, Path: ""}
			},
		},
		{
			name:      "fanart_min_res passes with full-res DB dimensions",
			imageType: "fanart",
			w:         1920, h: 1080,
			checker: func(e *Engine) Checker { return e.makeFanartMinResChecker() },
			cfg:     RuleConfig{MinWidth: 1920, MinHeight: 1080},
			artistFn: func(id string) *artist.Artist {
				return &artist.Artist{ID: id, Name: "Test", FanartExists: true, Path: ""}
			},
		},
		{
			name:      "fanart_aspect passes with correct aspect ratio DB dimensions",
			imageType: "fanart",
			w:         1920, h: 1080,
			checker: func(e *Engine) Checker { return e.makeFanartAspectChecker() },
			cfg:     RuleConfig{AspectRatio: 16.0 / 9.0, Tolerance: 0.1},
			artistFn: func(id string) *artist.Artist {
				return &artist.Artist{ID: id, Name: "Test", FanartExists: true, Path: ""}
			},
		},
		{
			name:      "logo_min_res passes with high-res DB dimensions",
			imageType: "logo",
			w:         800, h: 400,
			checker: func(e *Engine) Checker { return e.makeLogoMinResChecker() },
			cfg:     RuleConfig{MinWidth: 400},
			artistFn: func(id string) *artist.Artist {
				return &artist.Artist{ID: id, Name: "Test", LogoExists: true, Path: ""}
			},
		},
		{
			name:      "banner_min_res passes with high-res DB dimensions",
			imageType: "banner",
			w:         1200, h: 300,
			checker: func(e *Engine) Checker { return e.makeBannerMinResChecker() },
			cfg:     RuleConfig{MinWidth: 1000, MinHeight: 185},
			artistFn: func(id string) *artist.Artist {
				return &artist.Artist{ID: id, Name: "Test", BannerExists: true, Path: ""}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			artistID := "api-pass-" + tt.name
			seedImageDimensions(t, db, artistID, tt.imageType, tt.w, tt.h)

			e := &Engine{db: db, logger: slog.Default()}
			checker := tt.checker(e)
			a := tt.artistFn(artistID)

			v := checker(a, tt.cfg)
			if v != nil {
				t.Errorf("expected nil (passing rule), got violation: %s", v.Message)
			}
		})
	}
}

// --- API-sourced logo padding tests ---

// mockImageFetcher implements PlatformImageFetcher for testing.
type mockImageFetcher struct {
	fetchData   []byte
	fetchType   string
	fetchErr    error
	fetchCalls  int
	uploadData  []byte
	uploadType  string
	uploadErr   error
	uploadCalls int
	listSlots   map[string]int
	listErr     error
	listCalls   int
}

func (m *mockImageFetcher) FetchArtistImage(_ context.Context, _, _ string) ([]byte, string, error) {
	m.fetchCalls++
	return m.fetchData, m.fetchType, m.fetchErr
}

func (m *mockImageFetcher) UploadArtistImage(_ context.Context, _, _ string, data []byte, contentType string) error {
	m.uploadCalls++
	m.uploadData = data
	m.uploadType = contentType
	return m.uploadErr
}

func (m *mockImageFetcher) ListArtistImageSlots(_ context.Context, _ string) (map[string]int, error) {
	m.listCalls++
	return m.listSlots, m.listErr
}

// createTestPNGBytes returns raw PNG bytes for a padded image without writing
// to disk. The image has the same structure as createTestPNGWithPadding.
//
//nolint:unparam // test helper; totalW/totalH parameterized for readability and parity with createTestPNGWithPadding
func createTestPNGBytes(t *testing.T, totalW, totalH, padLeft, padRight, padTop, padBottom int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, totalW, totalH))
	for y := padTop; y < totalH-padBottom; y++ {
		for x := padLeft; x < totalW-padRight; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding png bytes: %v", err)
	}
	return buf.Bytes()
}

func TestCheckLogoPadding_APIFetch_ExcessPadding(t *testing.T) {
	// 200x100 logo with 30px padding on each side. Padding = 72%.
	data := createTestPNGBytes(t, 200, 100, 30, 30, 30, 30)

	mock := &mockImageFetcher{fetchData: data, fetchType: "image/png"}
	e := newTestEngine()
	e.SetImageFetcher(mock)
	checker := e.makeLogoPaddingChecker()

	a := artist.Artist{ID: "api-001", Name: "API Artist", LogoExists: true, Path: ""}
	v := checker(&a, RuleConfig{ThresholdPercent: 15})
	if v == nil {
		t.Fatal("expected violation for API-fetched logo with 72% padding")
	}
	if v.RuleID != RuleLogoPadding {
		t.Errorf("RuleID = %q, want %q", v.RuleID, RuleLogoPadding)
	}
	if !v.Fixable {
		t.Error("expected Fixable to be true")
	}
	if mock.fetchCalls != 1 {
		t.Errorf("fetchCalls = %d, want 1", mock.fetchCalls)
	}
}

func TestCheckLogoPadding_APIFetch_BelowThreshold(t *testing.T) {
	// 200x100 with 5px padding. Padding = 14.5%, below 15%.
	data := createTestPNGBytes(t, 200, 100, 5, 5, 5, 5)

	mock := &mockImageFetcher{fetchData: data, fetchType: "image/png"}
	e := newTestEngine()
	e.SetImageFetcher(mock)
	checker := e.makeLogoPaddingChecker()

	a := artist.Artist{ID: "api-002", Name: "API Artist 2", LogoExists: true, Path: ""}
	v := checker(&a, RuleConfig{ThresholdPercent: 15})
	if v != nil {
		t.Errorf("expected nil for API logo with 14.5%% padding, got %v", v)
	}
}

func TestCheckLogoPadding_APIFetch_CachesBytes(t *testing.T) {
	// Verify that the checker caches fetched bytes so a second evaluation
	// of the same artist does not fetch again.
	data := createTestPNGBytes(t, 200, 100, 30, 30, 30, 30)

	mock := &mockImageFetcher{fetchData: data, fetchType: "image/png"}
	e := newTestEngine()
	e.SetImageFetcher(mock)
	checker := e.makeLogoPaddingChecker()

	a := artist.Artist{ID: "api-003", Name: "Cache Test", LogoExists: true, Path: ""}

	// First call: fetches from the mock.
	v1 := checker(&a, RuleConfig{ThresholdPercent: 15})
	if v1 == nil {
		t.Fatal("expected violation on first API call")
	}
	if mock.fetchCalls != 1 {
		t.Errorf("fetchCalls after first call = %d, want 1", mock.fetchCalls)
	}

	// Second call: should use cached bytes, not fetch again.
	v2 := checker(&a, RuleConfig{ThresholdPercent: 15})
	if v2 == nil {
		t.Fatal("expected violation on second API call (from cache)")
	}
	if mock.fetchCalls != 1 {
		t.Errorf("fetchCalls after second call = %d, want 1 (cache hit expected)", mock.fetchCalls)
	}
}

func TestCheckLogoPadding_APIFetch_FetchError(t *testing.T) {
	mock := &mockImageFetcher{fetchErr: fmt.Errorf("connection refused")}
	e := newTestEngine()
	e.SetImageFetcher(mock)
	checker := e.makeLogoPaddingChecker()

	a := artist.Artist{ID: "api-004", Name: "Error Artist", LogoExists: true, Path: ""}
	v := checker(&a, RuleConfig{ThresholdPercent: 15})
	if v != nil {
		t.Errorf("expected nil when API fetch fails, got %v", v)
	}
}

func TestCheckLogoPadding_NoPathNoFetcher(t *testing.T) {
	// No path and no fetcher: should return nil without error.
	e := newTestEngine()
	checker := e.makeLogoPaddingChecker()
	a := artist.Artist{ID: "nf-001", Name: "No Fetch", LogoExists: true, Path: ""}
	v := checker(&a, RuleConfig{ThresholdPercent: 15})
	if v != nil {
		t.Errorf("expected nil when no path and no fetcher, got %v", v)
	}
}

// --- DB-based extraneous_images tests ---

// insertTestArtist inserts a minimal artist row for testing DB-based checkers.
// The artist has no filesystem path (simulating an API-imported artist).
func insertTestArtist(t *testing.T, db *sql.DB, id, name string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO artists (id, name, path) VALUES (?, ?, '')`,
		id, name)
	if err != nil {
		t.Fatalf("inserting test artist: %v", err)
	}
}

// insertTestImage inserts a row into artist_images for testing.
func insertTestImage(t *testing.T, db *sql.DB, artistID, imageType string, slotIndex int) {
	t.Helper()
	id := fmt.Sprintf("%s-%s-%d", artistID, imageType, slotIndex)
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag) VALUES (?, ?, ?, ?, 1)`,
		id, artistID, imageType, slotIndex)
	if err != nil {
		t.Fatalf("inserting test image row: %v", err)
	}
}

func TestCheckExtraneousImagesFromDB_ValidImages(t *testing.T) {
	db := setupTestDB(t)
	insertTestArtist(t, db, "art-1", "Test Artist")
	insertTestImage(t, db, "art-1", "thumb", 0)
	insertTestImage(t, db, "art-1", "fanart", 0)
	insertTestImage(t, db, "art-1", "logo", 0)
	insertTestImage(t, db, "art-1", "banner", 0)

	e := &Engine{db: db, logger: slog.Default()}
	a := &artist.Artist{ID: "art-1", Name: "Test Artist", Path: ""}
	v := e.checkExtraneousImagesFromDB(a, RuleConfig{})
	if v != nil {
		t.Errorf("expected nil for artist with only valid images, got: %s", v.Message)
	}
}

func TestCheckExtraneousImagesFromDB_UnknownImageType(t *testing.T) {
	db := setupTestDB(t)
	insertTestArtist(t, db, "art-2", "Test Artist 2")
	insertTestImage(t, db, "art-2", "thumb", 0)
	insertTestImage(t, db, "art-2", "poster", 0) // unknown type

	e := &Engine{db: db, logger: slog.Default()}
	a := &artist.Artist{ID: "art-2", Name: "Test Artist 2", Path: ""}
	v := e.checkExtraneousImagesFromDB(a, RuleConfig{})
	if v == nil {
		t.Fatal("expected violation for unknown image_type 'poster'")
	}
	if v.RuleID != RuleExtraneousImages {
		t.Errorf("RuleID = %q, want %q", v.RuleID, RuleExtraneousImages)
	}
	if v.Fixable {
		t.Error("expected Fixable to be false for DB-based extraneous check")
	}
	if !strings.Contains(v.Message, "poster/0") {
		t.Errorf("expected message to mention 'poster/0', got: %s", v.Message)
	}
}

func TestCheckExtraneousImagesFromDB_InvalidSlotIndex(t *testing.T) {
	db := setupTestDB(t)
	insertTestArtist(t, db, "art-3", "Test Artist 3")
	insertTestImage(t, db, "art-3", "thumb", 0)
	insertTestImage(t, db, "art-3", "thumb", 1) // invalid: thumb only supports slot 0

	e := &Engine{db: db, logger: slog.Default()}
	a := &artist.Artist{ID: "art-3", Name: "Test Artist 3", Path: ""}
	v := e.checkExtraneousImagesFromDB(a, RuleConfig{})
	if v == nil {
		t.Fatal("expected violation for thumb with slot_index > 0")
	}
	if !strings.Contains(v.Message, "thumb/1") {
		t.Errorf("expected message to mention 'thumb/1', got: %s", v.Message)
	}
}

func TestCheckExtraneousImagesFromDB_FanartMultiSlotValid(t *testing.T) {
	db := setupTestDB(t)
	insertTestArtist(t, db, "art-4", "Test Artist 4")
	insertTestImage(t, db, "art-4", "fanart", 0)
	insertTestImage(t, db, "art-4", "fanart", 1)
	insertTestImage(t, db, "art-4", "fanart", 2)

	e := &Engine{db: db, logger: slog.Default()}
	a := &artist.Artist{ID: "art-4", Name: "Test Artist 4", Path: ""}
	v := e.checkExtraneousImagesFromDB(a, RuleConfig{})
	if v != nil {
		t.Errorf("expected nil for fanart with multiple valid slots, got: %s", v.Message)
	}
}

func TestCheckExtraneousImagesFromDB_NoImages(t *testing.T) {
	db := setupTestDB(t)
	insertTestArtist(t, db, "art-5", "Test Artist 5")

	e := &Engine{db: db, logger: slog.Default()}
	a := &artist.Artist{ID: "art-5", Name: "Test Artist 5", Path: ""}
	v := e.checkExtraneousImagesFromDB(a, RuleConfig{})
	if v != nil {
		t.Errorf("expected nil for artist with no images, got: %s", v.Message)
	}
}

func TestCheckExtraneousImagesFromDB_NilDB(t *testing.T) {
	e := &Engine{db: nil, logger: slog.Default()}
	a := &artist.Artist{ID: "art-6", Name: "Test Artist 6", Path: ""}
	v := e.checkExtraneousImagesFromDB(a, RuleConfig{})
	if v != nil {
		t.Errorf("expected nil when db is nil, got: %s", v.Message)
	}
}

func TestCheckExtraneousImagesFromDB_PlatformUntracked(t *testing.T) {
	// Artist has thumb/0 in the DB, but the platform reports thumb + fanart.
	// The fanart/0 slot should be flagged as untracked.
	db := setupTestDB(t)
	insertTestArtist(t, db, "art-plat-1", "Platform Artist")
	insertTestImage(t, db, "art-plat-1", "thumb", 0)

	mock := &mockImageFetcher{
		listSlots: map[string]int{"thumb": 1, "fanart": 2},
	}
	e := &Engine{db: db, logger: slog.Default(), imageFetcher: mock}
	a := &artist.Artist{ID: "art-plat-1", Name: "Platform Artist", Path: ""}
	v := e.checkExtraneousImagesFromDB(a, RuleConfig{})
	if v == nil {
		t.Fatal("expected violation for platform-reported fanart with no DB row")
	}
	if v.RuleID != RuleExtraneousImages {
		t.Errorf("RuleID = %q, want %q", v.RuleID, RuleExtraneousImages)
	}
	if !strings.Contains(v.Message, "fanart/0 (untracked by Stillwater)") {
		t.Errorf("expected message to mention 'fanart/0 (untracked by Stillwater)', got: %s", v.Message)
	}
	if !strings.Contains(v.Message, "fanart/1 (untracked by Stillwater)") {
		t.Errorf("expected message to mention 'fanart/1 (untracked by Stillwater)', got: %s", v.Message)
	}
	// thumb/0 exists in DB, so it should NOT be flagged.
	if strings.Contains(v.Message, "thumb/0 (untracked") {
		t.Errorf("thumb/0 exists in DB and should not be flagged as untracked, got: %s", v.Message)
	}
	if mock.listCalls != 1 {
		t.Errorf("listCalls = %d, want 1", mock.listCalls)
	}
}

func TestCheckExtraneousImagesFromDB_PlatformAllTracked(t *testing.T) {
	// Platform reports slots that all exist in the DB -- no violation expected.
	db := setupTestDB(t)
	insertTestArtist(t, db, "art-plat-2", "Tracked Artist")
	insertTestImage(t, db, "art-plat-2", "thumb", 0)
	insertTestImage(t, db, "art-plat-2", "logo", 0)

	mock := &mockImageFetcher{
		listSlots: map[string]int{"thumb": 1, "logo": 1},
	}
	e := &Engine{db: db, logger: slog.Default(), imageFetcher: mock}
	a := &artist.Artist{ID: "art-plat-2", Name: "Tracked Artist", Path: ""}
	v := e.checkExtraneousImagesFromDB(a, RuleConfig{})
	if v != nil {
		t.Errorf("expected nil when all platform slots are tracked in DB, got: %s", v.Message)
	}
}

func TestCheckExtraneousImagesFromDB_PlatformFetchError(t *testing.T) {
	// When ListArtistImageSlots returns an error, the checker should still
	// return nil if the DB rows are all valid (graceful degradation).
	db := setupTestDB(t)
	insertTestArtist(t, db, "art-plat-3", "Error Artist")
	insertTestImage(t, db, "art-plat-3", "thumb", 0)

	mock := &mockImageFetcher{
		listErr: fmt.Errorf("connection refused"),
	}
	e := &Engine{db: db, logger: slog.Default(), imageFetcher: mock}
	a := &artist.Artist{ID: "art-plat-3", Name: "Error Artist", Path: ""}
	v := e.checkExtraneousImagesFromDB(a, RuleConfig{})
	if v != nil {
		t.Errorf("expected nil when platform fetch fails, got: %s", v.Message)
	}
}

func TestCheckExtraneousImagesFromDB_NoFetcher(t *testing.T) {
	// When no imageFetcher is configured, only DB-based checks run.
	db := setupTestDB(t)
	insertTestArtist(t, db, "art-plat-4", "No Fetcher Artist")
	insertTestImage(t, db, "art-plat-4", "thumb", 0)

	e := &Engine{db: db, logger: slog.Default()} // no imageFetcher
	a := &artist.Artist{ID: "art-plat-4", Name: "No Fetcher Artist", Path: ""}
	v := e.checkExtraneousImagesFromDB(a, RuleConfig{})
	if v != nil {
		t.Errorf("expected nil for valid DB rows with no fetcher, got: %s", v.Message)
	}
}

// --- DB-based backdrop_sequencing tests ---

func TestCheckBackdropSequencingFromDB_Contiguous(t *testing.T) {
	db := setupTestDB(t)
	insertTestArtist(t, db, "art-10", "Test Artist 10")
	insertTestImage(t, db, "art-10", "fanart", 0)
	insertTestImage(t, db, "art-10", "fanart", 1)
	insertTestImage(t, db, "art-10", "fanart", 2)

	e := &Engine{db: db, logger: slog.Default()}
	a := &artist.Artist{ID: "art-10", Name: "Test Artist 10", Path: ""}
	v := e.checkBackdropSequencingFromDB(a, RuleConfig{})
	if v != nil {
		t.Errorf("expected nil for contiguous fanart slots, got: %s", v.Message)
	}
}

func TestCheckBackdropSequencingFromDB_WithGap(t *testing.T) {
	db := setupTestDB(t)
	insertTestArtist(t, db, "art-11", "Test Artist 11")
	insertTestImage(t, db, "art-11", "fanart", 0)
	insertTestImage(t, db, "art-11", "fanart", 2) // gap at index 1

	e := &Engine{db: db, logger: slog.Default()}
	a := &artist.Artist{ID: "art-11", Name: "Test Artist 11", Path: ""}
	v := e.checkBackdropSequencingFromDB(a, RuleConfig{})
	if v == nil {
		t.Fatal("expected violation for gap in fanart slot sequence")
	}
	if v.RuleID != RuleBackdropSequencing {
		t.Errorf("RuleID = %q, want %q", v.RuleID, RuleBackdropSequencing)
	}
	if v.Fixable {
		t.Error("expected Fixable to be false for DB-based sequencing check")
	}
	if !strings.Contains(v.Message, "missing") {
		t.Errorf("expected message to mention 'missing', got: %s", v.Message)
	}
}

func TestCheckBackdropSequencingFromDB_NoFanart(t *testing.T) {
	db := setupTestDB(t)
	insertTestArtist(t, db, "art-12", "Test Artist 12")
	insertTestImage(t, db, "art-12", "thumb", 0)

	e := &Engine{db: db, logger: slog.Default()}
	a := &artist.Artist{ID: "art-12", Name: "Test Artist 12", Path: ""}
	v := e.checkBackdropSequencingFromDB(a, RuleConfig{})
	if v != nil {
		t.Errorf("expected nil when no fanart images exist, got: %s", v.Message)
	}
}

func TestCheckBackdropSequencingFromDB_SingleFanart(t *testing.T) {
	db := setupTestDB(t)
	insertTestArtist(t, db, "art-13", "Test Artist 13")
	insertTestImage(t, db, "art-13", "fanart", 0)

	e := &Engine{db: db, logger: slog.Default()}
	a := &artist.Artist{ID: "art-13", Name: "Test Artist 13", Path: ""}
	v := e.checkBackdropSequencingFromDB(a, RuleConfig{})
	if v != nil {
		t.Errorf("expected nil for single fanart image, got: %s", v.Message)
	}
}

func TestCheckBackdropSequencingFromDB_NilDB(t *testing.T) {
	e := &Engine{db: nil, logger: slog.Default()}
	a := &artist.Artist{ID: "art-14", Name: "Test Artist 14", Path: ""}
	v := e.checkBackdropSequencingFromDB(a, RuleConfig{})
	if v != nil {
		t.Errorf("expected nil when db is nil, got: %s", v.Message)
	}
}

func TestCheckBackdropSequencingFromDB_StartsAtNonZero(t *testing.T) {
	db := setupTestDB(t)
	insertTestArtist(t, db, "art-15", "Test Artist 15")
	insertTestImage(t, db, "art-15", "fanart", 1) // missing slot 0
	insertTestImage(t, db, "art-15", "fanart", 2)

	e := &Engine{db: db, logger: slog.Default()}
	a := &artist.Artist{ID: "art-15", Name: "Test Artist 15", Path: ""}
	v := e.checkBackdropSequencingFromDB(a, RuleConfig{})
	if v == nil {
		t.Fatal("expected violation when fanart starts at slot 1 instead of 0")
	}
}

// TestExtraneousImagesChecker_DispatchesToDB verifies that the full
// makeExtraneousImagesChecker dispatches to the DB path when Path is empty.
func TestExtraneousImagesChecker_DispatchesToDB(t *testing.T) {
	db := setupTestDB(t)
	insertTestArtist(t, db, "art-20", "Dispatch Test")
	insertTestImage(t, db, "art-20", "poster", 0) // unknown type

	e := &Engine{db: db, logger: slog.Default()}
	checker := e.makeExtraneousImagesChecker()
	a := artist.Artist{ID: "art-20", Name: "Dispatch Test", Path: ""}
	v := checker(&a, RuleConfig{})
	if v == nil {
		t.Fatal("expected violation from DB path when Path is empty")
	}
	if v.Fixable {
		t.Error("DB-based extraneous violations should not be fixable")
	}
}

// TestBackdropSequencingChecker_DispatchesToDB verifies that the full
// makeBackdropSequencingChecker dispatches to the DB path when Path is empty.
func TestBackdropSequencingChecker_DispatchesToDB(t *testing.T) {
	db := setupTestDB(t)
	insertTestArtist(t, db, "art-21", "Dispatch Test 2")
	insertTestImage(t, db, "art-21", "fanart", 0)
	insertTestImage(t, db, "art-21", "fanart", 3) // gap

	e := &Engine{db: db, logger: slog.Default()}
	checker := e.makeBackdropSequencingChecker()
	a := artist.Artist{ID: "art-21", Name: "Dispatch Test 2", Path: ""}
	v := checker(&a, RuleConfig{})
	if v == nil {
		t.Fatal("expected violation from DB path when Path is empty")
	}
	if v.Fixable {
		t.Error("DB-based sequencing violations should not be fixable")
	}
}
