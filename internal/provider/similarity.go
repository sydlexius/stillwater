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

// BestNameSimilarity returns the highest NameSimilarity score between query
// and any of the candidate names, ignoring empty ones.
//
// It exists because an artist has more than one true name. MusicBrainz carries
// a primary name, a sort-name, and any number of aliases, and the operator's
// folder may be named after any of them: "Barlow Girl" for BarlowGirl,
// "Beatles, The" for The Beatles, or a Latin-script alias for an artist whose
// primary name is in another script. Scoring only the primary name reports
// those as unrelated, which on the disambiguation screen means the right
// candidate ranks below wrong ones.
//
// Taking the MAXIMUM rather than an average is deliberate: matching any one of
// an artist's names is positive evidence, and an artist with forty aliases
// must not be penalized for the thirty-nine that do not match the query.
//
// Empty candidates are SKIPPED rather than scored, which is the one place this
// diverges from NameSimilarity: that function returns 100 for ("", "") because
// empty equals empty, but an empty candidate here means "this artist has no
// sort-name" or "this alias carries no name", and treating missing data as a
// perfect match would rank an incomplete candidate above a genuinely matching
// one. So this is not a drop-in replacement for every NameSimilarity call --
// only for those comparing a query against real names.
func BestNameSimilarity(query string, candidates ...string) int {
	best := 0
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if s := NameSimilarity(query, c); s > best {
			best = s
			if best == 100 {
				return best // cannot be beaten; skip the rest
			}
		}
	}
	return best
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
