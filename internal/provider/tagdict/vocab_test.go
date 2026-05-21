package tagdict

import (
	"context"
	"testing"
)

// equalStrings reports whether two string slices have the same elements in
// the same order.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestApplyVocabFilter_NilConfig verifies that a nil config is a complete
// no-op: the output is identical to the input.
func TestApplyVocabFilter_NilConfig(t *testing.T) {
	t.Parallel()
	input := []string{"Rock", "Pop", "Jazz"}
	got := ApplyVocabFilter(nil, VocabFieldGenres, input)
	if !equalStrings(got, input) {
		t.Fatalf("nil config changed output: got %v, want %v", got, input)
	}
}

// TestApplyVocabFilter_DefaultConfig verifies that the default config (empty
// exclude list, zero caps) is also a no-op.
func TestApplyVocabFilter_DefaultConfig(t *testing.T) {
	t.Parallel()
	input := []string{"Rock", "Pop"}
	got := ApplyVocabFilter(DefaultVocabConfig(), VocabFieldGenres, input)
	if !equalStrings(got, input) {
		t.Fatalf("default config changed output: got %v, want %v", got, input)
	}
}

// TestApplyVocabFilter_ExcludeExact verifies that an exact (no-wildcard)
// exclude pattern drops exactly the matching tag, case-insensitively.
func TestApplyVocabFilter_ExcludeExact(t *testing.T) {
	t.Parallel()
	cfg := &VocabConfig{Exclude: []string{"christian"}}

	// "christian" is dropped; "Christian Rock" is NOT (exact match only).
	input := []string{"Rock", "christian", "Christian Rock", "CHRISTIAN"}
	got := ApplyVocabFilter(cfg, VocabFieldGenres, input)
	want := []string{"Rock", "Christian Rock"}
	if !equalStrings(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestApplyVocabFilter_ExcludePrefixWildcard verifies a trailing-"*" pattern
// drops every tag with the given prefix.
func TestApplyVocabFilter_ExcludePrefixWildcard(t *testing.T) {
	t.Parallel()
	cfg := &VocabConfig{Exclude: []string{"christian*"}}

	input := []string{"Rock", "christian", "Christian Rock", "christian metal", "Pop"}
	got := ApplyVocabFilter(cfg, VocabFieldGenres, input)
	want := []string{"Rock", "Pop"}
	if !equalStrings(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestApplyVocabFilter_ExcludeSuffixWildcard verifies a leading-"*" pattern
// drops every tag with the given suffix.
func TestApplyVocabFilter_ExcludeSuffixWildcard(t *testing.T) {
	t.Parallel()
	cfg := &VocabConfig{Exclude: []string{"*core"}}

	input := []string{"Metalcore", "Rock", "Deathcore", "Pop"}
	got := ApplyVocabFilter(cfg, VocabFieldGenres, input)
	want := []string{"Rock", "Pop"}
	if !equalStrings(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestApplyVocabFilter_ExcludeContainsWildcard verifies a "*x*" pattern drops
// every tag containing the substring.
func TestApplyVocabFilter_ExcludeContainsWildcard(t *testing.T) {
	t.Parallel()
	cfg := &VocabConfig{Exclude: []string{"*live*"}}

	input := []string{"seen live", "Rock", "live favorites", "Jazz"}
	got := ApplyVocabFilter(cfg, VocabFieldGenres, input)
	want := []string{"Rock", "Jazz"}
	if !equalStrings(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestApplyVocabFilter_ExcludeMultipleStars verifies a pattern with interior
// wildcards matches segments in order.
func TestApplyVocabFilter_ExcludeMultipleStars(t *testing.T) {
	t.Parallel()
	cfg := &VocabConfig{Exclude: []string{"a*b*c"}}

	input := []string{"axbxc", "abc", "acb", "Rock"}
	got := ApplyVocabFilter(cfg, VocabFieldGenres, input)
	// "axbxc" and "abc" match a*b*c; "acb" does not (no c after b); "Rock" no.
	want := []string{"acb", "Rock"}
	if !equalStrings(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestApplyVocabFilter_BlankPatternIgnored verifies that a blank exclude
// pattern is skipped and does not drop every tag.
func TestApplyVocabFilter_BlankPatternIgnored(t *testing.T) {
	t.Parallel()
	cfg := &VocabConfig{Exclude: []string{"  ", ""}}

	input := []string{"Rock", "Pop"}
	got := ApplyVocabFilter(cfg, VocabFieldGenres, input)
	if !equalStrings(got, input) {
		t.Fatalf("blank patterns dropped tags: got %v, want %v", got, input)
	}
}

// TestApplyVocabFilter_MaxCount verifies the per-field cap truncates the list
// to the highest-priority (earliest) tags.
func TestApplyVocabFilter_MaxCount(t *testing.T) {
	t.Parallel()
	cfg := &VocabConfig{MaxGenres: 2}

	input := []string{"Rock", "Pop", "Jazz", "Blues"}
	got := ApplyVocabFilter(cfg, VocabFieldGenres, input)
	want := []string{"Rock", "Pop"}
	if !equalStrings(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestApplyVocabFilter_MaxCountPerField verifies each field uses its own cap
// and an unset cap (0) means unlimited.
func TestApplyVocabFilter_MaxCountPerField(t *testing.T) {
	t.Parallel()
	cfg := &VocabConfig{MaxGenres: 1, MaxStyles: 3}

	tags := []string{"a", "b", "c", "d"}
	if got := ApplyVocabFilter(cfg, VocabFieldGenres, tags); len(got) != 1 {
		t.Errorf("genres: expected 1, got %d (%v)", len(got), got)
	}
	if got := ApplyVocabFilter(cfg, VocabFieldStyles, tags); len(got) != 3 {
		t.Errorf("styles: expected 3, got %d (%v)", len(got), got)
	}
	// Moods cap is 0 (unlimited): all four survive.
	if got := ApplyVocabFilter(cfg, VocabFieldMoods, tags); len(got) != 4 {
		t.Errorf("moods: expected 4 (unlimited), got %d (%v)", len(got), got)
	}
}

// TestApplyVocabFilter_ExcludeThenCap verifies exclusion runs before the cap:
// excluded tags do not consume a slot in the capped output.
func TestApplyVocabFilter_ExcludeThenCap(t *testing.T) {
	t.Parallel()
	cfg := &VocabConfig{Exclude: []string{"christian*"}, MaxGenres: 2}

	// "christian rock" is excluded first, so the cap of 2 yields Rock + Pop,
	// not Rock + (excluded).
	input := []string{"Rock", "christian rock", "Pop", "Jazz"}
	got := ApplyVocabFilter(cfg, VocabFieldGenres, input)
	want := []string{"Rock", "Pop"}
	if !equalStrings(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestApplyVocabFilter_UnknownField verifies an unrecognized field name uses
// no cap (0) but still applies the shared exclude list.
func TestApplyVocabFilter_UnknownField(t *testing.T) {
	t.Parallel()
	cfg := &VocabConfig{Exclude: []string{"junk"}, MaxGenres: 1}

	input := []string{"Rock", "junk", "Pop"}
	got := ApplyVocabFilter(cfg, "unknown_field", input)
	// Exclude still applies; the genres cap does not apply to an unknown field.
	want := []string{"Rock", "Pop"}
	if !equalStrings(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestApplyVocabFilter_ExcludeAll verifies that a lone "*" exclude pattern
// drops every tag and returns a non-nil empty slice (not nil), so downstream
// JSON/NFO writers see an explicit empty list.
func TestApplyVocabFilter_ExcludeAll(t *testing.T) {
	t.Parallel()
	cfg := &VocabConfig{Exclude: []string{"*"}}
	got := ApplyVocabFilter(cfg, VocabFieldGenres, []string{"Rock", "Pop"})
	if got == nil {
		t.Fatal("expected a non-nil empty slice, got nil")
	}
	if len(got) != 0 {
		t.Fatalf("expected all tags dropped, got %v", got)
	}
}

// TestWildcardMatch covers the wildcard matcher directly, including edge cases.
func TestWildcardMatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"rock", "rock", true},
		{"rock", "pop", false},
		{"christian*", "christian rock", true},
		{"christian*", "christian", true},
		{"christian*", "post-christian", false},
		{"*core", "metalcore", true},
		{"*core", "core", true},
		{"*core", "corey", false},
		{"*live*", "seen live", true},
		{"*live*", "live", true},
		{"*live*", "rock", false},
		{"*", "anything", true},
		{"a*b*c", "axxbxxc", true},
		{"a*b*c", "abc", true},
		{"a*b*c", "acb", false},
		// Edge cases: empty pattern matches only the empty string;
		// consecutive "**" collapses to a single wildcard.
		{"", "", true},
		{"", "rock", false},
		{"*", "", true},
		{"a**c", "ac", true},
		{"a**c", "axc", true},
	}
	for _, c := range cases {
		if got := wildcardMatch(c.pattern, c.s); got != c.want {
			t.Errorf("wildcardMatch(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

// TestParseVocabConfig verifies JSON round-tripping of a VocabConfig.
func TestParseVocabConfig(t *testing.T) {
	t.Parallel()
	raw := `{"exclude":["christian","*core"],"max_genres":5,"max_styles":0,"max_moods":3}`
	cfg, err := ParseVocabConfig(raw)
	if err != nil {
		t.Fatalf("ParseVocabConfig: %v", err)
	}
	if len(cfg.Exclude) != 2 || cfg.Exclude[0] != "christian" {
		t.Errorf("unexpected exclude: %v", cfg.Exclude)
	}
	if cfg.MaxGenres != 5 || cfg.MaxStyles != 0 || cfg.MaxMoods != 3 {
		t.Errorf("unexpected caps: g=%d s=%d m=%d", cfg.MaxGenres, cfg.MaxStyles, cfg.MaxMoods)
	}

	if _, err := ParseVocabConfig("{not json"); err == nil {
		t.Error("ParseVocabConfig should return an error for malformed JSON")
	}

	// A blob that omits "exclude" must still parse to a non-nil slice so the
	// API surfaces it as [] rather than null.
	noExclude, err := ParseVocabConfig(`{"max_genres":2}`)
	if err != nil {
		t.Fatalf("ParseVocabConfig: %v", err)
	}
	if noExclude.Exclude == nil {
		t.Error("Exclude should be normalized to a non-nil slice when absent from the JSON")
	}
}

// TestDefaultVocabConfig verifies the default config is a non-nil no-op with a
// non-nil (empty) exclude slice so the JSON API surfaces [] rather than null.
func TestDefaultVocabConfig(t *testing.T) {
	t.Parallel()
	cfg := DefaultVocabConfig()
	if cfg == nil {
		t.Fatal("DefaultVocabConfig returned nil")
	}
	if cfg.Exclude == nil {
		t.Error("default Exclude should be non-nil (empty slice)")
	}
	if len(cfg.Exclude) != 0 || cfg.MaxGenres != 0 || cfg.MaxStyles != 0 || cfg.MaxMoods != 0 {
		t.Errorf("default config is not a no-op: %+v", cfg)
	}
}

// TestMetadataVocab_ContextRoundTrip verifies the WithMetadataVocab /
// MetadataVocab context helpers: a config stored on a context is read back
// unchanged, and a context with no config injected returns nil.
func TestMetadataVocab_ContextRoundTrip(t *testing.T) {
	t.Parallel()

	if got := MetadataVocab(context.Background()); got != nil {
		t.Errorf("expected nil from a context with no vocab config, got %v", got)
	}

	cfg := &VocabConfig{Exclude: []string{"junk"}, MaxGenres: 5}
	ctx := WithMetadataVocab(context.Background(), cfg)
	got := MetadataVocab(ctx)
	if got != cfg {
		t.Fatal("MetadataVocab did not round-trip the stored config")
	}
}
