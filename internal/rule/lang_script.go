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
	best := scriptUnknown
	bestN := 0
	for name, n := range counts {
		if n > bestN {
			best = name
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

// scriptMatchesAnyLocale returns true when the given script is expected for
// at least one of the BCP-47 locale tags in prefs.
func scriptMatchesAnyLocale(script string, prefs []string) bool {
	if script == scriptUnknown {
		return true
	}
	for _, p := range prefs {
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
