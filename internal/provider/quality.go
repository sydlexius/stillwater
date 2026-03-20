package provider

import "strings"

// minBiographyLength is the minimum byte length for a biography to be
// considered useful. Values shorter than this are treated as junk and rejected
// by the orchestrator so the next provider in the priority chain is tried.
// 50 bytes is roughly one sentence of ASCII text -- enough to distinguish
// real content from placeholder stubs like "?" or "N/A".
// len() measures bytes, not runes; for non-Latin scripts this is intentional
// since multi-byte characters convey more meaning per character.
const minBiographyLength = 50

// junkPatterns are case-insensitive match strings that providers sometimes
// return as placeholder metadata. When a provider biography (after trimming)
// matches one of these (case-insensitive), it is rejected regardless of length.
var junkPatterns = []string{
	"?", "??", "???",
	"n/a", "na",
	"unknown",
	"tbd", "tba",
	"-", "--", "---",
	".", "..", "...",
	"none",
	"no description available",
	"no description available.",
	"no biography available",
	"no biography available.",
}

// defaultMinFieldLength is the minimum byte length for generic text fields
// (excluding biography, which uses minBiographyLength). Single-character
// values like "?" or "-" are nearly always placeholders.
const defaultMinFieldLength = 2

// IsJunkValue reports whether a metadata field value is empty, matches a
// known placeholder pattern, or is below the field-specific minimum length.
// Field names control the minimum length threshold:
//   - "biography": 50 bytes (roughly one sentence)
//   - "genres", "styles", "moods": no minimum (single words are valid)
//   - all others: 2 bytes
func IsJunkValue(field, value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return true
	}

	// Check case-insensitive placeholder patterns.
	for _, pattern := range junkPatterns {
		if strings.EqualFold(trimmed, pattern) {
			return true
		}
	}

	// Field-specific minimum length thresholds.
	switch field {
	case "biography":
		return len(trimmed) < minBiographyLength
	case "genres", "styles", "moods":
		return false // single-word values are valid for list fields
	default:
		return len(trimmed) < defaultMinFieldLength
	}
}

// IsJunkBiography reports whether a biography value is too short or matches a
// known placeholder pattern. The orchestrator uses this to skip junk values and
// try the next provider in the priority chain.
func IsJunkBiography(bio string) bool {
	return IsJunkValue("biography", bio)
}
