package provider

import (
	"context"
	"strings"
)

// ctxKeyMetadataLanguages is the context key for the user's ordered metadata
// language preferences. The value is a []string of BCP 47 language tags
// (e.g. ["en-GB", "en", "ja"]).
type ctxKeyMetadataLanguages struct{}

// WithMetadataLanguages returns a child context carrying the user's ordered
// metadata language preferences.
func WithMetadataLanguages(ctx context.Context, langs []string) context.Context {
	return context.WithValue(ctx, ctxKeyMetadataLanguages{}, langs)
}

// MetadataLanguages retrieves the ordered metadata language preferences from
// the context. Returns nil when no preference has been set.
func MetadataLanguages(ctx context.Context) []string {
	langs, _ := ctx.Value(ctxKeyMetadataLanguages{}).([]string)
	return langs
}

// MatchLanguagePreference scores a locale string against the user's ordered
// preference list. Lower scores are better. Returns -1 if there is no match.
//
// Matching rules:
//  1. Exact match (case-insensitive): score = index
//  2. Parent language match (e.g. locale "en-GB" matches preference "en"): score = index + 0.5
//     (represented as index*2 + 1 in integer math to keep things simple)
//  3. Preference with dialect matches locale base (e.g. pref "en-GB", locale "en"): same rule
//
// The caller should pick the candidate with the lowest non-negative score.
func MatchLanguagePreference(locale string, prefs []string) int {
	if len(prefs) == 0 || locale == "" {
		return -1
	}

	locale = strings.ToLower(locale)
	localeBase := languageBase(locale)

	best := -1
	for i, pref := range prefs {
		pref = strings.ToLower(pref)
		prefBase := languageBase(pref)

		score := -1
		switch {
		case locale == pref:
			// Exact match: best possible score for this position.
			score = i * 2
		case localeBase == prefBase:
			// Base language matches (e.g. "en-GB" vs "en", or "en" vs "en-US").
			score = i*2 + 1
		}

		if score >= 0 && (best < 0 || score < best) {
			best = score
		}
	}
	return best
}

// SelectLocalizedBiography walks the user's preference list and returns the
// first non-empty biography from the candidates map. Keys in candidates are
// ISO 639-1 language codes (e.g. "en", "de", "ja"). If no preference matches,
// returns the fallback value.
func SelectLocalizedBiography(candidates map[string]string, prefs []string, fallback string) string {
	if len(prefs) == 0 {
		return fallback
	}
	for _, pref := range prefs {
		base := languageBase(strings.ToLower(pref))
		if bio, ok := candidates[base]; ok && bio != "" {
			return bio
		}
	}
	return fallback
}

// languageBase returns the primary language subtag from a BCP 47 tag.
// e.g. "en-GB" -> "en", "ja" -> "ja".
func languageBase(tag string) string {
	if idx := strings.IndexByte(tag, '-'); idx > 0 {
		return tag[:idx]
	}
	return tag
}
