package rule

import (
	"context"
	"fmt"
	goimage "image"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
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

// makeThumbSquareChecker returns a Checker closure that uses the Engine's
// cached directory listing to find and measure the thumbnail image.
func (e *Engine) makeThumbSquareChecker() Checker {
	return func(a *artist.Artist, cfg RuleConfig) *Violation {
		if !a.ThumbExists {
			return nil // thumb_exists rule handles this case
		}

		w, h, err := e.getImageDimensionsCached(a.Path, thumbPatterns)
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
}

// makeThumbMinResChecker returns a Checker closure that uses the Engine's
// cached directory listing to find and measure the thumbnail image.
func (e *Engine) makeThumbMinResChecker() Checker {
	return func(a *artist.Artist, cfg RuleConfig) *Violation {
		if !a.ThumbExists {
			return nil // thumb_exists rule handles this case
		}

		w, h, err := e.getImageDimensionsCached(a.Path, thumbPatterns)
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

// makeFanartMinResChecker returns a Checker closure that uses the Engine's
// cached directory listing to find and measure the fanart image.
func (e *Engine) makeFanartMinResChecker() Checker {
	return func(a *artist.Artist, cfg RuleConfig) *Violation {
		if !a.FanartExists {
			return nil // fanart_exists handles missing fanart
		}
		w, h, err := e.getImageDimensionsCached(a.Path, fanartPatterns)
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
}

// makeFanartAspectChecker returns a Checker closure that uses the Engine's
// cached directory listing to find and measure the fanart image.
func (e *Engine) makeFanartAspectChecker() Checker {
	return func(a *artist.Artist, cfg RuleConfig) *Violation {
		if !a.FanartExists {
			return nil
		}
		w, h, err := e.getImageDimensionsCached(a.Path, fanartPatterns)
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
}

// makeLogoMinResChecker returns a Checker closure that uses the Engine's
// cached directory listing to find and measure the logo image.
func (e *Engine) makeLogoMinResChecker() Checker {
	return func(a *artist.Artist, cfg RuleConfig) *Violation {
		if !a.LogoExists {
			return nil
		}
		w, _, err := e.getImageDimensionsCached(a.Path, logoPatterns)
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

// makeBannerMinResChecker returns a Checker closure that uses the Engine's
// cached directory listing to find and measure the banner image.
func (e *Engine) makeBannerMinResChecker() Checker {
	return func(a *artist.Artist, cfg RuleConfig) *Violation {
		if !a.BannerExists {
			return nil
		}
		w, h, err := e.getImageDimensionsCached(a.Path, bannerPatterns)
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

// makeLogoTrimmableChecker returns a Checker closure that detects logos with
// per-edge transparent padding exceeding a threshold. Results of the
// underlying TrimAlphaBounds image decode are cached by (filePath, modTime)
// on the Engine to avoid re-decoding the same PNG on every evaluation.
func (e *Engine) makeLogoTrimmableChecker() Checker {
	return func(a *artist.Artist, cfg RuleConfig) *Violation {
		if !a.LogoExists || a.Path == "" {
			return nil
		}

		// Find the logo file on disk using case-insensitive matching; only PNG
		// files have an alpha channel.
		entries, readErr := e.readDirCached(a.Path)
		if readErr != nil {
			e.logger.Debug("logo trimmable check skipped: cannot read artist directory",
				slog.String("artist", a.Name),
				slog.String("path", a.Path),
				slog.String("error", readErr.Error()))
			return nil
		}
		lowerToActual := buildLowerToActual(entries)

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

		content, original, ok := e.getLogoBoundsAlpha(logoPath)
		if !ok || original.Dx() == 0 || original.Dy() == 0 {
			return nil
		}

		// If content equals original, there is no padding to trim.
		if content == original {
			return nil
		}

		threshold := cfg.ThresholdPercent
		if threshold <= 0 {
			threshold = 5
		}
		if threshold > 100 {
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
}

// getLogoBoundsAlpha returns the content and original bounds for a PNG logo
// using TrimAlphaBounds, with results cached by (filePath, modTime). Returns
// ok=false if the file cannot be stat'd or decoded.
func (e *Engine) getLogoBoundsAlpha(logoPath string) (content, original goimage.Rectangle, ok bool) {
	modTime, err := e.fileModTimeCached(logoPath)
	if err != nil {
		e.logger.Debug("logo bounds skipped: cannot stat file",
			slog.String("path", logoPath),
			slog.String("error", err.Error()))
		return goimage.Rectangle{}, goimage.Rectangle{}, false
	}

	if cached, hit := e.lookupLogoBounds(logoPath, modTime); hit {
		return cached.content, cached.original, true
	}

	f, err := os.Open(logoPath) //nolint:gosec // G304: path from trusted library root
	if err != nil {
		e.logger.Debug("logo bounds skipped: cannot open file",
			slog.String("path", logoPath),
			slog.String("error", err.Error()))
		return goimage.Rectangle{}, goimage.Rectangle{}, false
	}
	defer f.Close() //nolint:errcheck

	c, orig, decErr := image.TrimAlphaBounds(f, 128)
	if decErr != nil {
		e.logger.Debug("logo bounds skipped: decode/trim failed",
			slog.String("path", logoPath),
			slog.String("error", decErr.Error()))
		return goimage.Rectangle{}, goimage.Rectangle{}, false
	}

	e.storeLogoBounds(logoPath, modTime, logoBoundsCacheEntry{content: c, original: orig})
	return c, orig, true
}

// getLogoBoundsContent returns the content and original bounds for any logo
// format using ContentBounds, with results cached by (filePath, modTime).
// Returns ok=false if the file cannot be stat'd or decoded.
func (e *Engine) getLogoBoundsContent(logoPath string) (content, original goimage.Rectangle, ok bool) {
	modTime, err := e.fileModTimeCached(logoPath)
	if err != nil {
		e.logger.Debug("logo bounds skipped: cannot stat file",
			slog.String("path", logoPath),
			slog.String("error", err.Error()))
		return goimage.Rectangle{}, goimage.Rectangle{}, false
	}

	if cached, hit := e.lookupLogoBounds(logoPath, modTime); hit {
		return cached.content, cached.original, true
	}

	f, err := os.Open(logoPath) //nolint:gosec // G304: path from trusted library root
	if err != nil {
		e.logger.Debug("logo bounds skipped: cannot open file",
			slog.String("path", logoPath),
			slog.String("error", err.Error()))
		return goimage.Rectangle{}, goimage.Rectangle{}, false
	}
	defer f.Close() //nolint:errcheck

	c, orig, decErr := image.ContentBounds(f)
	if decErr != nil {
		e.logger.Debug("logo bounds skipped: decode/trim failed",
			slog.String("path", logoPath),
			slog.String("error", decErr.Error()))
		return goimage.Rectangle{}, goimage.Rectangle{}, false
	}

	e.storeLogoBounds(logoPath, modTime, logoBoundsCacheEntry{content: c, original: orig})
	return c, orig, true
}

// makeLogoPaddingChecker returns a Checker closure that detects logos where
// total padding (transparent or whitespace) exceeds a configurable threshold.
// Unlike makeLogoTrimmableChecker which checks per-edge padding, this rule
// uses an area-based ratio and supports both PNG alpha and non-PNG whitespace.
// Results of the underlying ContentBounds image decode are cached by
// (filePath, modTime) on the Engine to avoid re-decoding the same file on
// every evaluation.
func (e *Engine) makeLogoPaddingChecker() Checker {
	return func(a *artist.Artist, cfg RuleConfig) *Violation {
		if !a.LogoExists || a.Path == "" {
			return nil
		}

		entries, readErr := e.readDirCached(a.Path)
		if readErr != nil {
			e.logger.Debug("logo padding check skipped: cannot read artist directory",
				slog.String("artist", a.Name),
				slog.String("path", a.Path),
				slog.String("error", readErr.Error()))
			return nil
		}
		lowerToActual := buildLowerToActual(entries)

		var logoPath string
		for _, pattern := range logoPatterns {
			if actual, ok := lowerToActual[strings.ToLower(pattern)]; ok {
				logoPath = filepath.Join(a.Path, actual)
				break
			}
		}
		if logoPath == "" {
			return nil
		}

		content, original, ok := e.getLogoBoundsContent(logoPath)
		if !ok || original.Dx() == 0 || original.Dy() == 0 {
			return nil
		}

		// If content fills the entire image, there is no padding.
		if content == original {
			return nil
		}

		totalArea := float64(original.Dx() * original.Dy())
		contentArea := float64(content.Dx() * content.Dy())
		paddingRatio := 1.0 - (contentArea / totalArea)

		threshold := cfg.ThresholdPercent
		if threshold <= 0 {
			threshold = 15
		}
		if threshold > 100 {
			threshold = 100
		}
		threshFrac := threshold / 100.0

		if paddingRatio <= threshFrac {
			return nil
		}

		return &Violation{
			RuleID:   RuleLogoPadding,
			RuleName: "Logo excessive padding",
			Category: "image",
			Severity: effectiveSeverity(cfg),
			Message: fmt.Sprintf(
				"artist %q logo has %.1f%% padding (threshold %.0f%%)",
				a.Name, paddingRatio*100, threshold,
			),
			Fixable: true,
		}
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
			discovered, discoverErr := image.DiscoverFanart(artistPath, fanartName)
			if discoverErr != nil {
				slog.Warn("discovering fanart for expected-files whitelist",
					slog.String("dir", artistPath),
					slog.String("primary", fanartName),
					slog.String("error", discoverErr.Error()))
			}
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
//
// When the artist's library has a shared-filesystem status, the expected set is expanded
// to include filenames from ALL platform profiles so that files written by a
// connected platform (Emby, Jellyfin, Kodi) are not flagged as extraneous.
//
// Responsibility boundary: this checker flags files with non-standard names
// (e.g., "backdrop_old.png"). It does NOT flag valid numbered
// fanart variants even if their indices have gaps (e.g., backdrop.jpg +
// backdrop3.jpg with no backdrop2.jpg). Gap detection is handled by the
// backdrop_sequencing rule (#519).
func (e *Engine) makeExtraneousImagesChecker() Checker {
	return func(a *artist.Artist, cfg RuleConfig) *Violation {
		if a.Path == "" {
			return nil
		}

		// When shared filesystem is detected, union expected files from all
		// profiles to avoid flagging platform-written images.
		if e.IsSharedFilesystem(context.Background(), a) && e.platformService != nil {
			expected := expectedImageFilesAllProfiles(context.Background(), e.platformService, e.logger, a.Path)
			return e.checkExtraneousAgainst(a, expected, cfg)
		}

		var profile *platform.Profile
		if e.platformService != nil {
			profile, _ = e.platformService.GetActive(context.Background())
		}
		expected := expectedImageFiles(profile, a.Path)

		entries, readErr := e.readDirCached(a.Path)
		if readErr != nil {
			return nil
		}

		var extraneous []string
		for _, entry := range entries {
			if entry.IsDir {
				continue
			}
			name := entry.Name
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

// checkExtraneousAgainst is the core logic for the extraneous images checker,
// extracted so both the normal and shared-filesystem paths can reuse it.
// It uses the Engine's cached directory listing to avoid redundant I/O.
func (e *Engine) checkExtraneousAgainst(a *artist.Artist, expected map[string]bool, cfg RuleConfig) *Violation {
	entries, readErr := e.readDirCached(a.Path)
	if readErr != nil {
		return nil
	}

	var extraneous []string
	for _, entry := range entries {
		if entry.IsDir {
			continue
		}
		name := entry.Name
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

// expectedImageFilesAllProfiles builds the expected image filename set by
// unioning filenames from ALL platform profiles. Used when a shared-filesystem
// status is detected so that files written by any connected platform are not flagged
// as extraneous. This is a package-level function so both the checker and the
// fixer can share the same logic without duplicating it.
func expectedImageFilesAllProfiles(ctx context.Context, svc *platform.Service, logger *slog.Logger, artistPath string) map[string]bool {
	profiles, err := svc.List(ctx)
	if err != nil {
		logger.Warn("listing profiles for shared-filesystem expected files",
			slog.String("error", err.Error()))
		// Fall back to active profile only.
		active, _ := svc.GetActive(ctx)
		return expectedImageFiles(active, artistPath)
	}

	merged := make(map[string]bool)
	for i := range profiles {
		for k, v := range expectedImageFiles(&profiles[i], artistPath) {
			if v {
				merged[k] = true
			}
		}
	}
	// Always include the default set too (no profile).
	for k, v := range expectedImageFiles(nil, artistPath) {
		if v {
			merged[k] = true
		}
	}
	return merged
}

// commonArticles are English articles stripped, suffixed, or kept as-is
// depending on ArticleMode.
var commonArticles = []string{"The", "A", "An"}

// canonicalDirName returns the expected directory name for an artist given
// the article handling mode. Returns empty string if the name is empty or
// results in an unsafe path element.
func canonicalDirName(name, articleMode string) string {
	if articleMode == "" {
		articleMode = "prefix"
	}

	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}

	// Replace characters not allowed in directory names on common filesystems.
	name = strings.NewReplacer(
		"/", "_",
		"\\", "_",
		":", "_",
		"*", "_",
		"?", "_",
		"\"", "_",
		"<", "_",
		">", "_",
		"|", "_",
	).Replace(name)

	switch articleMode {
	case "suffix":
		for _, art := range commonArticles {
			prefix := art + " "
			if len(name) > len(prefix) && strings.EqualFold(name[:len(prefix)], prefix) {
				return name[len(prefix):] + ", " + name[:len(art)]
			}
		}
	case "strip":
		for _, art := range commonArticles {
			prefix := art + " "
			if len(name) > len(prefix) && strings.EqualFold(name[:len(prefix)], prefix) {
				name = name[len(prefix):]
				break // strip at most one leading article
			}
		}
	}

	// Reject unsafe path elements.
	if name == "" || name == "." || name == ".." {
		return ""
	}
	return name
}

func checkDirectoryNameMismatch(a *artist.Artist, cfg RuleConfig) *Violation {
	if a.Path == "" {
		return nil
	}

	dirName := filepath.Base(a.Path)
	canonical := canonicalDirName(a.Name, cfg.ArticleMode)

	// Skip if the artist name cannot produce a safe directory name.
	if canonical == "" {
		return nil
	}

	if strings.EqualFold(dirName, canonical) {
		return nil
	}

	return &Violation{
		RuleID:   RuleDirectoryNameMismatch,
		RuleName: "Directory name matches artist",
		Category: "metadata",
		Severity: effectiveSeverity(cfg),
		Message:  fmt.Sprintf("directory %q does not match expected %q", dirName, canonical),
		Fixable:  true,
	}

}

// makeImageDuplicateChecker returns a Checker closure that detects visually
// similar images across different image slots for the same artist. It reads
// pre-computed dHash values from the artist_images table and compares all
// pairs using Hamming distance.
func (e *Engine) makeImageDuplicateChecker() Checker {
	return func(a *artist.Artist, cfg RuleConfig) *Violation {
		if a.Path == "" || e.db == nil {
			return nil
		}

		tolerance := cfg.Tolerance
		if tolerance <= 0 || tolerance > 1.0 {
			tolerance = 0.90
		}

		type slotHash struct {
			slot string
			hash uint64
		}

		// Select one hash per image_type (slot_index = 0) to compare across
		// different types only. Within-type comparison (e.g. fanart slot 0 vs 1)
		// is not the goal of this rule.
		rows, err := e.db.QueryContext(context.Background(),
			`SELECT image_type, phash FROM artist_images WHERE artist_id = ? AND slot_index = 0 AND exists_flag = 1 AND phash IS NOT NULL AND phash != '' AND phash != '0000000000000000'`,
			a.ID)
		if err != nil {
			e.logger.Debug("querying image hashes", "artist", a.Name, "error", err)
			return nil
		}
		defer rows.Close() //nolint:errcheck

		var hashes []slotHash
		for rows.Next() {
			var slot, hashStr string
			if err := rows.Scan(&slot, &hashStr); err != nil {
				e.logger.Debug("scanning image hash row", "artist", a.Name, "error", err)
				continue
			}
			h, err := image.ParseHashHex(hashStr)
			if err != nil || h == 0 {
				continue
			}
			hashes = append(hashes, slotHash{slot: slot, hash: h})
		}
		if err := rows.Err(); err != nil {
			e.logger.Debug("iterating image hash rows", "artist", a.Name, "error", err)
			return nil
		}

		// Compare all pairs.
		for i := 0; i < len(hashes); i++ {
			for j := i + 1; j < len(hashes); j++ {
				sim := image.Similarity(hashes[i].hash, hashes[j].hash)
				if sim >= tolerance {
					return &Violation{
						RuleID:   RuleImageDuplicate,
						RuleName: "No duplicate images",
						Category: "image",
						Severity: effectiveSeverity(cfg),
						Message: fmt.Sprintf("artist %q: %s and %s are %.0f%% similar",
							a.Name, hashes[i].slot, hashes[j].slot, sim*100),
						Fixable: false,
					}
				}
			}
		}

		return nil
	}
}

// truncateStr returns the first max runes of s, appending "..." if truncated.
// Uses rune-safe slicing to avoid splitting multi-byte UTF-8 characters.
func truncateStr(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}

func checkMetadataQuality(a *artist.Artist, cfg RuleConfig) *Violation {
	// Check biography for known placeholder/junk patterns.
	// This complements bio_exists (which checks length): a biography that is
	// "?" passes the length check at minLength=1 but is clearly junk.
	if a.Biography != "" && provider.IsJunkValue("biography", a.Biography) {
		return &Violation{
			RuleID:   RuleMetadataQuality,
			RuleName: "Metadata quality",
			Category: "metadata",
			Severity: effectiveSeverity(cfg),
			Message:  fmt.Sprintf("artist %q has placeholder biography: %q", a.Name, truncateStr(a.Biography, 50)),
			Fixable:  true,
		}
	}
	return nil
}

// makeBackdropSequencingChecker returns a Checker closure that detects
// non-contiguous backdrop/fanart numbering. When files like backdrop.jpg,
// backdrop3.jpg exist (gap at 1,2), or backdrop1.jpg exists without
// backdrop.jpg (wrong starting point), a violation is returned.
func (e *Engine) makeBackdropSequencingChecker() Checker {
	return func(a *artist.Artist, cfg RuleConfig) *Violation {
		if a.Path == "" {
			return nil
		}

		var profile *platform.Profile
		if e.platformService != nil {
			profile, _ = e.platformService.GetActive(context.Background())
		}

		var fanartNames []string
		if profile != nil {
			fanartNames = profile.ImageNaming.NamesForType("fanart")
		}
		if len(fanartNames) == 0 {
			fanartNames = image.FileNamesForType(image.DefaultFileNames, "fanart")
		}
		kodiNumbering := profile != nil && strings.EqualFold(profile.ID, "kodi")

		for _, primaryName := range fanartNames {
			discovered, err := image.DiscoverFanart(a.Path, primaryName)
			if err != nil {
				e.logger.Debug("discovering fanart for sequencing check",
					"dir", a.Path,
					"primary", primaryName,
					"error", err)
				continue
			}
			if len(discovered) == 0 {
				continue
			}

			// Check whether files occupy contiguous indices.
			for i, path := range discovered {
				expected := image.FanartFilename(primaryName, i, kodiNumbering)
				actual := filepath.Base(path)
				// Compare base names ignoring extension (jpg vs png is fine).
				expectedBase := strings.TrimSuffix(expected, filepath.Ext(expected))
				actualBase := strings.TrimSuffix(actual, filepath.Ext(actual))
				if !strings.EqualFold(expectedBase, actualBase) {
					var fileList []string
					for _, p := range discovered {
						fileList = append(fileList, filepath.Base(p))
					}
					return &Violation{
						RuleID:   RuleBackdropSequencing,
						RuleName: "Backdrop/fanart sequencing",
						Category: "image",
						Severity: effectiveSeverity(cfg),
						Message:  fmt.Sprintf("artist %q has non-sequential %s files: %s", a.Name, strings.TrimSuffix(primaryName, filepath.Ext(primaryName)), strings.Join(fileList, ", ")),
						Fixable:  true,
					}
				}
			}
		}
		return nil
	}
}
