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

	// Guards is 2-4 sentences expanding on the rule's Description, written for
	// the docs reference rather than the in-app violation message. Should name
	// the user-facing problem the rule prevents and the kind of artist
	// metadata the rule reads.
	Guards string

	// Examples lists 1-3 concrete real-world scenarios where the rule fires.
	// Each entry is a single sentence in user-facing language (no field
	// names or struct references).
	Examples []string

	// FixExample is a single concrete before/after illustration of the fix.
	// Empty for detection-only rules and for fixable rules whose fix has no
	// meaningful "before" worth showing (e.g. fetching a missing image).
	FixExample string
}

// rulesCatalogue maps rule IDs to their documentation metadata.
// Entries cover all 22 non-deprecated built-in rules.
var rulesCatalogue = map[string]RuleCatalogueEntry{
	RuleNFOExists: {
		FixBehavior: "Generates an NFO from the artist's stored metadata and writes it to disk.",
		Guards:      "Media servers like Emby and Jellyfin read artist information (name, biography, genre tags, sort order) from an artist.nfo file on disk. Without this file the platform falls back to its own metadata lookup, which can diverge from what Stillwater manages and cannot be locked. Every artist directory needs exactly one artist.nfo for Stillwater to act as the authoritative metadata source.",
		Examples: []string{
			"A newly-scanned artist directory that has never had an NFO written to it.",
			"An artist directory where artist.nfo was manually deleted or never generated after import.",
			"An artist added via a bulk import that completed before the NFO writer ran.",
		},
	},
	RuleNFOHasMBID: {
		FixBehavior: "Asks providers (MusicBrainz first, then any provider whose response carries an MBID reference) for the artist's MBID and writes it to the NFO.",
		Guards:      "The MusicBrainz Artist ID (MBID) is the stable cross-provider key that lets Stillwater retrieve biography, images, and aliases from MusicBrainz, Last.fm, Fanart.tv, and TheAudioDB. Without an MBID, those lookups cannot run and the artist is limited to whatever the initial scan produced. The rule reads the MBID field inside the existing artist.nfo file.",
		Examples: []string{
			"An artist whose NFO was generated from a filesystem scan that found no MBID in a pre-existing nfo.",
			"An NFO written by an older version of Stillwater before MBID population was implemented.",
			"An artist name that matches multiple MusicBrainz entries and was imported without a confirmed identity.",
		},
		FixExample: "Before: artist.nfo has no <musicbrainzartistid> element\nAfter:  artist.nfo contains <musicbrainzartistid>f59c5520-5f46-4d2c-b2c4-822eabf53419</musicbrainzartistid>",
	},
	RuleBioExists: {
		FixBehavior: "Fetches a biography from providers (Last.fm, Wikipedia, TheAudioDB, etc., per your priority order) and saves it to the artist record.",
		Guards:      "An artist without a biography shows a blank text area on artist detail pages in Emby, Jellyfin, and Kodi. The rule reads the biography stored in Stillwater's database and fires when the field is empty or shorter than the configured minimum length (default 10 characters). A biography of fewer than 10 characters is almost always a failed import artifact rather than real content.",
		Examples: []string{
			"An artist who was scanned from disk but whose providers returned no biography text at import time.",
			"An artist whose biography is a single letter or symbol left over from a corrupted provider response.",
			"A recently-added artist who was identified by name only, with no biography fetch attempted yet.",
		},
	},
	RuleMetadataQuality: {
		FixBehavior: "Clears the junk value and re-fetches from providers.",
		Guards:      "Some metadata providers return placeholder strings like '?', 'N/A', or 'No description available.' instead of real content. These pass a simple non-empty check but display as garbage on artist pages. The rule tests the stored biography against a list of known placeholder patterns and against a 50-character minimum length that distinguishes real prose from stub content. It complements bio_exists, which only checks whether any biography is present.",
		Examples: []string{
			"An artist whose biography was set to '?' by an earlier provider response before a better source became available.",
			"An artist biography populated as 'No biography available.' by a provider that did not yet have data.",
			"A biography of exactly one word or punctuation character imported from a minimal provider stub.",
		},
		FixExample: "Before: biography = \"?\"\nAfter:  biography = \"Radiohead are an English rock band formed in Abingdon, Oxfordshire, in 1985...\"",
	},
	RuleDirectoryNameMismatch: {
		FixBehavior: "Renames the directory on disk to match the canonical artist name.",
		Conditional: true,
		Caveats: []string{
			"Requires a local library path; skipped for pathless artists.",
			"Rename is skipped if the target directory already exists to avoid clobbering another artist's folder.",
			"Rename is skipped on shared-filesystem libraries to avoid collisions with the platform's own filesystem operations.",
		},
		Guards: "Stillwater derives the expected directory name from the artist's stored name and the configured article-handling mode (prefix, suffix, or strip). A mismatch means the filesystem path Stillwater tracks no longer matches the name it manages, which can cause platform refresh failures and break album scans that rely on the parent folder matching the artist. The checker compares the current directory's base name to the canonical form, handling Unicode normalization differences between NFC and NFD that macOS filesystems introduce silently.",
		Examples: []string{
			"Artist stored as 'The National' whose folder is '/music/National, The/' under suffix article mode.",
			"Artist stored as 'Smashing Pumpkins' whose folder is '/music/Smashing Pumpkins (Reunion)/' with a parenthetical that remained from a temporary import tag.",
			"Artist whose folder name has trailing whitespace, making it byte-for-byte different from the canonical name despite appearing identical in most file browsers.",
		},
		FixExample: "Before: /music/National, The/\nAfter:  /music/The National/",
	},
	RuleArtistIDMismatch: {
		FixBehavior: "",
		Guards:      "This rule detects when an artist's directory name differs significantly from the artist name stored in Stillwater's database, using fuzzy matching to allow minor variations like punctuation differences while flagging names that appear to belong to a completely different artist. A low similarity score (below the configurable threshold, default 80%) suggests the folder may have been misidentified during import, or that the database record was updated to a different artist while the directory name was never changed. The violation is informational; resolution requires manual review to confirm whether the artist record or the directory name is correct.",
		Examples: []string{
			"Artist 'Arcade Fire' stored in a folder named '/music/Muse/' because two library entries were swapped during a bulk rename.",
			"Artist 'The Cure' in a directory '/music/The Cure (80s discography)/' where the parenthetical pushes similarity below the threshold.",
			"Artist 'Sigur Ros' stored in '/music/Sigur Ros (UK releases)/', where the disambiguation suffix drops the Levenshtein similarity low enough to trigger.",
		},
	},
	RuleNameLanguagePref: {
		FixBehavior: "Updates the artist's stored name to the preferred-language form when one is available from MusicBrainz.",
		Conditional: true,
		Caveats: []string{
			"Only resolves when MusicBrainz returns a locale-specific alias; no change is made if no alias matches.",
		},
		Guards: "When your metadata language preference is set to English but an artist's stored name is in a non-Latin script (for example, the Japanese katakana form of a band name), the name appears in the script of the source provider rather than in the language you want. The rule uses Unicode script analysis to detect when the stored Name or SortName is in a script that does not match any of your preferred languages, and checks MusicBrainz for a localized alias that would fix it. A violation without a matching alias is still raised so you can edit the name manually or dismiss it.",
		Examples: []string{
			"Artist stored as '椎名林檎' when your language preference is 'en' (English) and MusicBrainz has the alias 'Shiina Ringo' available.",
			"Artist stored as 'Rammstein' with a SortName of 'ラムシュタイン' when Japanese is not in your preferred language list.",
			"Artist whose Latin-script stored name is correct for your preference but whose SortName was imported in Cyrillic from a provider that used a different locale.",
		},
		FixExample: "Before: Name = \"椎名林檎\", SortName = \"椎名林檎\"\nAfter:  Name = \"Shiina Ringo\", SortName = \"Ringo, Shiina\"",
	},
	RuleThumbExists: {
		FixBehavior: "Downloads a thumbnail image from configured providers (in priority order) and writes it to the artist directory.",
		Guards:      "The thumbnail (folder.jpg, artist.jpg, or poster.jpg) is the primary image media servers display in library browser grids, album artist headers, and the Now Playing overlay. An artist without a thumbnail appears as a blank placeholder tile in every view that shows artist artwork. The rule checks whether any of the recognized thumbnail filenames exist in the artist's directory.",
		Examples: []string{
			"A newly-imported artist directory that contains only an artist.nfo with no image files yet.",
			"An artist whose folder.jpg was deleted by an external media server cleanup routine.",
			"An artist that was added via API import and has never had images downloaded to disk.",
		},
	},
	RuleThumbSquare: {
		FixBehavior: "Fetches a square replacement thumbnail from providers; the existing non-square image is replaced, not cropped.",
		Guards:      "Media server artist grids, mobile app tiles, and the Now Playing card expect a square (1:1 aspect ratio) thumbnail. A non-square thumbnail is letterboxed or stretched depending on the platform, producing distorted artwork in artist lists. The rule measures the existing thumbnail's pixel dimensions and compares the width-to-height ratio against the configured target (default 1.0) within the configured tolerance (default 10%).",
		Examples: []string{
			"A thumbnail that is 600 x 900 pixels (portrait crop from an album cover), producing a 0.67 ratio that fails the 1.0 ± 0.1 test.",
			"A thumbnail at 1000 x 700 pixels (wide press photo) that appears with black bars when the media server displays it in a square grid cell.",
			"A thumbnail that was manually cropped to a 16:9 ratio and saved over folder.jpg.",
		},
		FixExample: "Before: folder.jpg is 600 x 900 px (ratio 0.67)\nAfter:  folder.jpg is 1000 x 1000 px (ratio 1.00), fetched from provider",
	},
	RuleThumbMinRes: {
		FixBehavior: "Fetches a higher-resolution thumbnail from providers and replaces the undersized image.",
		Guards:      "A low-resolution thumbnail appears blurry on high-density displays and in full-screen Now Playing views. The rule measures the existing thumbnail's pixel dimensions and requires both width and height to meet the configured minimum (default 500 x 500 px). Thumbnails below that size are common when the original image came from a low-quality provider source or was downscaled during an earlier import.",
		Examples: []string{
			"A thumbnail that is 200 x 200 px, imported from a provider that returned only a small preview image.",
			"A folder.jpg that was hand-placed by the user at 300 x 300 px but does not meet the library's quality standard.",
			"A thumbnail that was acceptable at the original 500 x 500 threshold but fails after the minimum was raised to 800 x 800 in settings.",
		},
		FixExample: "Before: folder.jpg is 300 x 300 px\nAfter:  folder.jpg is 1000 x 1000 px, fetched from provider",
	},
	RuleFanartExists: {
		FixBehavior: "Downloads a fanart/backdrop image from configured providers and writes it to the artist directory.",
		Guards:      "Fanart (also called backdrop or backdrop.jpg / fanart.jpg) is the wide background image displayed behind an artist's name in Emby and Jellyfin detail pages, and in screensaver and ambient-mode displays. Without one, those surfaces fall back to the platform's generic gradient. The rule checks whether any of the recognized fanart filenames exist in the artist's directory.",
		Examples: []string{
			"An artist directory that has a thumbnail but no fanart file, leaving the artist detail page with a blank background.",
			"A large library migrated from Kodi where backdrop.jpg files were renamed or moved during the migration.",
			"An artist with many albums whose fanart was never downloaded because the rule was disabled at import time.",
		},
	},
	RuleFanartMinRes: {
		FixBehavior: "Fetches a higher-resolution fanart image from providers and replaces the undersized file.",
		Guards:      "Fanart is displayed at full screen width on artist detail pages and in screensaver mode. At resolutions below 1920 x 1080 (the default minimum), the image appears noticeably soft on 1080p and 4K displays. The rule measures the existing fanart file's pixel dimensions and fires when either dimension falls short of the configured minimum.",
		Examples: []string{
			"A fanart.jpg that is 1280 x 720 px, which passes older standards but falls below the configured 1920 x 1080 minimum.",
			"A fanart that was fetched at a time when the provider only had a standard-definition version available.",
			"A fanart resized to 800 x 600 by a media server's own cleanup pass before Stillwater tracked dimensions.",
		},
		FixExample: "Before: fanart.jpg is 1280 x 720 px\nAfter:  fanart.jpg is 1920 x 1080 px, fetched from provider",
	},
	RuleFanartAspect: {
		FixBehavior: "Fetches a replacement fanart image with the correct aspect ratio from providers.",
		Guards:      "Emby, Jellyfin, and Kodi fanart slots expect a widescreen (16:9) image. A fanart at a different ratio is cropped or letterboxed, cutting off parts of the artwork or adding unsightly bars on artist detail pages and screensavers. The rule measures the existing fanart file and compares its width-to-height ratio against the configured target (default 1.778, which is 16/9) within the configured tolerance (default 10%).",
		Examples: []string{
			"A fanart at 1920 x 1440 px (4:3 ratio) that was fetched from a provider that returned a promotional photo cropped for print.",
			"A fanart at 1000 x 1000 px (square) that was mistakenly saved into the fanart slot instead of the thumbnail slot.",
			"A fanart that appears correct visually at 1920 x 1100 (1.745 ratio) but falls outside the 16:9 ± 10% tolerance window.",
		},
		FixExample: "Before: fanart.jpg is 1920 x 1440 px (ratio 1.33, 4:3)\nAfter:  fanart.jpg is 1920 x 1080 px (ratio 1.78, 16:9), fetched from provider",
	},
	RuleLogoExists: {
		FixBehavior: "Downloads a logo image from configured providers and writes it to the artist directory.",
		Guards:      "The artist logo (logo.png) is a transparent-background image of the band's name or symbol used by Emby and Jellyfin in overlays on artist detail pages and on Now Playing screens that render text-over-artwork. Without a logo the platform falls back to a plain text label, which is less visually polished. The rule checks whether any of the recognized logo filenames (logo.png, logo-white.png) exist in the artist's directory.",
		Examples: []string{
			"An artist directory that has a thumbnail and fanart but no logo.png, leaving the detail-page header without branded typography.",
			"An artist whose logo.png was removed during a library reorganization that excluded non-essential artwork.",
			"A recently-added artist whose providers had logo images available but the logo rule was disabled at the time of import.",
		},
	},
	RuleLogoMinRes: {
		FixBehavior: "Fetches a higher-resolution logo from providers and replaces the undersized file.",
		Guards:      "A low-resolution logo appears pixelated when scaled up on high-density displays. Logos are rendered at varying sizes depending on the platform and screen, and a logo narrower than the configured minimum (default 400 px) is noticeably soft in full-screen views. The rule checks only the width because logos are typically measured by their horizontal extent, and height varies with the design.",
		Examples: []string{
			"A logo.png that is 200 px wide, fetched from a provider that only had a small preview variant.",
			"A logo that was acceptable at a previous 200 px threshold but fails after the minimum was raised in settings.",
			"A logo whose original high-resolution file was replaced by a downscaled copy during a manual library edit.",
		},
		FixExample: "Before: logo.png is 200 px wide\nAfter:  logo.png is 800 px wide, fetched from provider",
	},
	RuleLogoPadding: {
		FixBehavior: "Crops the excess transparent or whitespace border from the logo image and writes the trimmed version in place.",
		Guards:      "Logo images from some providers include large transparent borders (PNG alpha) or white margins (JPG) around the actual artwork. This padding causes the logo to appear smaller than it should when platforms scale it to fit an overlay area, and can misalign it relative to other page elements. The rule computes the ratio of the padding area to the total image area and fires when padding exceeds the configured threshold (default 15%).",
		Examples: []string{
			"A logo.png whose artwork occupies only 40% of the image canvas, with a 60% transparent border added by the provider for padding.",
			"A logo fetched from Fanart.tv where the source artist uploaded the image on a white background with generous margins.",
			"A 1000 x 200 px logo where the actual letterform sits in a 500 x 100 px region centered in the canvas.",
		},
		FixExample: "Before: logo.png is 1000 x 400 px; artwork occupies a 500 x 200 px region (75% padding)\nAfter:  logo.png is 504 x 204 px (content bounds plus 2 px margin)",
	},
	RuleBannerExists: {
		FixBehavior: "Downloads a banner image from configured providers and writes it to the artist directory.",
		Guards:      "The banner image (banner.jpg) is a wide, short strip (typically 1000 x 185 px) displayed in the legacy list view and in some Kodi skins as a header bar above the artist's album list. Without one, that view falls back to a generic colored bar or plain text. The rule checks whether any of the recognized banner filenames (banner.jpg, banner.png) exist in the artist's directory.",
		Examples: []string{
			"An artist directory that has thumbnail and fanart images but no banner, leaving banner-view skins without artwork.",
			"A Kodi-focused library where banners were standard but the source was migrated from Emby which does not use them.",
			"An artist added with the banner rule disabled; when the rule is later enabled it identifies the gap.",
		},
	},
	RuleBannerMinRes: {
		FixBehavior: "Fetches a higher-resolution banner from providers and replaces the undersized file.",
		Guards:      "Banner images are displayed at their natural dimensions in list views; a banner narrower than 1000 px or shorter than 185 px (the defaults) appears noticeably small or pixelated in the header bar slot. The rule checks both dimensions because banners have a fixed strip shape and under-size in either axis is visible.",
		Examples: []string{
			"A banner.jpg that is 800 x 150 px, below the default 1000 x 185 px minimum.",
			"A banner fetched when only a low-quality variant was available, which fails after the minimum was tightened.",
			"A banner that was hand-placed at a non-standard 640 x 120 px size that looked acceptable at the original display scale.",
		},
		FixExample: "Before: banner.jpg is 800 x 150 px\nAfter:  banner.jpg is 1000 x 185 px, fetched from provider",
	},
	RuleBackdropSequencing: {
		FixBehavior: "Renames backdrop/fanart files to the canonical sequencing pattern (fanart.jpg, fanart1.jpg, fanart2.jpg ...).",
		Guards:      "When an artist has multiple backdrop images they must follow a contiguous naming sequence (fanart.jpg, fanart1.jpg, fanart2.jpg, ...) so that media servers discover all of them during a refresh scan. A gap in the sequence (for example, fanart.jpg and fanart3.jpg with no fanart1.jpg or fanart2.jpg) causes later images to be missed. Non-zero starting indices (fanart1.jpg with no fanart.jpg) have the same effect. The rule detects any deviation from the expected contiguous pattern.",
		Examples: []string{
			"An artist with fanart.jpg and fanart3.jpg on disk after fanart1.jpg and fanart2.jpg were deleted, leaving a sequence with a gap at positions 1 and 2.",
			"An artist with only fanart1.jpg present (index starts at 1) because the primary fanart.jpg was replaced with a renamed copy.",
			"An artist with fanart.jpg, fanart2.jpg, and fanart5.jpg after individual images were deleted and not renumbered.",
		},
		FixExample: "Before: fanart.jpg, fanart3.jpg  (gap at indices 1 and 2)\nAfter:  fanart.jpg, fanart1.jpg  (renumbered to fill gap)",
	},
	RuleBackdropMinCount: {
		FixBehavior: "",
		Guards:      "Some media server themes and screensaver modes rotate through multiple backdrop images for an artist; with only one backdrop (or none) those modes cannot cycle and the screen remains static. The rule counts the total number of backdrop/fanart files in the artist directory (or in the artist_images table for API-imported artists) and fires when the count falls below the configured minimum (default 1). Because no automated source provides multiple backdrops on demand, the violation is detection-only and requires you to upload additional images manually.",
		Examples: []string{
			"An artist with zero backdrops on disk who passed the fanart_exists rule because the single fanart was since deleted.",
			"An artist configured to require at least 3 backdrops for screensaver rotation, but whose directory has only 1.",
		},
	},
	RuleExtraneousImages: {
		FixBehavior: "Deletes image files in the artist directory that are not in the active platform profile's expected filenames; on shared-filesystem libraries the union of all configured profiles' filenames is used so files owned by another platform are preserved.",
		Conditional: true,
		Caveats: []string{
			"Runs in manual mode only; never auto-deletes files.",
			"Requires a configured platform profile; on shared-filesystem libraries the fix is skipped if no platform service is available.",
		},
		Guards: "Image files with non-canonical names (such as old backups, editor temp files, or images written by a platform under a different naming scheme) can confuse media servers during refresh. Emby and Jellyfin may pick up an unexpected file as the primary artwork if it sorts before the intended image. The rule builds the set of expected filenames from the active platform profile and the default canonical names, then flags any image file (jpg/jpeg/png) in the artist directory that is not in that set. Numbered fanart variants (fanart1.jpg, fanart2.jpg) that follow the contiguous sequence are whitelisted automatically.",
		Examples: []string{
			"An artist directory containing a 'backdrop_old.png' left over from a manual image swap, alongside the canonical fanart.jpg.",
			"A directory with 'folder_backup.jpg' saved by an earlier media server as a pre-overwrite copy before Stillwater managed the thumbnail.",
			"A directory with 'artistthumb.jpg' written by Kodi under a non-active profile's naming convention, which is not in the current Emby-profile expected-file set.",
		},
		FixExample: "Before: /music/Pink Floyd/ contains fanart.jpg, folder.jpg, backdrop_old.png, artist_backup.jpg\nAfter:  /music/Pink Floyd/ contains fanart.jpg, folder.jpg  (two extraneous files deleted)",
	},
	RuleImageDuplicate: {
		FixBehavior: "",
		Guards:      "When the thumbnail and fanart, or logo and banner, contain the same underlying photograph, media server detail pages show the same image in multiple slots, which wastes platform resources and looks unintentional. The rule reads pre-computed perceptual hash (dHash) values from Stillwater's database and compares all cross-slot pairs using Hamming distance; two images are considered duplicates when their similarity meets or exceeds the configured threshold (default 90%). The violation is informational; resolving it requires manually replacing one of the images with a distinct alternative.",
		Examples: []string{
			"An artist whose thumbnail and fanart are the same press photo, automatically resized into both slots by an earlier fix pass.",
			"An artist where logo.png and banner.jpg were both fetched from the same provider image source and are visually identical despite different dimensions.",
			"A library that was seeded by copying the fanart into every image slot as a placeholder before sourcing distinct artwork.",
		},
	},
}

// CatalogueEntry returns the documentation metadata for a rule ID.
// Returns a zero-value RuleCatalogueEntry if the ID is not present
// (e.g. deprecated rules).
func CatalogueEntry(id string) RuleCatalogueEntry {
	return rulesCatalogue[id]
}
