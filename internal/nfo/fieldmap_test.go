package nfo

import (
	"bytes"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

func TestDefaultFieldMap(t *testing.T) {
	fm := DefaultFieldMap()
	if !fm.DefaultBehavior {
		t.Error("DefaultFieldMap should have DefaultBehavior=true")
	}
	if len(fm.GenreSources) != 1 || fm.GenreSources[0] != "genres" {
		t.Errorf("GenreSources = %v, want [genres]", fm.GenreSources)
	}
	if fm.MoodsAsStyles {
		t.Error("DefaultFieldMap should have MoodsAsStyles=false")
	}
	if fm.AdvancedRemap != nil {
		t.Error("DefaultFieldMap should have AdvancedRemap=nil")
	}
}

func TestApplyFieldMap_DefaultPassthrough(t *testing.T) {
	fm := DefaultFieldMap()
	genres := []string{"Rock", "Jazz"}
	styles := []string{"Grunge", "Bebop"}
	moods := []string{"Aggressive", "Melancholy"}

	nfoGenres, nfoStyles, nfoMoods := ApplyFieldMap(fm, genres, styles, moods)

	assertSliceEqual(t, "genres", nfoGenres, genres)
	assertSliceEqual(t, "styles", nfoStyles, styles)
	assertSliceEqual(t, "moods", nfoMoods, moods)
}

func TestApplyFieldMap_DefaultPassthrough_EmptyInputs(t *testing.T) {
	fm := DefaultFieldMap()
	nfoGenres, nfoStyles, nfoMoods := ApplyFieldMap(fm, nil, nil, nil)

	if nfoGenres != nil {
		t.Errorf("genres should be nil for empty input, got %v", nfoGenres)
	}
	if nfoStyles != nil {
		t.Errorf("styles should be nil for empty input, got %v", nfoStyles)
	}
	if nfoMoods != nil {
		t.Errorf("moods should be nil for empty input, got %v", nfoMoods)
	}
}

func TestApplyFieldMap_MoodsAsStyles(t *testing.T) {
	fm := NFOFieldMap{
		DefaultBehavior: false,
		MoodsAsStyles:   true,
		GenreSources:    []string{"genres"},
	}
	genres := []string{"Rock"}
	styles := []string{"Grunge"}
	moods := []string{"Aggressive", "Melancholy"}

	nfoGenres, nfoStyles, nfoMoods := ApplyFieldMap(fm, genres, styles, moods)

	assertSliceEqual(t, "genres", nfoGenres, []string{"Rock"})
	// Styles should include original styles + moods
	assertSliceEqual(t, "styles", nfoStyles, []string{"Grunge", "Aggressive", "Melancholy"})
	// Moods are still written for Kodi
	assertSliceEqual(t, "moods", nfoMoods, []string{"Aggressive", "Melancholy"})
}

func TestApplyFieldMap_MoodsAsStyles_Deduplication(t *testing.T) {
	fm := NFOFieldMap{
		DefaultBehavior: false,
		MoodsAsStyles:   true,
		GenreSources:    []string{"genres"},
	}
	// "aggressive" appears in both styles and moods (different case)
	styles := []string{"Aggressive", "Grunge"}
	moods := []string{"aggressive", "Melancholy"}

	_, nfoStyles, _ := ApplyFieldMap(fm, nil, styles, moods)

	// Should deduplicate case-insensitively, keeping first occurrence
	assertSliceEqual(t, "styles", nfoStyles, []string{"Aggressive", "Grunge", "Melancholy"})
}

func TestApplyFieldMap_GenreSources_MultipleCategories(t *testing.T) {
	fm := NFOFieldMap{
		DefaultBehavior: false,
		GenreSources:    []string{"genres", "styles"},
	}
	genres := []string{"Rock", "Jazz"}
	styles := []string{"Grunge", "Rock"} // "Rock" overlaps with genres

	nfoGenres, nfoStyles, _ := ApplyFieldMap(fm, genres, styles, nil)

	// Genres should contain both genres and styles, deduplicated
	assertSliceEqual(t, "genres", nfoGenres, []string{"Rock", "Jazz", "Grunge"})
	// Styles remain unchanged (not affected by GenreSources)
	assertSliceEqual(t, "styles", nfoStyles, []string{"Grunge", "Rock"})
}

func TestApplyFieldMap_GenreSources_Empty(t *testing.T) {
	// When GenreSources is empty, it should default to ["genres"]
	fm := NFOFieldMap{
		DefaultBehavior: false,
		GenreSources:    nil,
	}
	genres := []string{"Rock"}

	nfoGenres, _, _ := ApplyFieldMap(fm, genres, nil, nil)
	assertSliceEqual(t, "genres", nfoGenres, []string{"Rock"})
}

func TestApplyFieldMap_AdvancedRemap_OverridesEverything(t *testing.T) {
	fm := NFOFieldMap{
		DefaultBehavior: true,              // should be ignored
		MoodsAsStyles:   true,              // should be ignored
		GenreSources:    []string{"moods"}, // should be ignored
		AdvancedRemap: map[string][]string{
			"genre": {"styles"},          // genres from styles
			"style": {"genres", "moods"}, // styles from genres+moods
			"mood":  {},                  // no moods written
		},
	}
	genres := []string{"Rock", "Jazz"}
	styles := []string{"Grunge"}
	moods := []string{"Aggressive"}

	nfoGenres, nfoStyles, nfoMoods := ApplyFieldMap(fm, genres, styles, moods)

	assertSliceEqual(t, "genres", nfoGenres, []string{"Grunge"})
	assertSliceEqual(t, "styles", nfoStyles, []string{"Rock", "Jazz", "Aggressive"})
	if len(nfoMoods) != 0 {
		t.Errorf("moods should be empty when remap has empty source, got %v", nfoMoods)
	}
}

func TestApplyFieldMap_AdvancedRemap_DeduplicatesAcrossSources(t *testing.T) {
	fm := NFOFieldMap{
		AdvancedRemap: map[string][]string{
			"genre": {"genres", "styles"},
		},
	}
	genres := []string{"Rock", "Jazz"}
	styles := []string{"rock", "Grunge"} // "rock" overlaps case-insensitively

	nfoGenres, _, _ := ApplyFieldMap(fm, genres, styles, nil)

	// Should deduplicate case-insensitively
	assertSliceEqual(t, "genres", nfoGenres, []string{"Rock", "Jazz", "Grunge"})
}

func TestApplyFieldMap_AdvancedRemap_MissingElement(t *testing.T) {
	// If an element key is not present in the remap, it gets no values
	fm := NFOFieldMap{
		AdvancedRemap: map[string][]string{
			"genre": {"genres"},
			// "style" and "mood" are absent
		},
	}
	genres := []string{"Rock"}
	styles := []string{"Grunge"}
	moods := []string{"Aggressive"}

	nfoGenres, nfoStyles, nfoMoods := ApplyFieldMap(fm, genres, styles, moods)

	assertSliceEqual(t, "genres", nfoGenres, []string{"Rock"})
	if len(nfoStyles) != 0 {
		t.Errorf("styles should be empty when not in remap, got %v", nfoStyles)
	}
	if len(nfoMoods) != 0 {
		t.Errorf("moods should be empty when not in remap, got %v", nfoMoods)
	}
}

func TestApplyFieldMap_JellyfinPreset(t *testing.T) {
	// Jellyfin ignores <genre> for artists, so map genres to <style>
	fm := NFOFieldMap{
		DefaultBehavior: false,
		MoodsAsStyles:   true,
		GenreSources:    []string{}, // no genres in <genre>
		AdvancedRemap: map[string][]string{
			"genre": {},                            // empty -- Jellyfin ignores it
			"style": {"genres", "styles", "moods"}, // everything as tags
			"mood":  {"moods"},                     // keep for Kodi
		},
	}
	genres := []string{"Rock", "Jazz"}
	styles := []string{"Grunge"}
	moods := []string{"Aggressive"}

	nfoGenres, nfoStyles, nfoMoods := ApplyFieldMap(fm, genres, styles, moods)

	if len(nfoGenres) != 0 {
		t.Errorf("genres should be empty for Jellyfin preset, got %v", nfoGenres)
	}
	assertSliceEqual(t, "styles", nfoStyles, []string{"Rock", "Jazz", "Grunge", "Aggressive"})
	assertSliceEqual(t, "moods", nfoMoods, []string{"Aggressive"})
}

func TestApplyFieldMap_PassthroughDoesNotMutateInput(t *testing.T) {
	fm := DefaultFieldMap()
	genres := []string{"Rock", "Jazz"}
	original := make([]string, len(genres))
	copy(original, genres)

	nfoGenres, _, _ := ApplyFieldMap(fm, genres, nil, nil)

	// Mutating the output should not affect the input
	if len(nfoGenres) > 0 {
		nfoGenres[0] = "MODIFIED"
	}
	assertSliceEqual(t, "original genres", genres, original)
}

func TestFromArtistWithFieldMap_Default(t *testing.T) {
	a := &artist.Artist{
		Name:   "Test Artist",
		Genres: []string{"Rock"},
		Styles: []string{"Grunge"},
		Moods:  []string{"Aggressive"},
	}

	n := FromArtistWithFieldMap(a, DefaultFieldMap())

	assertSliceEqual(t, "genres", n.Genres, []string{"Rock"})
	assertSliceEqual(t, "styles", n.Styles, []string{"Grunge"})
	assertSliceEqual(t, "moods", n.Moods, []string{"Aggressive"})
}

func TestFromArtistWithFieldMap_MoodsAsStyles(t *testing.T) {
	a := &artist.Artist{
		Name:   "Test Artist",
		Genres: []string{"Rock"},
		Styles: []string{"Grunge"},
		Moods:  []string{"Aggressive"},
	}

	fm := NFOFieldMap{
		DefaultBehavior: false,
		MoodsAsStyles:   true,
		GenreSources:    []string{"genres"},
	}
	n := FromArtistWithFieldMap(a, fm)

	assertSliceEqual(t, "genres", n.Genres, []string{"Rock"})
	assertSliceEqual(t, "styles", n.Styles, []string{"Grunge", "Aggressive"})
	assertSliceEqual(t, "moods", n.Moods, []string{"Aggressive"})
}

func TestFromArtistWithFieldMap_RoundTrip(t *testing.T) {
	// Create an NFO with field mapping, write to XML, parse back, verify elements.
	a := &artist.Artist{
		Name:          "Round Trip Artist",
		SortName:      "Round Trip Artist",
		MusicBrainzID: "mbid-roundtrip",
		Genres:        []string{"Rock", "Jazz"},
		Styles:        []string{"Grunge"},
		Moods:         []string{"Aggressive", "Melancholy"},
	}

	fm := NFOFieldMap{
		DefaultBehavior: false,
		MoodsAsStyles:   true,
		GenreSources:    []string{"genres"},
	}
	nfoData := FromArtistWithFieldMap(a, fm)

	var buf bytes.Buffer
	if err := Write(&buf, nfoData); err != nil {
		t.Fatalf("Write: %v", err)
	}

	parsed, err := Parse(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Genres should be unchanged
	assertSliceEqual(t, "parsed genres", parsed.Genres, []string{"Rock", "Jazz"})
	// Styles should include moods
	assertSliceEqual(t, "parsed styles", parsed.Styles, []string{"Grunge", "Aggressive", "Melancholy"})
	// Moods should be preserved
	assertSliceEqual(t, "parsed moods", parsed.Moods, []string{"Aggressive", "Melancholy"})
}

func TestDeduplicateCaseInsensitive(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  []string
	}{
		{"nil input", nil, nil},
		{"empty input", []string{}, nil},
		{"no duplicates", []string{"Rock", "Jazz"}, []string{"Rock", "Jazz"}},
		{"exact duplicates", []string{"Rock", "Rock"}, []string{"Rock"}},
		{"case duplicates", []string{"Rock", "rock", "ROCK"}, []string{"Rock"}},
		{"mixed", []string{"Rock", "Jazz", "rock", "Blues", "jazz"}, []string{"Rock", "Jazz", "Blues"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deduplicateCaseInsensitive(tt.input)
			assertSliceEqual(t, "result", got, tt.want)
		})
	}
}

// assertSliceEqual is a test helper that compares two string slices.
func assertSliceEqual(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: length = %d, want %d; got %v, want %v", name, len(got), len(want), got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %q, want %q", name, i, got[i], want[i])
		}
	}
}
