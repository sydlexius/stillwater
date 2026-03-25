package nfo

import "strings"

// NFOFieldMap configures how genre/style/mood categories map to NFO XML elements.
// This enables platform-specific compatibility: Emby, Jellyfin, and Kodi each
// interpret <genre>, <style>, and <mood> elements differently.
//
// Three tiers of configuration, evaluated in precedence order:
//   - Tier 3 (AdvancedRemap non-nil): overrides everything, full source-to-element matrix
//   - Tier 1 (DefaultBehavior=true, AdvancedRemap nil): passthrough, ignores MoodsAsStyles and GenreSources
//   - Tier 2 (DefaultBehavior=false, AdvancedRemap nil): uses MoodsAsStyles and GenreSources
type NFOFieldMap struct {
	// DefaultBehavior when true writes each category to its native element
	// (genres to <genre>, styles to <style>, moods to <mood>). This is the
	// Kodi-compatible default. When true, all other settings are ignored.
	DefaultBehavior bool `json:"default_behavior"`

	// MoodsAsStyles when true appends mood values as additional <style>
	// elements, making them visible as Tags in Emby/Jellyfin. The original
	// <mood> elements are still written for Kodi compatibility.
	MoodsAsStyles bool `json:"moods_as_styles"`

	// GenreSources controls which source categories feed the <genre> element.
	// Valid values: "genres", "styles", "moods". Default: ["genres"].
	GenreSources []string `json:"genre_sources"`

	// AdvancedRemap provides a full source-to-element mapping matrix.
	// Keys are NFO XML element names ("genre", "style", "mood").
	// Values are arrays of source category names ("genres", "styles", "moods").
	// When non-nil, this overrides DefaultBehavior, MoodsAsStyles, and GenreSources.
	AdvancedRemap map[string][]string `json:"advanced_remap"`
}

// DefaultFieldMap returns the default (Kodi-compatible) field mapping where
// each category maps directly to its native XML element.
func DefaultFieldMap() NFOFieldMap {
	return NFOFieldMap{
		DefaultBehavior: true,
		GenreSources:    []string{"genres"},
	}
}

// ApplyFieldMap takes the artist's genres, styles, and moods and returns the
// values that should be written to each NFO XML element, based on the field map
// configuration.
//
// The three tiers are evaluated in order of precedence:
//  1. If AdvancedRemap is non-nil: use the remap matrix (overrides everything)
//  2. If DefaultBehavior is true: passthrough (genres/styles/moods unchanged)
//  3. Otherwise: apply GenreSources and MoodsAsStyles settings
//
// Outputs are deduplicated case-insensitively when fields are merged
// (Tier 2 and Tier 3). Tier 1 passthrough returns unmodified copies.
func ApplyFieldMap(fm NFOFieldMap, genres, styles, moods []string) (nfoGenres, nfoStyles, nfoMoods []string) {
	// Tier 3: Advanced remap overrides everything when configured.
	if fm.AdvancedRemap != nil {
		sources := map[string][]string{
			"genres": genres,
			"styles": styles,
			"moods":  moods,
		}
		nfoGenres = collectSources(fm.AdvancedRemap["genre"], sources)
		nfoStyles = collectSources(fm.AdvancedRemap["style"], sources)
		nfoMoods = collectSources(fm.AdvancedRemap["mood"], sources)
		return nfoGenres, nfoStyles, nfoMoods
	}

	// Tier 1: Default behavior is a straight passthrough.
	if fm.DefaultBehavior {
		return copySlice(genres), copySlice(styles), copySlice(moods)
	}

	// Tier 2: Apply GenreSources and MoodsAsStyles.
	sources := map[string][]string{
		"genres": genres,
		"styles": styles,
		"moods":  moods,
	}

	// Build <genre> from configured sources (default: genres only).
	genreSources := fm.GenreSources
	if len(genreSources) == 0 {
		genreSources = []string{"genres"}
	}
	nfoGenres = collectSources(genreSources, sources)

	// Build <style>: start with styles, optionally append moods.
	nfoStyles = copySlice(styles)
	if fm.MoodsAsStyles {
		nfoStyles = deduplicateCaseInsensitive(append(nfoStyles, moods...))
	}

	// Build <mood>: always written for Kodi compatibility.
	nfoMoods = copySlice(moods)

	return nfoGenres, nfoStyles, nfoMoods
}

// collectSources gathers values from the named source categories and returns
// a deduplicated (case-insensitive) slice.
func collectSources(sourceNames []string, sources map[string][]string) []string {
	var result []string
	for _, name := range sourceNames {
		result = append(result, sources[name]...)
	}
	return deduplicateCaseInsensitive(result)
}

// deduplicateCaseInsensitive removes duplicate values case-insensitively,
// preserving the first occurrence of each value.
func deduplicateCaseInsensitive(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, v := range values {
		key := strings.ToLower(v)
		if _, exists := seen[key]; !exists {
			seen[key] = struct{}{}
			result = append(result, v)
		}
	}
	return result
}

// copySlice returns a shallow copy of the string slice. Returns nil for nil
// or empty input to keep XML output clean (omitempty).
func copySlice(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}
