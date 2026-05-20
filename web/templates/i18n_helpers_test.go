package templates

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/i18n"
)

// TestTInstrument exercises all three branches of the tInstrument render
// helper: a locale that has the translation, the English fallback when the
// locale lacks the key, and the raw-attribute passthrough when the key is
// absent everywhere. It also pins the case/whitespace normalization that
// keeps lookups robust to MusicBrainz attribute casing.
func TestTInstrument(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	// en has bass + guitar; ja has only bass, so "guitar" exercises the
	// non-English-to-English fallback chain.
	write("en.json", `{"instrument.bass": "Bass", "instrument.guitar": "Guitar"}`)
	write("ja.json", `{"instrument.bass": "ベース"}`)

	bundle, err := i18n.Load(dir)
	if err != nil {
		t.Fatalf("i18n.Load: %v", err)
	}
	ctx := i18n.WithTranslator(context.Background(), bundle.Translator("ja"))

	tests := []struct {
		name string
		attr string
		want string
	}{
		{"localized form", "bass", "ベース"},
		{"case-insensitive lookup", "BASS", "ベース"},
		{"whitespace trimmed", "  bass  ", "ベース"},
		{"english fallback when locale lacks key", "guitar", "Guitar"},
		{"raw passthrough when absent everywhere", "theremin", "theremin"},
		{"raw passthrough preserves original casing", "Theremin", "Theremin"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tInstrument(ctx, tc.attr); got != tc.want {
				t.Errorf("tInstrument(ctx, %q) = %q, want %q", tc.attr, got, tc.want)
			}
		})
	}
}
