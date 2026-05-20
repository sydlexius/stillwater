package provider

import (
	"context"
	"testing"
)

func TestWithMetadataLanguages(t *testing.T) {
	ctx := context.Background()

	// No languages set.
	if langs := MetadataLanguages(ctx); langs != nil {
		t.Errorf("expected nil, got %v", langs)
	}

	// Set languages and retrieve.
	langs := []string{"en-GB", "en", "ja"}
	ctx = WithMetadataLanguages(ctx, langs)

	// Mutate the original slice to verify defensive copy.
	langs[0] = "fr"
	got := MetadataLanguages(ctx)
	if got[0] != "en-GB" {
		t.Fatalf("expected defensive copy to preserve original value, got %q", got[0])
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 languages, got %d", len(got))
	}
	expected := []string{"en-GB", "en", "ja"}
	for i, want := range expected {
		if got[i] != want {
			t.Errorf("index %d: expected %q, got %q", i, want, got[i])
		}
	}
}

func TestMatchLanguagePreference(t *testing.T) {
	tests := []struct {
		name   string
		locale string
		prefs  []string
		want   int
	}{
		{name: "empty prefs", locale: "en", prefs: nil, want: -1},
		{name: "empty locale", locale: "", prefs: []string{"en"}, want: -1},
		{name: "exact match first", locale: "en", prefs: []string{"en", "ja"}, want: 0},
		{name: "exact match second", locale: "ja", prefs: []string{"en", "ja"}, want: 2},
		{name: "parent match", locale: "en-GB", prefs: []string{"en"}, want: 1},
		{name: "exact beats parent", locale: "en-GB", prefs: []string{"en", "en-GB"}, want: 1},
		{name: "case insensitive", locale: "EN-gb", prefs: []string{"en-GB"}, want: 0},
		{name: "no match", locale: "fr", prefs: []string{"en", "ja"}, want: -1},
		{name: "dialect pref matches base locale", locale: "en", prefs: []string{"en-GB"}, want: 1},
		{name: "unrelated dialect", locale: "fr-CA", prefs: []string{"en", "fr"}, want: 3},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MatchLanguagePreference(tc.locale, tc.prefs)
			if got != tc.want {
				t.Errorf("MatchLanguagePreference(%q, %v) = %d, want %d", tc.locale, tc.prefs, got, tc.want)
			}
		})
	}
}

func TestSelectLocalizedBiography(t *testing.T) {
	candidates := map[string]string{
		"en": "English biography",
		"ja": "Japanese biography",
		"fr": "French biography",
	}

	tests := []struct {
		name     string
		prefs    []string
		fallback string
		want     string
	}{
		{name: "empty prefs uses fallback", prefs: nil, fallback: "default", want: "default"},
		{name: "first pref match", prefs: []string{"ja", "en"}, fallback: "default", want: "Japanese biography"},
		{name: "dialect falls back to base", prefs: []string{"en-GB", "ja"}, fallback: "default", want: "English biography"},
		{name: "no match uses fallback", prefs: []string{"de", "ko"}, fallback: "default", want: "default"},
		{name: "skip empty candidate", prefs: []string{"en"}, fallback: "default", want: "English biography"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SelectLocalizedBiography(candidates, tc.prefs, tc.fallback)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSelectLocalizedBiography_SkipsEmpty(t *testing.T) {
	candidates := map[string]string{
		"en": "",   // Empty -- should be skipped
		"ja": "JA", // Non-empty -- should be picked
	}
	got := SelectLocalizedBiography(candidates, []string{"en", "ja"}, "fallback")
	if got != "JA" {
		t.Errorf("expected empty en to be skipped; got %q, want %q", got, "JA")
	}
}

func TestLanguageBase(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"en", "en"},
		{"en-GB", "en"},
		{"zh-Hant-TW", "zh"},
		{"ja", "ja"},
		{"", ""},
	}
	for _, tc := range tests {
		got := languageBase(tc.input)
		if got != tc.want {
			t.Errorf("languageBase(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestFirstMetadataLang(t *testing.T) {
	tests := []struct {
		name  string
		langs []string
		want  string
	}{
		{"no preference", nil, ""},
		{"single language", []string{"ja"}, "ja"},
		{"region subtag stripped", []string{"ja-JP"}, "ja"},
		{"lowercased", []string{"JA"}, "ja"},
		{"blank first entry skipped", []string{"", "ja"}, "ja"},
		{"whitespace first entry skipped", []string{"   ", "fr"}, "fr"},
		{"all entries blank", []string{"", "  "}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.langs != nil {
				ctx = WithMetadataLanguages(ctx, tc.langs)
			}
			if got := FirstMetadataLang(ctx); got != tc.want {
				t.Errorf("FirstMetadataLang(%v) = %q, want %q", tc.langs, got, tc.want)
			}
		})
	}
}

func TestNameRomanizationFallback(t *testing.T) {
	// Unset context must return true (preserve existing shipped behavior).
	ctx := context.Background()
	if got := NameRomanizationFallback(ctx); !got {
		t.Error("NameRomanizationFallback on unset ctx: expected true, got false")
	}

	// Explicit true.
	ctx = WithNameRomanizationFallback(context.Background(), true)
	if got := NameRomanizationFallback(ctx); !got {
		t.Error("WithNameRomanizationFallback(true): expected true, got false")
	}

	// Explicit false.
	ctx = WithNameRomanizationFallback(context.Background(), false)
	if got := NameRomanizationFallback(ctx); got {
		t.Error("WithNameRomanizationFallback(false): expected false, got true")
	}
}
