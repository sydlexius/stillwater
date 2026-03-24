package provider

import (
	"testing"
)

func TestNameSimilarity(t *testing.T) {
	tests := []struct {
		a, b string
		min  int
		max  int
	}{
		{"Radiohead", "Radiohead", 100, 100},
		{"radiohead", "Radiohead", 100, 100},
		{"The Beatles", "Beatles", 100, 100},
		{"The The", "The", 0, 59},
		{"Adele", "Kim Kardashian", 0, 30},
		{"Guns N' Roses", "Guns N Roses", 80, 100},
		{"AC/DC", "ACDC", 100, 100},
		{"!!!", "!!!", 100, 100},
		{"!!!", "???", 0, 0},
		{"\u00c9milie Simon", "Emilie Simon", 80, 100},
		{"", "Radiohead", 0, 0},
		{"Radiohead", "", 0, 0},
		{"", "", 100, 100},
	}
	for _, tt := range tests {
		score := NameSimilarity(tt.a, tt.b)
		if score < tt.min || score > tt.max {
			t.Errorf("NameSimilarity(%q, %q) = %d, want [%d, %d]",
				tt.a, tt.b, score, tt.min, tt.max)
		}
	}
}

func TestNormalizeName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Radiohead", "radiohead"},
		{"The Beatles", "beatles"},
		{"The The", "the the"},
		{"AC/DC", "acdc"},
		{"Guns N' Roses", "guns n roses"},
		{"  Adele  ", "adele"},
	}
	for _, tt := range tests {
		if got := NormalizeName(tt.input); got != tt.want {
			t.Errorf("NormalizeName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestLevenshteinRunes(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "", 3},
		{"", "abc", 3},
		{"kitten", "sitting", 3},
		{"radiohead", "radiohead", 0},
	}
	for _, tt := range tests {
		if got := LevenshteinRunes([]rune(tt.a), []rune(tt.b)); got != tt.want {
			t.Errorf("LevenshteinRunes(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
