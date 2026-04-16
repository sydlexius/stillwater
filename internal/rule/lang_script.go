package rule

import (
	"strings"
	"unicode"
)

const (
	scriptLatin      = "latin"
	scriptHan        = "han"
	scriptHiragana   = "hiragana"
	scriptKatakana   = "katakana"
	scriptHangul     = "hangul"
	scriptCyrillic   = "cyrillic"
	scriptArabic     = "arabic"
	scriptHebrew     = "hebrew"
	scriptGreek      = "greek"
	scriptDevanagari = "devanagari"
	scriptThai       = "thai"
	scriptUnknown    = "unknown"
)

var scriptTables = []struct {
	name  string
	table *unicode.RangeTable
}{
	{scriptLatin, unicode.Latin},
	{scriptHan, unicode.Han},
	{scriptHiragana, unicode.Hiragana},
	{scriptKatakana, unicode.Katakana},
	{scriptHangul, unicode.Hangul},
	{scriptCyrillic, unicode.Cyrillic},
	{scriptArabic, unicode.Arabic},
	{scriptHebrew, unicode.Hebrew},
	{scriptGreek, unicode.Greek},
	{scriptDevanagari, unicode.Devanagari},
	{scriptThai, unicode.Thai},
}

// dominantScript returns the Unicode script that accounts for the most
// letter/symbol runes in s. Whitespace, digits, and punctuation are ignored.
// Returns scriptUnknown when no classifiable runes are found.
func dominantScript(s string) string {
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
		return scriptUnknown
	}
	// Iterate scriptTables (fixed order) instead of the counts map (random
	// order) so mixed-script ties resolve deterministically to the script
	// listed first in scriptTables.
	best := scriptUnknown
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
	"en": {scriptLatin},
	"es": {scriptLatin},
	"de": {scriptLatin},
	"fr": {scriptLatin},
	"it": {scriptLatin},
	"pt": {scriptLatin},
	"nl": {scriptLatin},
	"sv": {scriptLatin},
	"no": {scriptLatin},
	"da": {scriptLatin},
	"fi": {scriptLatin},
	"pl": {scriptLatin},
	"cs": {scriptLatin},
	"sk": {scriptLatin},
	"hu": {scriptLatin},
	"ro": {scriptLatin},
	"tr": {scriptLatin},
	"id": {scriptLatin},
	"ms": {scriptLatin},
	"vi": {scriptLatin},
	"tl": {scriptLatin},
	"ja": {scriptHan, scriptHiragana, scriptKatakana},
	"zh": {scriptHan},
	"ko": {scriptHangul, scriptHan},
	"ru": {scriptCyrillic},
	"uk": {scriptCyrillic},
	"be": {scriptCyrillic},
	"bg": {scriptCyrillic},
	"sr": {scriptCyrillic, scriptLatin},
	"ar": {scriptArabic},
	"fa": {scriptArabic},
	"he": {scriptHebrew},
	"el": {scriptGreek},
	"hi": {scriptDevanagari},
	"th": {scriptThai},
}

// iso15924ToScript maps ISO 15924 script codes (as used in BCP-47 script
// subtags like "sr-Latn" or "zh-Hant") to our internal script constants.
// Only scripts we detect via dominantScript are included.
var iso15924ToScript = map[string]string{
	"latn": scriptLatin,
	"hani": scriptHan,
	"hans": scriptHan,
	"hant": scriptHan,
	"hira": scriptHiragana,
	"kana": scriptKatakana,
	"hang": scriptHangul,
	"kore": scriptHangul,
	"cyrl": scriptCyrillic,
	"arab": scriptArabic,
	"hebr": scriptHebrew,
	"grek": scriptGreek,
	"deva": scriptDevanagari,
	"thai": scriptThai,
}

// parseBCP47Script extracts the explicit ISO 15924 script subtag from a
// BCP-47 tag (e.g. "sr-Latn" -> "latn", "zh-Hant-TW" -> "hant"). Returns
// the empty string when no script subtag is present. The script subtag is
// always a 4-letter segment; language is 2-3 letters and region is 2
// letters or 3 digits, so the 4-letter position after the language is
// unambiguous.
func parseBCP47Script(tag string) string {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(tag)), "-")
	for _, p := range parts[1:] {
		if len(p) == 4 {
			return p
		}
	}
	return ""
}

// scriptMatchesAnyLocale returns true when the given script is expected for
// at least one of the BCP-47 locale tags in prefs. Explicit script subtags
// (sr-Latn, zh-Hant, etc.) are honored directly; otherwise the base language
// maps through localeScripts.
func scriptMatchesAnyLocale(script string, prefs []string) bool {
	if script == scriptUnknown {
		return true
	}
	for _, p := range prefs {
		// Honor an explicit BCP-47 script subtag first: "sr-Latn" must
		// mean Latin only, not Serbia's default [cyrillic, latin] set.
		if sub := parseBCP47Script(p); sub != "" {
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
