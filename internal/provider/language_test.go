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
	got := MetadataLanguages(ctx)
	if len(got) != 3 {
		t.Fatalf("expected 3 languages, got %d", len(got))
	}
	for i, want := range langs {
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
