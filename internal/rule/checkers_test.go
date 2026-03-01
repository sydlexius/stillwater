package rule

import (
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

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
	// Create a temp dir with a square image
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "folder.jpg"), 500, 500)

	a := artist.Artist{Name: "Test", ThumbExists: true, Path: dir}
	v := checkThumbSquare(&a, RuleConfig{AspectRatio: 1.0, Tolerance: 0.1})
	if v != nil {
		t.Errorf("expected nil for square thumbnail, got %v", v)
	}

	// Create a non-square image
	dir2 := t.TempDir()
	createTestJPEG(t, filepath.Join(dir2, "folder.jpg"), 800, 400)

	a2 := artist.Artist{Name: "Test2", ThumbExists: true, Path: dir2}
	v2 := checkThumbSquare(&a2, RuleConfig{AspectRatio: 1.0, Tolerance: 0.1})
	if v2 == nil {
		t.Error("expected violation for non-square thumbnail")
	}
}

func TestCheckThumbSquare_NoThumb(t *testing.T) {
	// When thumb does not exist, checker should return nil (thumb_exists handles it)
	a := artist.Artist{Name: "Test", ThumbExists: false}
	v := checkThumbSquare(&a, RuleConfig{AspectRatio: 1.0, Tolerance: 0.1})
	if v != nil {
		t.Errorf("expected nil when ThumbExists is false, got %v", v)
	}
}

func TestCheckThumbMinRes(t *testing.T) {
	// Create a high-res image
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "folder.jpg"), 1000, 1000)

	a := artist.Artist{Name: "Test", ThumbExists: true, Path: dir}
	v := checkThumbMinRes(&a, RuleConfig{MinWidth: 500, MinHeight: 500})
	if v != nil {
		t.Errorf("expected nil for high-res thumbnail, got %v", v)
	}

	// Create a low-res image
	dir2 := t.TempDir()
	createTestJPEG(t, filepath.Join(dir2, "folder.jpg"), 200, 200)

	a2 := artist.Artist{Name: "Test2", ThumbExists: true, Path: dir2}
	v2 := checkThumbMinRes(&a2, RuleConfig{MinWidth: 500, MinHeight: 500})
	if v2 == nil {
		t.Error("expected violation for low-res thumbnail")
	}
}

func TestCheckThumbMinRes_DefaultValues(t *testing.T) {
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "folder.jpg"), 600, 600)

	a := artist.Artist{Name: "Test", ThumbExists: true, Path: dir}
	// Zero config should default to 500x500
	v := checkThumbMinRes(&a, RuleConfig{})
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
	// Missing fanart: skip check
	a := artist.Artist{Name: "Test", FanartExists: false}
	if v := checkFanartMinRes(&a, RuleConfig{}); v != nil {
		t.Errorf("expected nil when FanartExists is false, got %v", v)
	}

	// High-res fanart: pass
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)
	a = artist.Artist{Name: "Test", FanartExists: true, Path: dir}
	if v := checkFanartMinRes(&a, RuleConfig{MinWidth: 1920, MinHeight: 1080}); v != nil {
		t.Errorf("expected nil for 1920x1080 fanart, got %v", v)
	}

	// Low-res fanart: violation
	dir2 := t.TempDir()
	createTestJPEG(t, filepath.Join(dir2, "fanart.jpg"), 800, 450)
	a2 := artist.Artist{Name: "Test2", FanartExists: true, Path: dir2}
	v := checkFanartMinRes(&a2, RuleConfig{MinWidth: 1920, MinHeight: 1080})
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
	if v := checkFanartMinRes(&a3, RuleConfig{}); v != nil {
		t.Errorf("expected nil for 2000x1200 with default 1920x1080, got %v", v)
	}
}

func TestCheckFanartAspect(t *testing.T) {
	// Missing fanart: skip
	a := artist.Artist{Name: "Test", FanartExists: false}
	if v := checkFanartAspect(&a, RuleConfig{}); v != nil {
		t.Errorf("expected nil when FanartExists is false, got %v", v)
	}

	// Correct 16:9 fanart: pass
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)
	a = artist.Artist{Name: "Test", FanartExists: true, Path: dir}
	if v := checkFanartAspect(&a, RuleConfig{AspectRatio: 16.0 / 9.0, Tolerance: 0.1}); v != nil {
		t.Errorf("expected nil for 16:9 fanart, got %v", v)
	}

	// Square fanart: violation
	dir2 := t.TempDir()
	createTestJPEG(t, filepath.Join(dir2, "fanart.jpg"), 1000, 1000)
	a2 := artist.Artist{Name: "Test2", FanartExists: true, Path: dir2}
	v := checkFanartAspect(&a2, RuleConfig{AspectRatio: 16.0 / 9.0, Tolerance: 0.1})
	if v == nil {
		t.Error("expected violation for square fanart with 16:9 check")
	}
	if v != nil && v.RuleID != RuleFanartAspect {
		t.Errorf("RuleID = %q, want %q", v.RuleID, RuleFanartAspect)
	}
}

func TestCheckLogoMinRes(t *testing.T) {
	// Missing logo: skip
	a := artist.Artist{Name: "Test", LogoExists: false}
	if v := checkLogoMinRes(&a, RuleConfig{}); v != nil {
		t.Errorf("expected nil when LogoExists is false, got %v", v)
	}

	// Wide-enough logo: pass
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "logo.png"), 500, 200)
	a = artist.Artist{Name: "Test", LogoExists: true, Path: dir}
	if v := checkLogoMinRes(&a, RuleConfig{MinWidth: 400}); v != nil {
		t.Errorf("expected nil for 500px logo, got %v", v)
	}

	// Narrow logo: violation
	dir2 := t.TempDir()
	createTestJPEG(t, filepath.Join(dir2, "logo.png"), 200, 100)
	a2 := artist.Artist{Name: "Test2", LogoExists: true, Path: dir2}
	v := checkLogoMinRes(&a2, RuleConfig{MinWidth: 400})
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
	if v := checkLogoMinRes(&a3, RuleConfig{}); v != nil {
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
	// Missing banner: skip
	a := artist.Artist{Name: "Test", BannerExists: false}
	if v := checkBannerMinRes(&a, RuleConfig{}); v != nil {
		t.Errorf("expected nil when BannerExists is false, got %v", v)
	}

	// Large-enough banner: pass
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "banner.jpg"), 1000, 185)
	a = artist.Artist{Name: "Test", BannerExists: true, Path: dir}
	if v := checkBannerMinRes(&a, RuleConfig{MinWidth: 1000, MinHeight: 185}); v != nil {
		t.Errorf("expected nil for 1000x185 banner, got %v", v)
	}

	// Small banner: violation
	dir2 := t.TempDir()
	createTestJPEG(t, filepath.Join(dir2, "banner.jpg"), 500, 100)
	a2 := artist.Artist{Name: "Test2", BannerExists: true, Path: dir2}
	v := checkBannerMinRes(&a2, RuleConfig{MinWidth: 1000, MinHeight: 185})
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
	if v := checkBannerMinRes(&a3, RuleConfig{}); v != nil {
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

func TestCheckExtraneousImages_EmptyPath(t *testing.T) {
	e := &Engine{platformService: nil}
	checker := e.makeExtraneousImagesChecker()

	a := artist.Artist{Name: "Test", Path: ""}
	v := checker(&a, RuleConfig{})
	if v != nil {
		t.Errorf("expected nil for empty path, got %v", v)
	}
}

func TestGetImageDimensions_NoMatch(t *testing.T) {
	dir := t.TempDir()
	_, _, err := getImageDimensions(dir, []string{"fanart.jpg", "fanart.png"})
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
