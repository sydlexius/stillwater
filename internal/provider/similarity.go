package provider

import (
	"strings"
	"unicode"
)

// DefaultNameSimilarityThreshold is the default minimum score (0-100) below
// which a search result is considered a name mismatch. Providers that perform
// name-based lookups compare the returned artist name against the search term
// and reject results that score below this threshold.
const DefaultNameSimilarityThreshold = 60

// NameSimilarity returns a 0-100 score indicating how similar two artist names
// are. The comparison is case-insensitive and strips common prefixes like "The".
func NameSimilarity(a, b string) int {
	// Fast path: case-insensitive exact match before normalization.
	// Handles punctuation-heavy names like "!!!" that normalize to empty.
	// Guard: whitespace-only ("   ") must not match empty ("") via TrimSpace.
	ta, tb := strings.TrimSpace(a), strings.TrimSpace(b)
	if strings.EqualFold(ta, tb) && (ta != "" || (a == "" && b == "")) {
		return 100
	}
	a = NormalizeName(a)
	b = NormalizeName(b)
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 100
	}
	ra, rb := []rune(a), []rune(b)
	maxLen := len(ra)
	if len(rb) > maxLen {
		maxLen = len(rb)
	}
	dist := LevenshteinRunes(ra, rb)
	if dist >= maxLen {
		return 0
	}
	return 100 - (dist*100)/maxLen
}

// NormalizeName lowercases, strips "the " prefix, and removes punctuation and
// symbols (keeping letters, digits, and spaces) for comparison purposes.
func NormalizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' {
			b.WriteRune(r)
		}
	}
	s = strings.TrimSpace(b.String())
	// Strip leading "the " only when the cleaned remainder is a distinct name,
	// not another article (e.g., "The The" is a real band, not "The" + article).
	if after, found := strings.CutPrefix(s, "the "); found {
		after = strings.TrimSpace(after)
		if after != "" && after != "the" {
			s = after
		}
	}
	return s
}

// LevenshteinRunes computes the Levenshtein edit distance between two rune
// slices. Operating on runes ensures multi-byte Unicode characters (accented
// letters, CJK, Cyrillic) are counted as single characters.
func LevenshteinRunes(a, b []rune) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	// Use a single-row DP approach with reused row buffers.
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			ins := curr[j-1] + 1
			del := prev[j] + 1
			sub := prev[j-1] + cost
			curr[j] = ins
			if del < curr[j] {
				curr[j] = del
			}
			if sub < curr[j] {
				curr[j] = sub
			}
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}
