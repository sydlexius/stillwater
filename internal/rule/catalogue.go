package rule

// RuleCatalogueEntry holds documentation metadata for a built-in rule.
// Kept separate from Rule to avoid adding doc-only fields to the API/DB model.
type RuleCatalogueEntry struct {
	// FixBehavior describes what the automated fixer does.
	// Empty string means no automated fix (detection-only).
	FixBehavior string
	// Conditional reports whether the fixer can refuse to act for some
	// violations of this rule (e.g. shared-filesystem libraries, missing
	// localized aliases, manual-only modes). When true the catalogue renders
	// the rule as "Sometimes" rather than "Yes" in the at-a-glance table.
	Conditional bool
	// Caveats lists known limitations or gotchas. May be nil.
	Caveats []string
}

// rulesCatalogue maps rule IDs to their documentation metadata.
// Entries cover all 22 non-deprecated built-in rules.
var rulesCatalogue = map[string]RuleCatalogueEntry{
	RuleNFOExists: {
		FixBehavior: "Generates an NFO from the artist's stored metadata and writes it to disk.",
	},
	RuleNFOHasMBID: {
		FixBehavior: "Asks providers (MusicBrainz first, then any provider whose response carries an MBID reference) for the artist's MBID and writes it to the NFO.",
	},
	RuleBioExists: {
		FixBehavior: "Fetches a biography from providers (Last.fm, Wikipedia, TheAudioDB, etc., per your priority order) and saves it to the artist record.",
	},
	RuleMetadataQuality: {
		FixBehavior: "Clears the junk value and re-fetches from providers.",
	},
	RuleDirectoryNameMismatch: {
		FixBehavior: "Renames the directory on disk to match the canonical artist name.",
		Conditional: true,
		Caveats: []string{
			"Requires a local library path; skipped for pathless artists.",
			"Rename is skipped if the target directory already exists to avoid clobbering another artist's folder.",
			"Rename is skipped on shared-filesystem libraries to avoid collisions with the platform's own filesystem operations.",
		},
	},
	RuleArtistIDMismatch: {
		FixBehavior: "",
	},
	RuleNameLanguagePref: {
		FixBehavior: "Updates the artist's stored name to the preferred-language form when one is available from MusicBrainz.",
		Conditional: true,
		Caveats: []string{
			"Only resolves when MusicBrainz returns a locale-specific alias; no change is made if no alias matches.",
		},
	},
	RuleThumbExists: {
		FixBehavior: "Downloads a thumbnail image from configured providers (in priority order) and writes it to the artist directory.",
	},
	RuleThumbSquare: {
		FixBehavior: "Fetches a square replacement thumbnail from providers; the existing non-square image is replaced, not cropped.",
	},
	RuleThumbMinRes: {
		FixBehavior: "Fetches a higher-resolution thumbnail from providers and replaces the undersized image.",
	},
	RuleFanartExists: {
		FixBehavior: "Downloads a fanart/backdrop image from configured providers and writes it to the artist directory.",
	},
	RuleFanartMinRes: {
		FixBehavior: "Fetches a higher-resolution fanart image from providers and replaces the undersized file.",
	},
	RuleFanartAspect: {
		FixBehavior: "Fetches a replacement fanart image with the correct aspect ratio from providers.",
	},
	RuleLogoExists: {
		FixBehavior: "Downloads a logo image from configured providers and writes it to the artist directory.",
	},
	RuleLogoMinRes: {
		FixBehavior: "Fetches a higher-resolution logo from providers and replaces the undersized file.",
	},
	RuleLogoPadding: {
		FixBehavior: "Crops the excess transparent or whitespace border from the logo image and writes the trimmed version in place.",
	},
	RuleBannerExists: {
		FixBehavior: "Downloads a banner image from configured providers and writes it to the artist directory.",
	},
	RuleBannerMinRes: {
		FixBehavior: "Fetches a higher-resolution banner from providers and replaces the undersized file.",
	},
	RuleBackdropSequencing: {
		FixBehavior: "Renames backdrop/fanart files to the canonical sequencing pattern (fanart.jpg, fanart1.jpg, fanart2.jpg ...).",
	},
	RuleBackdropMinCount: {
		FixBehavior: "",
	},
	RuleExtraneousImages: {
		FixBehavior: "Deletes image files from the artist directory that do not match any recognized Stillwater filename pattern.",
		Conditional: true,
		Caveats: []string{
			"Runs in manual mode only; never auto-deletes files.",
		},
	},
	RuleImageDuplicate: {
		FixBehavior: "",
	},
}

// CatalogueEntry returns the documentation metadata for a rule ID.
// Returns a zero-value RuleCatalogueEntry if the ID is not present
// (e.g. deprecated rules).
func CatalogueEntry(id string) RuleCatalogueEntry {
	return rulesCatalogue[id]
}
