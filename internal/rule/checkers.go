package rule

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/scraper"
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

// getThumbDimensions finds and reads the dimensions of the thumbnail image
// in the given artist directory.
func getThumbDimensions(dirPath string) (int, int, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return 0, 0, fmt.Errorf("reading directory: %w", err)
	}

	files := make(map[string]string, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			files[strings.ToLower(e.Name())] = e.Name()
		}
	}

	for _, pattern := range thumbPatterns {
		if _, ok := files[strings.ToLower(pattern)]; ok {
			thumbPath := filepath.Join(dirPath, pattern)
			f, err := os.Open(thumbPath) //nolint:gosec // G304: path from trusted library root
			if err != nil {
				continue
			}
			w, h, err := image.GetDimensions(f)
			f.Close() //nolint:errcheck,gosec
			if err != nil {
				continue
			}
			return w, h, nil
		}
	}

	return 0, 0, fmt.Errorf("no thumbnail found in %s", dirPath)
}

// makeFallbackChecker returns a Checker that flags artists whose metadata
// was populated by a fallback provider instead of the configured primary.
func makeFallbackChecker(scraperSvc *scraper.Service) Checker {
	return func(a *artist.Artist, cfg RuleConfig) *Violation {
		if len(a.MetadataSources) == 0 {
			return nil
		}
		scraperCfg, err := scraperSvc.GetConfig(context.Background(), scraper.ScopeGlobal)
		if err != nil {
			return nil
		}
		var fallbackFields []string
		for _, fc := range scraperCfg.Fields {
			source, ok := a.MetadataSources[string(fc.Field)]
			if ok && provider.ProviderName(source) != fc.Primary {
				fallbackFields = append(fallbackFields, string(fc.Field)+" ("+source+")")
			}
		}
		if len(fallbackFields) == 0 {
			return nil
		}
		return &Violation{
			RuleID:   RuleFallbackUsed,
			RuleName: "Fallback provider used",
			Category: "metadata",
			Severity: effectiveSeverity(cfg),
			Message: fmt.Sprintf("artist %q: %d field(s) from fallback providers: %s",
				a.Name, len(fallbackFields), strings.Join(fallbackFields, ", ")),
			Fixable: false,
		}
	}
}

func effectiveSeverity(cfg RuleConfig) string {
	if cfg.Severity != "" {
		return cfg.Severity
	}
	return "warning"
}
