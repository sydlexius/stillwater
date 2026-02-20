package nfo

import "testing"

func TestDiff_Identical(t *testing.T) {
	a := &ArtistNFO{Name: "Radiohead", SortName: "Radiohead", Type: "Group"}
	b := &ArtistNFO{Name: "Radiohead", SortName: "Radiohead", Type: "Group"}

	result := Diff(a, b)
	if result.HasDiff {
		t.Error("expected no diff for identical NFOs")
	}
	for _, f := range result.Fields {
		if f.Status != "unchanged" {
			t.Errorf("field %s: expected unchanged, got %s", f.Field, f.Status)
		}
	}
}

func TestDiff_NilOld(t *testing.T) {
	b := &ArtistNFO{
		Name:                "Radiohead",
		MusicBrainzArtistID: "a74b1b7f-71a5-4011-9441-d0b5e4122711",
		Genres:              []string{"Rock", "Alternative"},
	}

	result := Diff(nil, b)
	if !result.HasDiff {
		t.Error("expected diff when old is nil")
	}

	nameField := findField(result, "Name")
	if nameField == nil || nameField.Status != "added" {
		t.Errorf("expected Name to be added, got %v", nameField)
	}

	genreField := findField(result, "Genres")
	if genreField == nil || genreField.Status != "added" {
		t.Errorf("expected Genres to be added, got %v", genreField)
	}
	if genreField != nil && genreField.NewValue != "Rock, Alternative" {
		t.Errorf("expected genres 'Rock, Alternative', got %q", genreField.NewValue)
	}
}

func TestDiff_NilNew(t *testing.T) {
	a := &ArtistNFO{Name: "Radiohead", Biography: "A band from Oxford."}

	result := Diff(a, nil)
	if !result.HasDiff {
		t.Error("expected diff when new is nil")
	}

	nameField := findField(result, "Name")
	if nameField == nil || nameField.Status != "removed" {
		t.Errorf("expected Name to be removed, got %v", nameField)
	}
}

func TestDiff_MixedChanges(t *testing.T) {
	old := &ArtistNFO{
		Name:      "Radiohead",
		SortName:  "Radiohead",
		Type:      "Group",
		Biography: "Old bio",
		Genres:    []string{"Rock"},
	}
	newNFO := &ArtistNFO{
		Name:                "Radiohead",
		SortName:            "Radiohead",
		Type:                "Group",
		Biography:           "New bio with more detail",
		Genres:              []string{"Rock", "Alternative"},
		MusicBrainzArtistID: "a74b1b7f",
	}

	result := Diff(old, newNFO)
	if !result.HasDiff {
		t.Error("expected diff for changed fields")
	}

	bio := findField(result, "Biography")
	if bio == nil || bio.Status != "changed" {
		t.Errorf("expected Biography to be changed, got %v", bio)
	}

	mbid := findField(result, "MusicBrainz ID")
	if mbid == nil || mbid.Status != "added" {
		t.Errorf("expected MusicBrainz ID to be added, got %v", mbid)
	}

	name := findField(result, "Name")
	if name == nil || name.Status != "unchanged" {
		t.Errorf("expected Name to be unchanged, got %v", name)
	}

	genres := findField(result, "Genres")
	if genres == nil || genres.Status != "changed" {
		t.Errorf("expected Genres to be changed, got %v", genres)
	}
}

func TestDiff_BothNil(t *testing.T) {
	result := Diff(nil, nil)
	if result.HasDiff {
		t.Error("expected no diff when both are nil")
	}
}

func findField(result *DiffResult, name string) *FieldDiff {
	for i := range result.Fields {
		if result.Fields[i].Field == name {
			return &result.Fields[i]
		}
	}
	return nil
}
