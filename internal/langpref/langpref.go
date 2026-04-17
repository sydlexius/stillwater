// Package langpref owns the user's ordered metadata-language preference
// list. It provides validation, canonical normalization, and a small
// repository API over the user_preferences table so providers, rule
// engines, and UI can all consume a single source of truth.
//
// The stored format is a JSON array of BCP 47 language tags (for example
// ["en-GB", "en", "ja"]). Order is meaningful: earlier entries are
// higher-priority. Duplicates (case-insensitive) are rejected at the
// validation boundary.
//
// The package intentionally does not depend on any other internal package
// so it can be imported by providers, the API layer, and the settings
// import/export layer without cycles.
package langpref

import (
	"encoding/json"
	"strings"
)

// PreferenceKey is the row key used in the user_preferences table.
const PreferenceKey = "metadata_languages"

// DefaultJSON is the canonical JSON value returned when no preference is
// stored for a user, or when a stored value is unparsable. Keep this in
// sync with DefaultTags.
const DefaultJSON = `["en"]`

// MaxEntries caps how many language tags a user can store. The limit is
// intentionally generous; it exists purely to bound input at the API
// boundary.
const MaxEntries = 20

// MaxTagLength bounds a single BCP 47 tag including extended and
// private-use subtags. 100 bytes is well above the longest valid tag.
const MaxTagLength = 100

// DefaultTags returns a fresh copy of the default tag list. Callers may
// mutate the returned slice without affecting other callers.
func DefaultTags() []string {
	return []string{"en"}
}

// Validate checks whether tags is a well-formed ordered language
// preference list and returns a canonically normalized copy. The returned
// slice preserves input order. Rules:
//
//  1. At least one tag, at most MaxEntries.
//  2. Every tag must be a syntactically valid BCP 47 language tag.
//  3. Tags are canonicalized (lowercase language, Title-case script,
//     UPPERCASE region).
//  4. Duplicates (comparing canonical lowercase form) are rejected.
//
// Validate never mutates the input slice.
func Validate(tags []string) ([]string, bool) {
	if len(tags) == 0 || len(tags) > MaxEntries {
		return nil, false
	}
	seen := make(map[string]bool, len(tags))
	normalized := make([]string, 0, len(tags))
	for _, tag := range tags {
		if tag == "" || !isValidLanguageTag(tag) {
			return nil, false
		}
		canon := normalizeTag(tag)
		lower := strings.ToLower(canon)
		if seen[lower] {
			return nil, false
		}
		seen[lower] = true
		normalized = append(normalized, canon)
	}
	return normalized, true
}

// ValidateJSON parses raw as a JSON array of language tags, validates,
// and returns the canonical JSON encoding plus the parsed slice. The
// returned JSON uses canonical casing and preserves the input order.
func ValidateJSON(raw string) (string, []string, bool) {
	var tags []string
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		return "", nil, false
	}
	canonical, ok := Validate(tags)
	if !ok {
		return "", nil, false
	}
	enc, err := json.Marshal(canonical)
	if err != nil {
		return "", nil, false
	}
	return string(enc), canonical, true
}

// NormalizeJSON is the read-path counterpart to ValidateJSON. It returns
// the canonical JSON form if raw validates, otherwise DefaultJSON. Use
// this when reading persisted values so stale or manually edited rows
// always return a clean value to callers.
func NormalizeJSON(raw string) string {
	canonical, _, ok := ValidateJSON(raw)
	if !ok {
		return DefaultJSON
	}
	return canonical
}

// ParseJSON parses a persisted JSON array into a slice of tags. Returns
// nil on parse failure. Callers that need validation should use
// ValidateJSON instead.
func ParseJSON(raw string) []string {
	if raw == "" {
		return nil
	}
	var tags []string
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		return nil
	}
	return tags
}

// EncodeJSON encodes tags as a canonical JSON array. The caller is
// responsible for passing an already-validated slice; if validation is
// needed, use ValidateJSON instead.
func EncodeJSON(tags []string) (string, error) {
	b, err := json.Marshal(tags)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// isValidLanguageTag performs a lightweight structural check against
// BCP 47 tags such as "en", "en-GB", or "zh-Hant-TW". The primary
// language subtag must be 2-3 ASCII letters (ISO 639). Subsequent
// subtags are 1-8 alphanumeric characters separated by hyphens.
func isValidLanguageTag(s string) bool {
	if len(s) == 0 || len(s) > MaxTagLength {
		return false
	}
	parts := strings.Split(s, "-")
	primary := parts[0]
	if len(primary) < 2 || len(primary) > 3 {
		return false
	}
	for _, c := range primary {
		if !isASCIILetter(c) {
			return false
		}
	}
	for _, p := range parts[1:] {
		if len(p) == 0 || len(p) > 8 {
			return false
		}
		for _, c := range p {
			if !isASCIILetter(c) && !isASCIIDigit(c) {
				return false
			}
		}
	}
	return true
}

func isASCIILetter(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isASCIIDigit(c rune) bool {
	return c >= '0' && c <= '9'
}

// normalizeTag applies BCP 47 canonical casing: lowercase language,
// Title-case script (4 letters), uppercase region (2 letters). For
// example: "EN-gb" becomes "en-GB"; "zh-hant-tw" becomes "zh-Hant-TW".
//
// Extension subtags (those introduced by a singleton letter other than
// "x") and private-use subtags (introduced by "x") preserve their
// internal subtag casing per convention because their internal
// structure follows different rules.
func normalizeTag(tag string) string {
	parts := strings.Split(tag, "-")
	// Primary language subtag: always lowercase.
	parts[0] = strings.ToLower(parts[0])
	for i := 1; i < len(parts); i++ {
		p := parts[i]
		// A single-letter subtag (a-w, y) is a BCP 47 singleton that
		// starts an extension sequence. Stop applying casing rules from
		// here onward because extension subtag semantics are
		// extension-defined.
		if len(p) == 1 && isASCIILetter(rune(p[0])) && p[0] != 'x' && p[0] != 'X' {
			parts[i] = strings.ToLower(p)
			break
		}
		// Private-use prefix "x" also stops structural casing.
		if (p[0] == 'x' || p[0] == 'X') && len(p) == 1 {
			parts[i] = "x"
			break
		}
		switch {
		case len(p) == 4:
			// Script subtag: Title-case (e.g. "Hant", "Latn").
			parts[i] = strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
		case len(p) == 2 && isASCIILetter(rune(p[0])):
			// Region subtag: uppercase (e.g. "GB", "TW").
			parts[i] = strings.ToUpper(p)
		default:
			// Variant or other subtag: lowercase by convention.
			parts[i] = strings.ToLower(p)
		}
	}
	return strings.Join(parts, "-")
}
