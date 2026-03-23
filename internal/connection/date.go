package connection

import (
	"regexp"
	"strings"
	"time"
)

// Compiled regexps for structured date detection.
var (
	reISO8601  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T`)
	reYMD      = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	reYM       = regexp.MustCompile(`^\d{4}-\d{2}$`)
	reYear     = regexp.MustCompile(`^\d{4}$`)
	reInSuffix = regexp.MustCompile(`(?i)\s+in\s+.*$`)
	reExtract  = regexp.MustCompile(`\b(1\d{3}|20\d{2})\b`)
)

// Named-month layouts to try after stripping location suffixes.
var namedMonthLayouts = []string{
	"January 2, 2006",
	"Jan 2, 2006",
	"2 January 2006",
	"January 2006",
	"Jan 2006",
}

// NormalizeDateForPlatform converts a free-form date string to yyyy-MM-dd
// format suitable for Emby/Jellyfin PremiereDate and EndDate fields. It
// returns the original string for values already in ISO 8601 format, pads
// partial dates (yyyy or yyyy-MM), parses named-month formats, and falls
// back to extracting the first 4-digit year. Returns "" for completely
// unparsable input.
func NormalizeDateForPlatform(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	// Already full ISO 8601 with time component -- pass through.
	if reISO8601.MatchString(s) {
		return s
	}

	// Already yyyy-MM-dd -- pass through.
	if reYMD.MatchString(s) {
		return s
	}

	// yyyy-MM -- pad day.
	if reYM.MatchString(s) {
		return s + "-01"
	}

	// yyyy -- pad month and day.
	if reYear.MatchString(s) {
		return s + "-01-01"
	}

	// Strip " in <location>" suffix and try named-month layouts.
	cleaned := reInSuffix.ReplaceAllString(s, "")
	cleaned = strings.TrimSpace(cleaned)

	for _, layout := range namedMonthLayouts {
		if t, err := time.Parse(layout, cleaned); err == nil {
			return t.Format("2006-01-02")
		}
	}

	// Last resort: extract first 4-digit year (1000-2099).
	if m := reExtract.FindString(s); m != "" {
		return m + "-01-01"
	}

	return ""
}
