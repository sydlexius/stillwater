package provider

import "testing"

func TestDominantScript(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"latin name", "Rammstein", ScriptLatin},
		{"kanji name", "\u5c3e\u5d0e\u8c4a", ScriptHan},
		{"hiragana name", "\u3072\u3089\u304c\u306a", ScriptHiragana},
		{"katakana name", "\u30d0\u30d3\u30e1\u30bf\u30eb", ScriptKatakana},
		{"hangul name", "\ubc29\ud0c4\uc18c\ub144\ub2e8", ScriptHangul},
		{"cyrillic name", "\u0420\u0430\u043c\u043c\u0448\u0442\u0430\u0439\u043d", ScriptCyrillic},
		{"arabic name", "\u0641\u064a\u0631\u0648\u0632", ScriptArabic},
		{"greek name", "\u039c\u03af\u03ba\u03b7\u03c2", ScriptGreek},
		{"devanagari name", "\u0939\u093f\u0902\u0926\u0940", ScriptDevanagari},
		{"thai name", "\u0e44\u0e17\u0e22", ScriptThai},
		{"hebrew name", "\u05e2\u05d1\u05e8\u05d9\u05ea", ScriptHebrew},
		{"mixed latin-kanji prefers dominant", "X\u5c3e\u5d0e\u8c4a", ScriptHan},
		{"digits and spaces ignored", "  123  ", ScriptUnknown},
		{"empty string", "", ScriptUnknown},
		{"latin with numbers", "Blink-182", ScriptLatin},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DominantScript(tt.input)
			if got != tt.want {
				t.Errorf("DominantScript(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestScriptMatchesAnyLocale(t *testing.T) {
	tests := []struct {
		name   string
		script string
		prefs  []string
		want   bool
	}{
		{"latin matches english", ScriptLatin, []string{"en"}, true},
		{"latin matches german", ScriptLatin, []string{"de"}, true},
		{"han matches japanese", ScriptHan, []string{"ja"}, true},
		{"hiragana matches japanese", ScriptHiragana, []string{"ja"}, true},
		{"katakana matches japanese", ScriptKatakana, []string{"ja"}, true},
		{"han matches chinese", ScriptHan, []string{"zh"}, true},
		{"hangul matches korean", ScriptHangul, []string{"ko"}, true},
		{"cyrillic matches russian", ScriptCyrillic, []string{"ru"}, true},
		{"han does not match english", ScriptHan, []string{"en"}, false},
		{"latin does not match japanese", ScriptLatin, []string{"ja"}, false},
		{"hangul does not match english", ScriptHangul, []string{"en-US", "en"}, false},
		{"unknown always matches", ScriptUnknown, []string{"en"}, true},
		{"second pref matches", ScriptCyrillic, []string{"en", "ru"}, true},
		{"locale tag stripped", ScriptLatin, []string{"en-US", "en-GB"}, true},
		{"unmapped locale is permissive (matches any script)", ScriptLatin, []string{"xx"}, true},
		{"unmapped locale matches cyrillic too", ScriptCyrillic, []string{"mk"}, true},
		{"serbian latin", ScriptLatin, []string{"sr"}, true},
		{"serbian cyrillic", ScriptCyrillic, []string{"sr"}, true},
		{"sr-Latn accepts latin only", ScriptLatin, []string{"sr-Latn"}, true},
		{"sr-Latn rejects cyrillic", ScriptCyrillic, []string{"sr-Latn"}, false},
		{"sr-Cyrl accepts cyrillic only", ScriptCyrillic, []string{"sr-Cyrl"}, true},
		{"sr-Cyrl rejects latin", ScriptLatin, []string{"sr-Cyrl"}, false},
		{"zh-Hant with region accepts han", ScriptHan, []string{"zh-Hant-TW"}, true},
		// A 4-char variant subtag (digit-led) is not a script; the loop
		// must fall through to the base-language lookup, matching Latin
		// from "de". Regression: pre-fix ParseBCP47Script returned "1901"
		// and the caller short-circuited via continue, producing false.
		{"de-1901 variant falls back to base", ScriptLatin, []string{"de-1901"}, true},
		// Private-use prefix "x" in position 1: returns no script and
		// the base "ja" is consulted. Latin does not satisfy ja.
		{"ja-x-latn rejects latin", ScriptLatin, []string{"ja-x-latn"}, false},
		// Same shape but with kanji satisfies ja via the base language.
		{"ja-x-latn accepts han via base", ScriptHan, []string{"ja-x-latn"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ScriptMatchesAnyLocale(tt.script, tt.prefs)
			if got != tt.want {
				t.Errorf("ScriptMatchesAnyLocale(%q, %v) = %v, want %v", tt.script, tt.prefs, got, tt.want)
			}
		})
	}
}

// TestParseBCP47Script locks in the RFC 5646 position + alpha-shape
// constraints on script subtag extraction. Indirect coverage through
// ScriptMatchesAnyLocale / ScriptSatisfiesLocale is not enough because
// those tests only observe the end-to-end boolean; a buggy parser that
// returns an invalid 4-char segment would still produce the correct
// downstream result in most cases thanks to iso15924ToScript filtering.
func TestParseBCP47Script(t *testing.T) {
	tests := []struct {
		tag  string
		want string
	}{
		{"sr-Latn", "latn"},
		{"zh-Hant-TW", "hant"},
		{"zh-hant-tw", "hant"},
		{"en", ""},
		{"en-US", ""},
		{"", ""},
		{"de-1901", ""},         // variant with leading digit, not a script
		{"ja-x-latn", ""},       // "x" in position 1 is a private-use singleton
		{"en-US-Latn", ""},      // script cannot follow region per RFC 5646
		{"de-DE-1901", ""},      // region in position 1, variant at position 2
		{"und-Latn", "latn"},    // undetermined-language + script is valid
		{"  sr-Latn  ", "latn"}, // whitespace trimmed, case lowered
		{"SR-LATN", "latn"},     // uppercase input normalizes to lowercase
	}
	for _, tt := range tests {
		t.Run(tt.tag, func(t *testing.T) {
			if got := ParseBCP47Script(tt.tag); got != tt.want {
				t.Errorf("ParseBCP47Script(%q) = %q, want %q", tt.tag, got, tt.want)
			}
		})
	}
}

// TestScriptSatisfiesLocale covers the stricter variant used by provider
// optimizations (e.g. MusicBrainz alias-fetch skip): unknown scripts and
// empty preferences both return false so callers default to doing the work
// rather than silently skipping.
func TestScriptSatisfiesLocale(t *testing.T) {
	tests := []struct {
		name  string
		input string
		prefs []string
		want  bool
	}{
		{"kanji name with ja pref", "\u5c3e\u5d0e\u8c4a", []string{"ja"}, true},
		{"latin name with ja pref", "Hirokazu Asakura", []string{"ja"}, false},
		{"latin name with en pref", "Hirokazu Asakura", []string{"en"}, true},
		{"latin name with multi pref including ja", "Hirokazu", []string{"ja", "fr", "en"}, true},
		{"kanji name with multi pref including ja", "\u5c3e\u5d0e", []string{"ja", "fr", "en"}, true},
		{"latin name with unmapped locale returns false", "Rammstein", []string{"xx"}, false},
		{"digits only returns false on unknown", "  123  ", []string{"en"}, false},
		{"empty prefs returns false", "Rammstein", nil, false},
		{"empty input returns false", "", []string{"en"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ScriptSatisfiesLocale(tt.input, tt.prefs)
			if got != tt.want {
				t.Errorf("ScriptSatisfiesLocale(%q, %v) = %v, want %v", tt.input, tt.prefs, got, tt.want)
			}
		})
	}
}

func TestLocaleExpectsOnlyNonLatinScript(t *testing.T) {
	tests := []struct {
		name string
		tag  string
		want bool
	}{
		// Latin-family locales: Latin is expected, so a Latin alias can still
		// improve the canonical. Skip must NOT be authorized.
		{"english base", "en", false},
		{"english region", "en-US", false},
		{"german", "de", false},
		{"french", "fr", false},
		{"spanish", "es", false},

		// Non-Latin-only locales: skip IS authorized.
		{"japanese", "ja", true},
		{"japanese region", "ja-JP", true},
		{"chinese", "zh", true},
		{"korean", "ko", true},
		{"russian", "ru", true},
		{"arabic", "ar", true},
		{"hebrew", "he", true},
		{"greek", "el", true},
		{"hindi", "hi", true},
		{"thai", "th", true},

		// Mixed-script locale: Latin IS in the allowed set, so skip is NOT
		// authorized (conservative fetch protects typography improvements).
		{"serbian mixed", "sr", false},

		// Explicit BCP-47 script subtag takes precedence over base language.
		{"serbian latin subtag", "sr-Latn", false},
		{"serbian cyrillic subtag", "sr-Cyrl", true},
		{"chinese traditional subtag", "zh-Hant", true},
		{"chinese simplified subtag", "zh-Hans", true},
		{"english explicit latin subtag", "en-Latn", false},

		// Empty and unmapped inputs return false (no positive evidence).
		{"empty tag", "", false},
		{"whitespace only", "   ", false},
		{"unmapped locale", "xx", false},
		{"unmapped with region", "xx-YY", false},

		// Script subtag that is not 4 alpha chars is ignored (per
		// ParseBCP47Script contract). "de-1901" falls through to base "de"
		// which is Latin -> false.
		{"german variant 1901", "de-1901", false},

		// Unmapped script subtag (exists in BCP-47 but not in our
		// iso15924ToScript table). Conservative: return false.
		{"unmapped script subtag", "en-Zzzz", false},

		// Case normalization: base-language branch lowercases before map
		// lookup, so uppercase input must resolve identically to lowercase.
		{"uppercase japanese base", "JA", true},
		{"uppercase english region", "EN-US", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := LocaleExpectsOnlyNonLatinScript(tt.tag)
			if got != tt.want {
				t.Errorf("LocaleExpectsOnlyNonLatinScript(%q) = %v, want %v", tt.tag, got, tt.want)
			}
		})
	}
}
