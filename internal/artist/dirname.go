package artist

// dirname.go -- shared helper for computing the expected on-disk directory
// name for an artist given an article-handling mode.
//
// CanonicalDirName is the single source of truth used by both the rule engine
// (internal/rule for the directory_name_mismatch checker and fixer) and the
// merge orchestrator (MergeArtists in merge_artists.go) when deciding which
// of a near-duplicate pair carries the "MB-canonical" directory basename.
// Keeping the helper in the artist package avoids an import cycle (rule
// already imports artist; the inverse would not compile).

import "strings"

// CommonArticles are English articles stripped, suffixed, or kept as-is
// depending on the article-handling mode passed to CanonicalDirName.
var CommonArticles = []string{"The", "A", "An"}

// CanonicalDirName returns the expected directory name for an artist given
// the article handling mode. Returns the empty string when the name is empty
// or resolves to an unsafe path element ("." or "..").
//
// Modes:
//   - "prefix" (default): keep the leading article in place ("The Cure" -> "The Cure").
//   - "suffix": move the leading article to the end with a comma ("The Cure" -> "Cure, The").
//   - "strip":  drop the leading article entirely ("The Cure" -> "Cure").
//
// Filesystem-reserved characters in the source name are replaced with
// underscores (AC/DC -> AC_DC) so the result is safe to use as a directory
// basename on every supported platform.
func CanonicalDirName(name, articleMode string) string {
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
		for _, art := range CommonArticles {
			prefix := art + " "
			if len(name) > len(prefix) && strings.EqualFold(name[:len(prefix)], prefix) {
				return name[len(prefix):] + ", " + name[:len(art)]
			}
		}
	case "strip":
		for _, art := range CommonArticles {
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
