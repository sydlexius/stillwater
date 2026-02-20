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

func TestEffectiveSeverity(t *testing.T) {
	if s := effectiveSeverity(RuleConfig{Severity: "error"}); s != "error" {
		t.Errorf("expected error, got %q", s)
	}
	if s := effectiveSeverity(RuleConfig{}); s != "warning" {
		t.Errorf("expected warning, got %q", s)
	}
}
