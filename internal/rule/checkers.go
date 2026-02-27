package rule

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/platform"
)

// Checker evaluates a single rule against an artist.
// Returns a Violation if the rule is not satisfied, or nil if it passes.
type Checker func(a *artist.Artist, cfg RuleConfig) *Violation

// thumbPatterns matches the scanner's detection patterns for thumbnails.
var thumbPatterns = []string{
	"folder.jpg", "folder.png",
	"artist.jpg", "artist.png",
	"poster.jpg", "poster.png",
}

var fanartPatterns = []string{"fanart.jpg", "fanart.png", "backdrop.jpg", "backdrop.png"}
var logoPatterns = []string{"logo.png", "logo.jpg", "clearlogo.png"}
var bannerPatterns = []string{"banner.jpg", "banner.png"}

func checkNFOExists(a *artist.Artist, _ RuleConfig) *Violation {
	if a.NFOExists {
		return nil
	}
	return &Violation{
		RuleID:   RuleNFOExists,
		RuleName: "NFO file exists",
		Category: "nfo",
		Severity: "error",
		Message:  fmt.Sprintf("artist %q has no artist.nfo file", a.Name),
		Fixable:  true,
	}
}

func checkNFOHasMBID(a *artist.Artist, _ RuleConfig) *Violation {
	if a.MusicBrainzID != "" {
		return nil
	}
	return &Violation{
		RuleID:   RuleNFOHasMBID,
		RuleName: "NFO has MusicBrainz ID",
		Category: "nfo",
		Severity: "error",
		Message:  fmt.Sprintf("artist %q has no MusicBrainz ID", a.Name),
		Fixable:  true,
	}
}

func checkThumbExists(a *artist.Artist, _ RuleConfig) *Violation {
	if a.ThumbExists {
		return nil
	}
	return &Violation{
		RuleID:   RuleThumbExists,
		RuleName: "Thumbnail image exists",
		Category: "image",
		Severity: "error",
		Message:  fmt.Sprintf("artist %q has no thumbnail image", a.Name),
		Fixable:  true,
	}
}

func checkThumbSquare(a *artist.Artist, cfg RuleConfig) *Violation {
	if !a.ThumbExists {
		return nil // thumb_exists rule handles this case
	}

	w, h, err := getThumbDimensions(a.Path)
	if err != nil {
		return nil // cannot read image; skip check
	}

	ratio := cfg.AspectRatio
	if ratio == 0 {
		ratio = 1.0
	}
	tolerance := cfg.Tolerance
	if tolerance == 0 {
		tolerance = 0.1
	}

	if image.ValidateAspectRatio(w, h, ratio, tolerance) {
		return nil
	}

	actual := float64(w) / float64(h)
	return &Violation{
		RuleID:   RuleThumbSquare,
		RuleName: "Thumbnail is square",
		Category: "image",
		Severity: effectiveSeverity(cfg),
		Message:  fmt.Sprintf("artist %q thumbnail aspect ratio %.2f does not match expected %.2f", a.Name, actual, ratio),
		Fixable:  true,
	}
}

func checkThumbMinRes(a *artist.Artist, cfg RuleConfig) *Violation {
	if !a.ThumbExists {
		return nil // thumb_exists rule handles this case
	}

	w, h, err := getThumbDimensions(a.Path)
	if err != nil {
		return nil // cannot read image; skip check
	}

	minW := cfg.MinWidth
	if minW == 0 {
		minW = 500
	}
	minH := cfg.MinHeight
	if minH == 0 {
		minH = 500
	}

	if w >= minW && h >= minH {
		return nil
	}

	return &Violation{
		RuleID:   RuleThumbMinRes,
		RuleName: "Thumbnail minimum resolution",
		Category: "image",
		Severity: effectiveSeverity(cfg),
		Message:  fmt.Sprintf("artist %q thumbnail is %dx%d, minimum required is %dx%d", a.Name, w, h, minW, minH),
		Fixable:  true,
	}
}

func checkFanartExists(a *artist.Artist, _ RuleConfig) *Violation {
	if a.FanartExists {
		return nil
	}
	return &Violation{
		RuleID:   RuleFanartExists,
		RuleName: "Fanart image exists",
		Category: "image",
		Severity: "warning",
		Message:  fmt.Sprintf("artist %q has no fanart image", a.Name),
		Fixable:  true,
	}
}

func checkLogoExists(a *artist.Artist, _ RuleConfig) *Violation {
	if a.LogoExists {
		return nil
	}
	return &Violation{
		RuleID:   RuleLogoExists,
		RuleName: "Logo image exists",
		Category: "image",
		Severity: "info",
		Message:  fmt.Sprintf("artist %q has no logo image", a.Name),
		Fixable:  true,
	}
}

func checkBioExists(a *artist.Artist, cfg RuleConfig) *Violation {
	minLen := cfg.MinLength
	if minLen == 0 {
		minLen = 10
	}

	if len(a.Biography) >= minLen {
		return nil
	}

	msg := fmt.Sprintf("artist %q has no biography", a.Name)
	if a.Biography != "" {
		msg = fmt.Sprintf("artist %q biography is too short (%d chars, minimum %d)", a.Name, len(a.Biography), minLen)
	}

	return &Violation{
		RuleID:   RuleBioExists,
		RuleName: "Biography exists",
		Category: "metadata",
		Severity: effectiveSeverity(cfg),
		Message:  msg,
		Fixable:  true,
	}
}

// getImageDimensions finds the first matching file in the directory and returns its dimensions.
func getImageDimensions(dirPath string, patterns []string) (int, int, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return 0, 0, fmt.Errorf("reading directory: %w", err)
	}

	files := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			files[strings.ToLower(e.Name())] = true
		}
	}

	for _, pattern := range patterns {
		if files[strings.ToLower(pattern)] {
			p := filepath.Join(dirPath, pattern)
			f, err := os.Open(p) //nolint:gosec // G304: path from trusted library root
			if err != nil {
				continue
			}
			w, h, err := image.GetDimensions(f)
			f.Close() //nolint:errcheck
			if err != nil {
				continue
			}
			return w, h, nil
		}
	}

	return 0, 0, fmt.Errorf("no matching image in %s", dirPath)
}

// getThumbDimensions finds and reads the dimensions of the thumbnail image
// in the given artist directory.
func getThumbDimensions(dirPath string) (int, int, error) {
	return getImageDimensions(dirPath, thumbPatterns)
}

func checkFanartMinRes(a *artist.Artist, cfg RuleConfig) *Violation {
	if !a.FanartExists {
		return nil // fanart_exists handles missing fanart
	}
	w, h, err := getImageDimensions(a.Path, fanartPatterns)
	if err != nil {
		return nil
	}
	minW, minH := cfg.MinWidth, cfg.MinHeight
	if minW == 0 {
		minW = 1920
	}
	if minH == 0 {
		minH = 1080
	}
	if w >= minW && h >= minH {
		return nil
	}
	return &Violation{
		RuleID:   RuleFanartMinRes,
		RuleName: "Fanart minimum resolution",
		Category: "image",
		Severity: effectiveSeverity(cfg),
		Message:  fmt.Sprintf("artist %q fanart is %dx%d, minimum required is %dx%d", a.Name, w, h, minW, minH),
		Fixable:  true,
	}
}

func checkFanartAspect(a *artist.Artist, cfg RuleConfig) *Violation {
	if !a.FanartExists {
		return nil
	}
	w, h, err := getImageDimensions(a.Path, fanartPatterns)
	if err != nil {
		return nil
	}
	ratio := cfg.AspectRatio
	if ratio == 0 {
		ratio = 16.0 / 9.0
	}
	tol := cfg.Tolerance
	if tol == 0 {
		tol = 0.1
	}
	if image.ValidateAspectRatio(w, h, ratio, tol) {
		return nil
	}
	actual := float64(w) / float64(h)
	return &Violation{
		RuleID:   RuleFanartAspect,
		RuleName: "Fanart aspect ratio",
		Category: "image",
		Severity: effectiveSeverity(cfg),
		Message:  fmt.Sprintf("artist %q fanart aspect ratio %.3f, expected %.3f", a.Name, actual, ratio),
		Fixable:  true,
	}
}

func checkLogoMinRes(a *artist.Artist, cfg RuleConfig) *Violation {
	if !a.LogoExists {
		return nil
	}
	w, _, err := getImageDimensions(a.Path, logoPatterns)
	if err != nil {
		return nil
	}
	minW := cfg.MinWidth
	if minW == 0 {
		minW = 400
	}
	if w >= minW {
		return nil
	}
	return &Violation{
		RuleID:   RuleLogoMinRes,
		RuleName: "Logo minimum width",
		Category: "image",
		Severity: effectiveSeverity(cfg),
		Message:  fmt.Sprintf("artist %q logo is %dpx wide, minimum is %dpx", a.Name, w, minW),
		Fixable:  true,
	}
}

func checkBannerExists(a *artist.Artist, _ RuleConfig) *Violation {
	if a.BannerExists {
		return nil
	}
	return &Violation{
		RuleID:   RuleBannerExists,
		RuleName: "Banner image exists",
		Category: "image",
		Severity: "info",
		Message:  fmt.Sprintf("artist %q has no banner image", a.Name),
		Fixable:  true,
	}
}

func checkBannerMinRes(a *artist.Artist, cfg RuleConfig) *Violation {
	if !a.BannerExists {
		return nil
	}
	w, h, err := getImageDimensions(a.Path, bannerPatterns)
	if err != nil {
		return nil
	}
	minW, minH := cfg.MinWidth, cfg.MinHeight
	if minW == 0 {
		minW = 1000
	}
	if minH == 0 {
		minH = 185
	}
	if w >= minW && h >= minH {
		return nil
	}
	return &Violation{
		RuleID:   RuleBannerMinRes,
		RuleName: "Banner minimum resolution",
		Category: "image",
		Severity: effectiveSeverity(cfg),
		Message:  fmt.Sprintf("artist %q banner is %dx%d, minimum required is %dx%d", a.Name, w, h, minW, minH),
		Fixable:  true,
	}
}

func effectiveSeverity(cfg RuleConfig) string {
	if cfg.Severity != "" {
		return cfg.Severity
	}
	return "warning"
}

// imageExtensions is the set of file extensions considered as image files.
var imageExtensions = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true,
}

// expectedImageFiles builds the set of expected (canonical) filenames for an
// artist directory given the active platform profile and the artist's directory
// path. Used by both the extraneous images checker and fixer to avoid logic
// duplication.
func expectedImageFiles(profile *platform.Profile, artistPath string) map[string]bool {
	expected := make(map[string]bool)
	expected["artist.nfo"] = true

	imageTypes := []string{"thumb", "fanart", "logo", "banner"}
	for _, imageType := range imageTypes {
		var names []string
		if profile != nil {
			names = profile.ImageNaming.NamesForType(imageType)
		}
		if len(names) == 0 {
			names = image.FileNamesForType(image.DefaultFileNames, imageType)
		}
		for _, name := range names {
			expected[strings.ToLower(name)] = true
			// Also allow alternate extension variants.
			base := strings.TrimSuffix(name, filepath.Ext(name))
			for ext := range imageExtensions {
				expected[strings.ToLower(base+ext)] = true
			}
		}
	}

	// Whitelist numbered fanart variants discovered on disk. This prevents
	// files like backdrop2.jpg, fanart1.jpg from being flagged as extraneous.
	if artistPath != "" {
		var fanartPrimary string
		if profile != nil {
			fanartPrimary = profile.ImageNaming.PrimaryName("fanart")
		}
		if fanartPrimary == "" {
			fanartPrimary = image.PrimaryFileName(image.DefaultFileNames, "fanart")
		}
		for _, p := range image.DiscoverFanart(artistPath, fanartPrimary) {
			expected[strings.ToLower(filepath.Base(p))] = true
		}
	}

	return expected
}

// makeExtraneousImagesChecker returns a Checker closure that detects non-canonical
// image files in an artist directory. The canonical set is derived from the active
// platform profile: for each image type, all configured names plus their alternate
// extension variants are considered expected.
func (e *Engine) makeExtraneousImagesChecker() Checker {
	return func(a *artist.Artist, cfg RuleConfig) *Violation {
		if a.Path == "" {
			return nil
		}

		var profile *platform.Profile
		if e.platformService != nil {
			profile, _ = e.platformService.GetActive(context.Background())
		}
		expected := expectedImageFiles(profile, a.Path)

		entries, readErr := os.ReadDir(a.Path)
		if readErr != nil {
			return nil
		}

		var extraneous []string
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			ext := strings.ToLower(filepath.Ext(name))
			if !imageExtensions[ext] {
				continue
			}
			if !expected[strings.ToLower(name)] {
				extraneous = append(extraneous, name)
			}
		}

		if len(extraneous) == 0 {
			return nil
		}

		return &Violation{
			RuleID:   RuleExtraneousImages,
			RuleName: "Extraneous image files",
			Category: "image",
			Severity: effectiveSeverity(cfg),
			Message:  fmt.Sprintf("artist %q has %d extraneous image file(s): %s", a.Name, len(extraneous), strings.Join(extraneous, ", ")),
			Fixable:  true,
		}
	}
}
