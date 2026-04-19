package provider

import (
	"strings"
	"unicode"
)

// Script constants identify Unicode scripts used in artist names and other
// metadata strings. Values are lowercase so they compose naturally into log
// messages.
const (
	ScriptLatin      = "latin"
	ScriptHan        = "han"
	ScriptHiragana   = "hiragana"
	ScriptKatakana   = "katakana"
	ScriptHangul     = "hangul"
	ScriptCyrillic   = "cyrillic"
	ScriptArabic     = "arabic"
	ScriptHebrew     = "hebrew"
	ScriptGreek      = "greek"
	ScriptDevanagari = "devanagari"
	ScriptThai       = "thai"
	ScriptUnknown    = "unknown"
)

var scriptTables = []struct {
	name  string
	table *unicode.RangeTable
}{
	{ScriptLatin, unicode.Latin},
	{ScriptHan, unicode.Han},
	{ScriptHiragana, unicode.Hiragana},
	{ScriptKatakana, unicode.Katakana},
	{ScriptHangul, unicode.Hangul},
	{ScriptCyrillic, unicode.Cyrillic},
	{ScriptArabic, unicode.Arabic},
	{ScriptHebrew, unicode.Hebrew},
	{ScriptGreek, unicode.Greek},
	{ScriptDevanagari, unicode.Devanagari},
	{ScriptThai, unicode.Thai},
}

// DominantScript returns the Unicode script that accounts for the most
// letter/symbol runes in s. Whitespace, digits, and punctuation are ignored.
// Returns ScriptUnknown when no classifiable runes are found.
func DominantScript(s string) string {
	counts := make(map[string]int, len(scriptTables))
	total := 0
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsDigit(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			continue
		}
		for _, st := range scriptTables {
			if unicode.Is(st.table, r) {
				counts[st.name]++
				total++
				break
			}
		}
	}
	if total == 0 {
		return ScriptUnknown
	}
	// Iterate scriptTables (fixed order) instead of the counts map (random
	// order) so mixed-script ties resolve deterministically to the script
	// listed first in scriptTables.
	best := ScriptUnknown
	bestN := 0
	for _, st := range scriptTables {
		if n := counts[st.name]; n > bestN {
			best = st.name
			bestN = n
		}
	}
	return best
}

// localeScripts maps a BCP-47 language prefix to the set of scripts that
// are expected for text in that language. Japanese is special: names can be
// in Han (kanji), Hiragana, or Katakana, all of which are valid.
var localeScripts = map[string][]string{
	"en": {ScriptLatin},
	"es": {ScriptLatin},
	"de": {ScriptLatin},
	"fr": {ScriptLatin},
	"it": {ScriptLatin},
	"pt": {ScriptLatin},
	"nl": {ScriptLatin},
	"sv": {ScriptLatin},
	"no": {ScriptLatin},
	"da": {ScriptLatin},
	"fi": {ScriptLatin},
	"pl": {ScriptLatin},
	"cs": {ScriptLatin},
	"sk": {ScriptLatin},
	"hu": {ScriptLatin},
	"ro": {ScriptLatin},
	"tr": {ScriptLatin},
	"id": {ScriptLatin},
	"ms": {ScriptLatin},
	"vi": {ScriptLatin},
	"tl": {ScriptLatin},
	"ja": {ScriptHan, ScriptHiragana, ScriptKatakana},
	"zh": {ScriptHan},
	"ko": {ScriptHangul, ScriptHan},
	"ru": {ScriptCyrillic},
	"uk": {ScriptCyrillic},
	"be": {ScriptCyrillic},
	"bg": {ScriptCyrillic},
	"sr": {ScriptCyrillic, ScriptLatin},
	"ar": {ScriptArabic},
	"fa": {ScriptArabic},
	"he": {ScriptHebrew},
	"el": {ScriptGreek},
	"hi": {ScriptDevanagari},
	"th": {ScriptThai},
}

// iso15924ToScript maps ISO 15924 script codes (as used in BCP-47 script
// subtags like "sr-Latn" or "zh-Hant") to our internal script constants.
// Only scripts we detect via DominantScript are included.
var iso15924ToScript = map[string]string{
	"latn": ScriptLatin,
	"hani": ScriptHan,
	"hans": ScriptHan,
	"hant": ScriptHan,
	"hira": ScriptHiragana,
	"kana": ScriptKatakana,
	"hang": ScriptHangul,
	"kore": ScriptHangul,
	"cyrl": ScriptCyrillic,
	"arab": ScriptArabic,
	"hebr": ScriptHebrew,
	"grek": ScriptGreek,
	"deva": ScriptDevanagari,
	"thai": ScriptThai,
}

// ParseBCP47Script extracts the explicit ISO 15924 script subtag from a
// BCP-47 tag (e.g. "sr-Latn" -> "latn", "zh-Hant-TW" -> "hant"). Returns
// the empty string when no script subtag is present.
//
// Per RFC 5646, the script subtag has two defining properties:
//
//  1. Position: it must immediately follow the language/extlang subtag.
//     A 4-character segment later in the tag is a region (rare) or a
//     variant, not a script.
//  2. Shape: it must be exactly 4 alphabetic characters. A 4-digit-or-
//     leading-digit variant like "1901" (in "de-1901", the 1901 German
//     orthography variant) is position-1-valid but is NOT a script.
//
// Enforcing both together means callers that fall back to the base
// language on empty result (ScriptMatchesAnyLocale, ScriptSatisfiesLocale)
// correctly handle tags with variants, private-use subtags ("ja-x-latn"),
// or extensions without a phantom script match short-circuiting the loop.
func ParseBCP47Script(tag string) string {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(tag)), "-")
	if len(parts) < 2 {
		return ""
	}
	p := parts[1]
	if len(p) != 4 {
		return ""
	}
	for _, c := range p {
		if c < 'a' || c > 'z' {
			return ""
		}
	}
	return p
}

// ScriptMatchesAnyLocale returns true when the given script is expected for
// at least one of the BCP-47 locale tags in prefs. Explicit script subtags
// (sr-Latn, zh-Hant, etc.) are honored directly; otherwise the base language
// maps through localeScripts. ScriptUnknown always matches so callers that
// validate text (e.g. rule checkers) do not flag ambiguous strings.
func ScriptMatchesAnyLocale(script string, prefs []string) bool {
	if script == ScriptUnknown {
		return true
	}
	for _, p := range prefs {
		// Honor an explicit BCP-47 script subtag first: "sr-Latn" must
		// mean Latin only, not Serbia's default [cyrillic, latin] set.
		if sub := ParseBCP47Script(p); sub != "" {
			if mapped, ok := iso15924ToScript[sub]; ok && mapped == script {
				return true
			}
			continue
		}
		lang := strings.SplitN(strings.ToLower(strings.TrimSpace(p)), "-", 2)[0]
		allowed, ok := localeScripts[lang]
		if !ok {
			// Unmapped locales are treated as permissive: any script matches.
			// This avoids false positives for languages not in the map (e.g.
			// Macedonian, Mongolian, Tajik use Cyrillic but are not listed).
			return true
		}
		for _, s := range allowed {
			if script == s {
				return true
			}
		}
	}
	return false
}

// ScriptSatisfiesLocale reports whether the dominant script of s is a
// positive match for at least one locale in prefs. Unlike
// ScriptMatchesAnyLocale, this returns false for unclassifiable input and for
// unmapped locales -- the caller gets a clear "we know the script and it
// matches" signal, which is what optimizations like alias-fetch skipping
// need. Empty prefs also returns false so callers never skip work without
// an explicit preference.
func ScriptSatisfiesLocale(s string, prefs []string) bool {
	if len(prefs) == 0 {
		return false
	}
	script := DominantScript(s)
	if script == ScriptUnknown {
		return false
	}
	for _, p := range prefs {
		// Honor an explicit BCP-47 script subtag before falling back to the
		// language-to-scripts map; "sr-Latn" is Latin only, regardless of
		// what the localeScripts entry for "sr" would permit.
		if sub := ParseBCP47Script(p); sub != "" {
			if mapped, ok := iso15924ToScript[sub]; ok && mapped == script {
				return true
			}
			continue
		}
		lang := strings.SplitN(strings.ToLower(strings.TrimSpace(p)), "-", 2)[0]
		allowed, ok := localeScripts[lang]
		if !ok {
			// Unmapped locales give no positive evidence; skip without
			// claiming a match. This is the strict-mode divergence from
			// ScriptMatchesAnyLocale's permissive fallback.
			continue
		}
		for _, candidate := range allowed {
			if script == candidate {
				return true
			}
		}
	}
	return false
}
