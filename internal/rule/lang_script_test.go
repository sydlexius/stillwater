package rule

import "testing"

func TestDominantScript(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"latin name", "Rammstein", scriptLatin},
		{"kanji name", "\u5c3e\u5d0e\u8c4a", scriptHan},
		{"hiragana name", "\u3072\u3089\u304c\u306a", scriptHiragana},
		{"katakana name", "\u30d0\u30d3\u30e1\u30bf\u30eb", scriptKatakana},
		{"hangul name", "\ubc29\ud0c4\uc18c\ub144\ub2e8", scriptHangul},
		{"cyrillic name", "\u0420\u0430\u043c\u043c\u0448\u0442\u0430\u0439\u043d", scriptCyrillic},
		{"arabic name", "\u0641\u064a\u0631\u0648\u0632", scriptArabic},
		{"greek name", "\u039c\u03af\u03ba\u03b7\u03c2", scriptGreek},
		{"devanagari name", "\u0939\u093f\u0902\u0926\u0940", scriptDevanagari},
		{"thai name", "\u0e44\u0e17\u0e22", scriptThai},
		{"hebrew name", "\u05e2\u05d1\u05e8\u05d9\u05ea", scriptHebrew},
		{"mixed latin-kanji prefers dominant", "X\u5c3e\u5d0e\u8c4a", scriptHan},
		{"digits and spaces ignored", "  123  ", scriptUnknown},
		{"empty string", "", scriptUnknown},
		{"latin with numbers", "Blink-182", scriptLatin},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dominantScript(tt.input)
			if got != tt.want {
				t.Errorf("dominantScript(%q) = %q, want %q", tt.input, got, tt.want)
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
		{"latin matches english", scriptLatin, []string{"en"}, true},
		{"latin matches german", scriptLatin, []string{"de"}, true},
		{"han matches japanese", scriptHan, []string{"ja"}, true},
		{"hiragana matches japanese", scriptHiragana, []string{"ja"}, true},
		{"katakana matches japanese", scriptKatakana, []string{"ja"}, true},
		{"han matches chinese", scriptHan, []string{"zh"}, true},
		{"hangul matches korean", scriptHangul, []string{"ko"}, true},
		{"cyrillic matches russian", scriptCyrillic, []string{"ru"}, true},
		{"han does not match english", scriptHan, []string{"en"}, false},
		{"latin does not match japanese", scriptLatin, []string{"ja"}, false},
		{"hangul does not match english", scriptHangul, []string{"en-US", "en"}, false},
		{"unknown always matches", scriptUnknown, []string{"en"}, true},
		{"second pref matches", scriptCyrillic, []string{"en", "ru"}, true},
		{"locale tag stripped", scriptLatin, []string{"en-US", "en-GB"}, true},
		{"unmapped locale is permissive (matches any script)", scriptLatin, []string{"xx"}, true},
		{"unmapped locale matches cyrillic too", scriptCyrillic, []string{"mk"}, true},
		{"serbian latin", scriptLatin, []string{"sr"}, true},
		{"serbian cyrillic", scriptCyrillic, []string{"sr"}, true},
		{"sr-Latn accepts latin only", scriptLatin, []string{"sr-Latn"}, true},
		{"sr-Latn rejects cyrillic", scriptCyrillic, []string{"sr-Latn"}, false},
		{"sr-Cyrl accepts cyrillic only", scriptCyrillic, []string{"sr-Cyrl"}, true},
		{"sr-Cyrl rejects latin", scriptLatin, []string{"sr-Cyrl"}, false},
		{"zh-Hant with region accepts han", scriptHan, []string{"zh-Hant-TW"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scriptMatchesAnyLocale(tt.script, tt.prefs)
			if got != tt.want {
				t.Errorf("scriptMatchesAnyLocale(%q, %v) = %v, want %v", tt.script, tt.prefs, got, tt.want)
			}
		})
	}
}
