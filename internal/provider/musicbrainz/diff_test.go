package musicbrainz

import (
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

func TestComputeDiffs(t *testing.T) {
	tests := []struct {
		name      string
		artist    *artist.Artist
		snapshots map[string]artist.MBSnapshot
		sources   map[string]string
		wantCount int
		wantField string // if wantCount == 1, check this field name
	}{
		{
			name: "no snapshots returns no diffs",
			artist: &artist.Artist{
				Name:   "Test",
				Genres: []string{"rock"},
			},
			snapshots: map[string]artist.MBSnapshot{},
			wantCount: 0,
		},
		{
			name: "matching values returns no diffs",
			artist: &artist.Artist{
				Name: "Test Artist",
				Type: "group",
			},
			snapshots: map[string]artist.MBSnapshot{
				"name": {Field: "name", MBValue: "Test Artist"},
				"type": {Field: "type", MBValue: "group"},
			},
			wantCount: 0,
		},
		{
			name: "case-insensitive matching for text fields",
			artist: &artist.Artist{
				Type: "Group",
			},
			snapshots: map[string]artist.MBSnapshot{
				"type": {Field: "type", MBValue: "group"},
			},
			wantCount: 0,
		},
		{
			name: "different type produces diff",
			artist: &artist.Artist{
				Type: "person",
			},
			snapshots: map[string]artist.MBSnapshot{
				"type": {Field: "type", MBValue: "group"},
			},
			sources:   map[string]string{"type": "discogs"},
			wantCount: 1,
			wantField: "type",
		},
		{
			name: "empty stillwater value skipped",
			artist: &artist.Artist{
				Type: "",
			},
			snapshots: map[string]artist.MBSnapshot{
				"type": {Field: "type", MBValue: "group"},
			},
			wantCount: 0,
		},
		{
			name: "genre set comparison (sorted, case-insensitive)",
			artist: &artist.Artist{
				Genres: []string{"Rock", "Alternative"},
			},
			snapshots: map[string]artist.MBSnapshot{
				"genres": {Field: "genres", MBValue: `["alternative","rock"]`},
			},
			wantCount: 0,
		},
		{
			name: "genre difference detected",
			artist: &artist.Artist{
				Genres: []string{"rock", "alternative", "synth-pop"},
			},
			snapshots: map[string]artist.MBSnapshot{
				"genres": {Field: "genres", MBValue: `["rock","alternative"]`},
			},
			sources:   map[string]string{"genres": "audiodb"},
			wantCount: 1,
			wantField: "genres",
		},
		{
			name: "multiple diffs",
			artist: &artist.Artist{
				Name:   "New Name",
				Type:   "person",
				Formed: "1990",
			},
			snapshots: map[string]artist.MBSnapshot{
				"name":   {Field: "name", MBValue: "Old Name"},
				"type":   {Field: "type", MBValue: "group"},
				"formed": {Field: "formed", MBValue: "1990"},
			},
			wantCount: 2,
		},
		{
			name: "date field difference",
			artist: &artist.Artist{
				Born: "1965-11-21",
			},
			snapshots: map[string]artist.MBSnapshot{
				"born": {Field: "born", MBValue: "1965"},
			},
			sources:   map[string]string{"born": "wikidata"},
			wantCount: 1,
			wantField: "born",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diffs := ComputeDiffs(tt.artist, tt.snapshots, tt.sources)
			if len(diffs) != tt.wantCount {
				t.Fatalf("len(diffs) = %d, want %d; diffs = %+v", len(diffs), tt.wantCount, diffs)
			}
			if tt.wantCount == 1 && tt.wantField != "" {
				if diffs[0].Field != tt.wantField {
					t.Errorf("diffs[0].Field = %q, want %q", diffs[0].Field, tt.wantField)
				}
			}
		})
	}
}

func TestComputeDiffs_source_attribution(t *testing.T) {
	a := &artist.Artist{Type: "person"}
	snaps := map[string]artist.MBSnapshot{
		"type": {Field: "type", MBValue: "group"},
	}
	sources := map[string]string{"type": "discogs"}

	diffs := ComputeDiffs(a, snaps, sources)
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d", len(diffs))
	}
	if diffs[0].Source != "discogs" {
		t.Errorf("Source = %q, want %q", diffs[0].Source, "discogs")
	}
	if diffs[0].StillwaterValue != "person" {
		t.Errorf("StillwaterValue = %q, want %q", diffs[0].StillwaterValue, "person")
	}
	if diffs[0].MusicBrainzValue != "group" {
		t.Errorf("MusicBrainzValue = %q, want %q", diffs[0].MusicBrainzValue, "group")
	}
}

func TestExtractMBFieldValues(t *testing.T) {
	meta := &provider.ArtistMetadata{
		Name:    "Test Artist",
		Type:    "group",
		Gender:  "",
		Genres:  []string{"rock", "pop"},
		Formed:  "1990",
		Born:    "1965",
		Country: "US", // not a snapshot field
	}

	sources := []provider.FieldSource{
		{Field: "name", Provider: provider.NameMusicBrainz},
		{Field: "type", Provider: provider.NameMusicBrainz},
		{Field: "genres", Provider: provider.NameAudioDB}, // not from MB
		{Field: "formed", Provider: provider.NameMusicBrainz},
		{Field: "born", Provider: provider.NameMusicBrainz},
	}

	snapshots := ExtractMBFieldValues(meta, sources)

	// Should have name, type, formed, born (4 fields from MB).
	// genres is from AudioDB so excluded.
	if len(snapshots) != 4 {
		t.Fatalf("len(snapshots) = %d, want 4", len(snapshots))
	}

	fieldValues := make(map[string]string)
	for _, s := range snapshots {
		fieldValues[s.Field] = s.MBValue
	}

	if fieldValues["name"] != "Test Artist" {
		t.Errorf("name = %q, want %q", fieldValues["name"], "Test Artist")
	}
	if fieldValues["type"] != "group" {
		t.Errorf("type = %q, want %q", fieldValues["type"], "group")
	}
	if fieldValues["formed"] != "1990" {
		t.Errorf("formed = %q, want %q", fieldValues["formed"], "1990")
	}
	if _, ok := fieldValues["genres"]; ok {
		t.Error("genres should not be in snapshots (sourced from AudioDB)")
	}
}

func TestExtractMBFieldValues_no_mb_sources(t *testing.T) {
	meta := &provider.ArtistMetadata{
		Name:   "Test",
		Genres: []string{"pop"},
	}
	sources := []provider.FieldSource{
		{Field: "name", Provider: provider.NameAudioDB},
		{Field: "genres", Provider: provider.NameDiscogs},
	}

	snapshots := ExtractMBFieldValues(meta, sources)
	if len(snapshots) != 0 {
		t.Errorf("len(snapshots) = %d, want 0 (no MB sources)", len(snapshots))
	}
}

func TestValuesEqual(t *testing.T) {
	tests := []struct {
		name  string
		field string
		a, b  string
		want  bool
	}{
		{"identical strings", "type", "group", "group", true},
		{"case difference", "type", "Group", "group", true},
		{"whitespace trimmed", "name", "  Test  ", "Test", true},
		{"different values", "name", "Alice", "Bob", false},
		{"genres same order", "genres", `["pop","rock"]`, `["pop","rock"]`, true},
		{"genres different order", "genres", `["rock","pop"]`, `["pop","rock"]`, true},
		{"genres different case", "genres", `["Rock","Pop"]`, `["pop","rock"]`, true},
		{"genres different content", "genres", `["rock","pop","jazz"]`, `["pop","rock"]`, false},
		{"empty genres", "genres", `[]`, `[]`, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := valuesEqual(tt.field, tt.a, tt.b)
			if got != tt.want {
				t.Errorf("valuesEqual(%q, %q, %q) = %v, want %v", tt.field, tt.a, tt.b, got, tt.want)
			}
		})
	}
}
