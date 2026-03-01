package rule

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

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
var logoPatterns = []string{"logo.png", "logo-white.png"}
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

// Regex patterns for artist name normalisation. Prefixed with "artist" to
// avoid collisions with any patterns in other checker files.
var (
	artistParenSuffix = regexp.MustCompile(`\s*\(.*\)\s*$`)
	artistPunctuation = regexp.MustCompile(`[^\p{L}\p{N}\s]`)
	artistMultiSpace  = regexp.MustCompile(`\s+`)
)

// normalizeArtistName lowercases, removes trailing parenthetical suffixes,
// strips a leading "The " or trailing ", The" (sort-name format), strips
// punctuation, and collapses whitespace.
func normalizeArtistName(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = artistParenSuffix.ReplaceAllString(s, "")
	s = strings.TrimPrefix(s, "the ")
	s = strings.TrimSuffix(s, ", the")
	s = artistPunctuation.ReplaceAllString(s, "")
	s = artistMultiSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// levenshteinDistance computes the edit distance between two strings using
// runes so that Unicode characters are handled correctly.
func levenshteinDistance(a, b string) int {
	ra := []rune(a)
	rb := []rune(b)
	la, lb := len(ra), len(rb)

	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}

	// Single-row DP.
	prev := make([]int, lb+1)
	for j := range prev {
		prev[j] = j
	}

	for i := 1; i <= la; i++ {
		cur := make([]int, lb+1)
		cur[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if unicode.ToLower(ra[i-1]) == unicode.ToLower(rb[j-1]) {
				cost = 0
			}
			ins := cur[j-1] + 1
			del := prev[j] + 1
			sub := prev[j-1] + cost
			m := ins
			if del < m {
				m = del
			}
			if sub < m {
				m = sub
			}
			cur[j] = m
		}
		prev = cur
	}

	return prev[lb]
}

// nameSimilarity returns a value between 0.0 and 1.0 indicating how similar
// two strings are, based on normalised Levenshtein distance.
func nameSimilarity(a, b string) float64 {
	if a == "" && b == "" {
		return 1.0
	}
	maxLen := len([]rune(a))
	if l := len([]rune(b)); l > maxLen {
		maxLen = l
	}
	if maxLen == 0 {
		return 1.0
	}
	dist := levenshteinDistance(a, b)
	return 1.0 - float64(dist)/float64(maxLen)
}

func checkLogoTrimmable(a *artist.Artist, cfg RuleConfig) *Violation {
	if !a.LogoExists || a.Path == "" {
		return nil
	}

	// Find the logo file on disk using case-insensitive matching; only PNG
	// files have an alpha channel.
	entries, readErr := os.ReadDir(a.Path)
	if readErr != nil {
		return nil
	}
	lowerToActual := make(map[string]string, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			lowerToActual[strings.ToLower(e.Name())] = e.Name()
		}
	}

	var logoPath string
	for _, pattern := range logoPatterns {
		if actual, ok := lowerToActual[strings.ToLower(pattern)]; ok {
			if strings.ToLower(filepath.Ext(actual)) == ".png" {
				logoPath = filepath.Join(a.Path, actual)
				break
			}
		}
	}
	if logoPath == "" {
		return nil
	}

	f, err := os.Open(logoPath) //nolint:gosec // G304: path from trusted library root
	if err != nil {
		return nil
	}
	defer f.Close() //nolint:errcheck

	content, original, err := image.TrimAlphaBounds(f, 128)
	if err != nil || original.Dx() == 0 || original.Dy() == 0 {
		return nil
	}

	// If content equals original, there is no padding to trim.
	if content == original {
		return nil
	}

	threshold := cfg.ThresholdPercent
	if threshold == 0 {
		threshold = 5
	}
	if threshold < 0 {
		threshold = 0
	} else if threshold > 100 {
		threshold = 100
	}
	threshFrac := threshold / 100.0

	leftFrac := float64(content.Min.X-original.Min.X) / float64(original.Dx())
	rightFrac := float64(original.Max.X-content.Max.X) / float64(original.Dx())
	topFrac := float64(content.Min.Y-original.Min.Y) / float64(original.Dy())
	bottomFrac := float64(original.Max.Y-content.Max.Y) / float64(original.Dy())

	if leftFrac <= threshFrac && rightFrac <= threshFrac && topFrac <= threshFrac && bottomFrac <= threshFrac {
		return nil
	}

	return &Violation{
		RuleID:   RuleLogoTrimmable,
		RuleName: "Logo transparent padding",
		Category: "image",
		Severity: effectiveSeverity(cfg),
		Message: fmt.Sprintf(
			"artist %q logo has excess transparent padding (left %.1f%%, right %.1f%%, top %.1f%%, bottom %.1f%%)",
			a.Name, leftFrac*100, rightFrac*100, topFrac*100, bottomFrac*100,
		),
		Fixable: true,
	}
}

func checkArtistIDMismatch(a *artist.Artist, cfg RuleConfig) *Violation {
	if a.Path == "" {
		return nil
	}

	folderName := filepath.Base(a.Path)
	normFolder := normalizeArtistName(folderName)
	normStored := normalizeArtistName(a.Name)

	// Also consider the sort name (e.g. "Beatles, The") so that
	// sort-name-style folder names do not trigger false positives.
	normSort := normalizeArtistName(a.SortName)

	// Exact normalised match against either name or sort name passes.
	if normFolder == normStored || (normSort != "" && normFolder == normSort) {
		return nil
	}

	tolerance := cfg.Tolerance
	if tolerance == 0 {
		tolerance = 0.8
	}

	// Use the higher similarity of the two comparisons.
	sim := nameSimilarity(normFolder, normStored)
	if normSort != "" {
		if sortSim := nameSimilarity(normFolder, normSort); sortSim > sim {
			sim = sortSim
		}
	}
	if sim >= tolerance {
		return nil
	}

	return &Violation{
		RuleID:   RuleArtistIDMismatch,
		RuleName: "Artist/ID mismatch",
		Category: "metadata",
		Severity: effectiveSeverity(cfg),
		Message:  fmt.Sprintf("folder name %q does not match artist name %q (%.0f%% similar)", folderName, a.Name, sim*100),
		Fixable:  false,
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

	// Whitelist numbered fanart variants discovered on disk plus their
	// canonical names and alternate extensions. Discover against ALL
	// configured fanart names (not just the primary) so that numbered
	// variants of non-primary names are also whitelisted.
	//
	// TODO: DiscoverFanart calls os.ReadDir per fanart name. For typical
	// artist directories (10-20 files) this is negligible, but if
	// performance becomes a concern, cache the directory listing and
	// share it across iterations.
	if artistPath != "" {
		var fanartNames []string
		if profile != nil {
			fanartNames = profile.ImageNaming.NamesForType("fanart")
		}
		if len(fanartNames) == 0 {
			fanartNames = image.FileNamesForType(image.DefaultFileNames, "fanart")
		}
		kodiNumbering := profile != nil && strings.EqualFold(profile.ID, "kodi")
		for _, fanartName := range fanartNames {
			discovered := image.DiscoverFanart(artistPath, fanartName)
			for i, p := range discovered {
				// Whitelist the actual file on disk.
				expected[strings.ToLower(filepath.Base(p))] = true
				// Whitelist the canonical name for this index position.
				canonical := image.FanartFilename(fanartName, i, kodiNumbering)
				expected[strings.ToLower(canonical)] = true
				// Whitelist alternate extensions of the canonical name.
				base := strings.TrimSuffix(canonical, filepath.Ext(canonical))
				for ext := range imageExtensions {
					expected[strings.ToLower(base+ext)] = true
				}
			}
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
