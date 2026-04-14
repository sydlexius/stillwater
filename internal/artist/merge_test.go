package artist

import (
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

func TestIsIndividualType(t *testing.T) {
	individual := []string{"solo", "person", "character"}
	for _, typ := range individual {
		if !IsIndividualType(typ) {
			t.Errorf("IsIndividualType(%q) = false, want true", typ)
		}
	}
	nonIndividual := []string{"group", "orchestra", "choir", "other", ""}
	for _, typ := range nonIndividual {
		if IsIndividualType(typ) {
			t.Errorf("IsIndividualType(%q) = true, want false", typ)
		}
	}
}

func TestApplyMetadata_NilUpdate(t *testing.T) {
	a := &Artist{Name: "Test"}
	if ApplyMetadata(a, nil, FillEmpty, MergeOptions{}) {
		t.Error("expected no change for nil update")
	}
}

// --- OverwriteAttempted tests ---

func TestOverwriteAttempted_OverwritesAttemptedFields(t *testing.T) {
	a := &Artist{Biography: "old bio", Born: "1970"}
	u := &MetadataUpdate{Biography: "new bio", Born: "1980", Genres: []string{"rock"}}
	changed := ApplyMetadata(a, u, OverwriteAttempted, MergeOptions{
		AttemptedFields: []string{"biography", "born", "genres"},
	})
	if !changed {
		t.Error("expected change")
	}
	if a.Biography != "new bio" {
		t.Errorf("biography = %q, want %q", a.Biography, "new bio")
	}
	if a.Born != "1980" {
		t.Errorf("born = %q, want %q", a.Born, "1980")
	}
	if len(a.Genres) != 1 || a.Genres[0] != "rock" {
		t.Errorf("genres = %v, want [rock]", a.Genres)
	}
}

func TestOverwriteAttempted_ClearsEmptyAttemptedFields(t *testing.T) {
	a := &Artist{Biography: "old bio", Genres: []string{"jazz"}}
	u := &MetadataUpdate{} // empty values for attempted fields = clear
	changed := ApplyMetadata(a, u, OverwriteAttempted, MergeOptions{
		AttemptedFields: []string{"biography", "genres"},
	})
	if !changed {
		t.Error("expected change")
	}
	if a.Biography != "" {
		t.Errorf("biography should be cleared, got %q", a.Biography)
	}
	if a.Genres != nil {
		t.Errorf("genres should be nil, got %v", a.Genres)
	}
}

func TestOverwriteAttempted_PreservesUnattemptedFields(t *testing.T) {
	a := &Artist{Biography: "keep me", Born: "1970"}
	u := &MetadataUpdate{Biography: "discard", Born: "discard"}
	ApplyMetadata(a, u, OverwriteAttempted, MergeOptions{
		AttemptedFields: []string{}, // nothing attempted
	})
	if a.Biography != "keep me" {
		t.Errorf("biography should be preserved, got %q", a.Biography)
	}
	if a.Born != "1970" {
		t.Errorf("born should be preserved, got %q", a.Born)
	}
}

func TestOverwriteAttempted_TypeGenderNeverCleared(t *testing.T) {
	a := &Artist{Type: "person", Gender: "male"}
	u := &MetadataUpdate{Type: "", Gender: ""}
	ApplyMetadata(a, u, OverwriteAttempted, MergeOptions{
		AttemptedFields: []string{"type", "gender"},
	})
	if a.Type != "person" {
		t.Errorf("type should not be cleared, got %q", a.Type)
	}
	if a.Gender != "male" {
		t.Errorf("gender should not be cleared, got %q", a.Gender)
	}
}

func TestOverwriteAttempted_TypeGenderOverwriteWhenNonEmpty(t *testing.T) {
	a := &Artist{Type: "person", Gender: "male"}
	u := &MetadataUpdate{Type: "group", Gender: "female"}
	changed := ApplyMetadata(a, u, OverwriteAttempted, MergeOptions{})
	if !changed {
		t.Error("expected change")
	}
	if a.Type != "group" {
		t.Errorf("type = %q, want %q", a.Type, "group")
	}
	if a.Gender != "female" {
		t.Errorf("gender = %q, want %q", a.Gender, "female")
	}
}

func TestOverwriteAttempted_YearsActiveNonEmptyOnly(t *testing.T) {
	a := &Artist{YearsActive: "1990-2000"}
	u := &MetadataUpdate{YearsActive: ""}
	ApplyMetadata(a, u, OverwriteAttempted, MergeOptions{})
	if a.YearsActive != "1990-2000" {
		t.Errorf("years_active should not be cleared, got %q", a.YearsActive)
	}

	u.YearsActive = "2000-2010"
	changed := ApplyMetadata(a, u, OverwriteAttempted, MergeOptions{})
	if !changed || a.YearsActive != "2000-2010" {
		t.Errorf("years_active = %q, want %q", a.YearsActive, "2000-2010")
	}
}

func TestOverwriteAttempted_ProviderIDsFillEmptyOnly(t *testing.T) {
	a := &Artist{MusicBrainzID: "existing-mbid"}
	u := &MetadataUpdate{
		MusicBrainzID: "new-mbid",
		AudioDBID:     "new-audiodb",
		DiscogsID:     "new-discogs",
	}
	changed := ApplyMetadata(a, u, OverwriteAttempted, MergeOptions{})
	if !changed {
		t.Error("expected change for new AudioDBID/DiscogsID")
	}
	if a.MusicBrainzID != "existing-mbid" {
		t.Errorf("existing MBID should be preserved, got %q", a.MusicBrainzID)
	}
	if a.AudioDBID != "new-audiodb" {
		t.Errorf("AudioDBID = %q, want %q", a.AudioDBID, "new-audiodb")
	}
	if a.DiscogsID != "new-discogs" {
		t.Errorf("DiscogsID = %q, want %q", a.DiscogsID, "new-discogs")
	}
}

func TestOverwriteAttempted_SkipsNameSortName(t *testing.T) {
	a := &Artist{Name: "Original", SortName: "Original, The", Disambiguation: "UK"}
	u := &MetadataUpdate{Name: "Changed", SortName: "Changed, The", Disambiguation: "US"}
	ApplyMetadata(a, u, OverwriteAttempted, MergeOptions{
		AttemptedFields: []string{"name", "sort_name", "disambiguation"},
	})
	if a.Name != "Original" {
		t.Errorf("Name should not change, got %q", a.Name)
	}
	if a.SortName != "Original, The" {
		t.Errorf("SortName should not change, got %q", a.SortName)
	}
	// Disambiguation now follows Type/Gender semantics: non-empty overwrite
	// so a provider-supplied value replaces any stale local value without
	// waiting for a separate NFO import pass.
	if a.Disambiguation != "US" {
		t.Errorf("Disambiguation should be overwritten, got %q", a.Disambiguation)
	}
}

// TestOverwriteAttempted_DisambiguationNonEmptyOnly verifies that an empty
// provider Disambiguation does NOT clear a populated local value.
func TestOverwriteAttempted_DisambiguationNonEmptyOnly(t *testing.T) {
	a := &Artist{Disambiguation: "UK rock band"}
	u := &MetadataUpdate{Disambiguation: ""}
	ApplyMetadata(a, u, OverwriteAttempted, MergeOptions{})
	if a.Disambiguation != "UK rock band" {
		t.Errorf("Disambiguation = %q, want preserved %q", a.Disambiguation, "UK rock band")
	}
}

func TestOverwriteAttempted_FilterDatesByType(t *testing.T) {
	tests := []struct {
		name          string
		artistType    string
		wantBorn      string
		wantFormed    string
		wantDied      string
		wantDisbanded string
	}{
		{"solo clears formed/disbanded", "solo", "1970", "", "2020", ""},
		{"person clears formed/disbanded", "person", "1970", "", "2020", ""},
		{"character clears formed/disbanded", "character", "1970", "", "2020", ""},
		{"group clears born/died", "group", "", "1980", "", "2000"},
		{"orchestra clears born/died", "orchestra", "", "1980", "", "2000"},
		{"choir clears born/died", "choir", "", "1980", "", "2000"},
		{"empty type preserves all", "", "1970", "1980", "2020", "2000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &Artist{Type: tt.artistType}
			u := &MetadataUpdate{Born: "1970", Formed: "1980", Died: "2020", Disbanded: "2000"}
			ApplyMetadata(a, u, OverwriteAttempted, MergeOptions{
				AttemptedFields:   []string{"born", "formed", "died", "disbanded"},
				FilterDatesByType: true,
			})
			if a.Born != tt.wantBorn {
				t.Errorf("born = %q, want %q", a.Born, tt.wantBorn)
			}
			if a.Formed != tt.wantFormed {
				t.Errorf("formed = %q, want %q", a.Formed, tt.wantFormed)
			}
			if a.Died != tt.wantDied {
				t.Errorf("died = %q, want %q", a.Died, tt.wantDied)
			}
			if a.Disbanded != tt.wantDisbanded {
				t.Errorf("disbanded = %q, want %q", a.Disbanded, tt.wantDisbanded)
			}
		})
	}
}

func TestOverwriteAttempted_Sources(t *testing.T) {
	a := &Artist{}
	u := &MetadataUpdate{Biography: "bio"}
	ApplyMetadata(a, u, OverwriteAttempted, MergeOptions{
		AttemptedFields: []string{"biography"},
		Sources: []provider.FieldSource{
			{Field: "biography", Provider: provider.NameMusicBrainz},
			{Field: "genres", Provider: provider.NameAudioDB},
		},
	})
	if a.MetadataSources == nil {
		t.Fatal("MetadataSources should be populated")
	}
	if a.MetadataSources["biography"] != string(provider.NameMusicBrainz) {
		t.Errorf("biography source = %q, want %q", a.MetadataSources["biography"], provider.NameMusicBrainz)
	}
	if a.MetadataSources["genres"] != string(provider.NameAudioDB) {
		t.Errorf("genres source = %q, want %q", a.MetadataSources["genres"], provider.NameAudioDB)
	}
}

func TestOverwriteAttempted_AllAttemptedSliceFields(t *testing.T) {
	a := &Artist{}
	u := &MetadataUpdate{
		Styles: []string{"blues"},
		Moods:  []string{"melancholy"},
	}
	changed := ApplyMetadata(a, u, OverwriteAttempted, MergeOptions{
		AttemptedFields: []string{"styles", "moods"},
	})
	if !changed {
		t.Error("expected change")
	}
	if len(a.Styles) != 1 || a.Styles[0] != "blues" {
		t.Errorf("styles = %v, want [blues]", a.Styles)
	}
	if len(a.Moods) != 1 || a.Moods[0] != "melancholy" {
		t.Errorf("moods = %v, want [melancholy]", a.Moods)
	}
}

// --- FillEmpty tests ---

func TestFillEmpty_OnlyFillsEmptyFields(t *testing.T) {
	a := &Artist{
		Type:          "person",
		MusicBrainzID: "existing",
		DiscogsID:     "existing-discogs",
		Biography:     "existing bio",
		Genres:        []string{"rock"},
		YearsActive:   "1990-2000",
		Born:          "1970",
	}
	u := &MetadataUpdate{
		Type:          "group",
		Gender:        "female",
		MusicBrainzID: "new-mbid",
		AudioDBID:     "new-audiodb",
		DiscogsID:     "new-discogs",
		WikidataID:    "new-wikidata",
		DeezerID:      "new-deezer",
		SpotifyID:     "new-spotify",
		Biography:     "new bio",
		Genres:        []string{"jazz"},
		Styles:        []string{"swing"},
		Moods:         []string{"happy"},
		YearsActive:   "2000-2010",
		Born:          "1990",
		Formed:        "1985",
		Died:          "2020",
		Disbanded:     "2010",
	}
	changed := ApplyMetadata(a, u, FillEmpty, MergeOptions{})
	if !changed {
		t.Error("expected change")
	}
	// Existing fields preserved.
	if a.Type != "person" {
		t.Errorf("type should be preserved, got %q", a.Type)
	}
	if a.MusicBrainzID != "existing" {
		t.Errorf("MBID should be preserved, got %q", a.MusicBrainzID)
	}
	if a.DiscogsID != "existing-discogs" {
		t.Errorf("DiscogsID should be preserved, got %q", a.DiscogsID)
	}
	if a.Biography != "existing bio" {
		t.Errorf("biography should be preserved, got %q", a.Biography)
	}
	if len(a.Genres) != 1 || a.Genres[0] != "rock" {
		t.Errorf("genres should be preserved, got %v", a.Genres)
	}
	if a.YearsActive != "1990-2000" {
		t.Errorf("years_active should be preserved, got %q", a.YearsActive)
	}
	if a.Born != "1970" {
		t.Errorf("born should be preserved, got %q", a.Born)
	}
	// Empty fields filled.
	if a.Gender != "female" {
		t.Errorf("gender = %q, want %q", a.Gender, "female")
	}
	if a.AudioDBID != "new-audiodb" {
		t.Errorf("AudioDBID = %q, want %q", a.AudioDBID, "new-audiodb")
	}
	if a.WikidataID != "new-wikidata" {
		t.Errorf("WikidataID = %q, want %q", a.WikidataID, "new-wikidata")
	}
	if a.DeezerID != "new-deezer" {
		t.Errorf("DeezerID = %q, want %q", a.DeezerID, "new-deezer")
	}
	if a.SpotifyID != "new-spotify" {
		t.Errorf("SpotifyID = %q, want %q", a.SpotifyID, "new-spotify")
	}
	if len(a.Styles) != 1 || a.Styles[0] != "swing" {
		t.Errorf("styles = %v, want [swing]", a.Styles)
	}
	if len(a.Moods) != 1 || a.Moods[0] != "happy" {
		t.Errorf("moods = %v, want [happy]", a.Moods)
	}
	if a.Formed != "1985" {
		t.Errorf("formed = %q, want %q", a.Formed, "1985")
	}
	if a.Died != "2020" {
		t.Errorf("died = %q, want %q", a.Died, "2020")
	}
	if a.Disbanded != "2010" {
		t.Errorf("disbanded = %q, want %q", a.Disbanded, "2010")
	}
}

func TestFillEmpty_SkipsNameSortNameDisambiguation(t *testing.T) {
	a := &Artist{}
	u := &MetadataUpdate{Name: "New", SortName: "New, The", Disambiguation: "UK"}
	ApplyMetadata(a, u, FillEmpty, MergeOptions{})
	if a.Name != "" {
		t.Errorf("Name should not be set by FillEmpty, got %q", a.Name)
	}
	if a.SortName != "" {
		t.Errorf("SortName should not be set by FillEmpty, got %q", a.SortName)
	}
	if a.Disambiguation != "" {
		t.Errorf("Disambiguation should not be set by FillEmpty, got %q", a.Disambiguation)
	}
}

func TestFillEmpty_ReturnsFalseWhenNothingChanged(t *testing.T) {
	a := &Artist{
		Type: "group", Gender: "mixed", MusicBrainzID: "mbid",
		Biography: "bio", Genres: []string{"rock"}, Born: "1980",
	}
	u := &MetadataUpdate{
		Type: "person", Gender: "female", MusicBrainzID: "other",
		Biography: "other bio", Genres: []string{"jazz"}, Born: "1990",
	}
	if ApplyMetadata(a, u, FillEmpty, MergeOptions{}) {
		t.Error("expected no change when all target fields are already populated")
	}
}

// --- NFOImport tests ---

func TestNFOImport_IdentityFieldsNonEmptyOverwrite(t *testing.T) {
	a := &Artist{Name: "Old Name", SortName: "Old", Biography: "old bio", MusicBrainzID: "old-mbid"}
	u := &MetadataUpdate{Name: "New Name", SortName: "New", Biography: "new bio", MusicBrainzID: "new-mbid"}
	changed := ApplyMetadata(a, u, NFOImport, MergeOptions{})
	if !changed {
		t.Error("expected change")
	}
	if a.Name != "New Name" {
		t.Errorf("name = %q, want %q", a.Name, "New Name")
	}
	if a.MusicBrainzID != "new-mbid" {
		t.Errorf("MBID = %q, want %q", a.MusicBrainzID, "new-mbid")
	}

	// Empty incoming preserves existing.
	a2 := &Artist{Name: "Keep", Biography: "keep bio"}
	u2 := &MetadataUpdate{Name: "", Biography: ""}
	ApplyMetadata(a2, u2, NFOImport, MergeOptions{})
	if a2.Name != "Keep" {
		t.Errorf("name should be preserved when incoming is empty, got %q", a2.Name)
	}
	if a2.Biography != "keep bio" {
		t.Errorf("biography should be preserved when incoming is empty, got %q", a2.Biography)
	}
}

func TestNFOImport_ProviderIDsNonEmptyOverwrite(t *testing.T) {
	a := &Artist{DiscogsID: "old-discogs"}
	u := &MetadataUpdate{
		DiscogsID:  "new-discogs",
		WikidataID: "new-wikidata",
		DeezerID:   "new-deezer",
		SpotifyID:  "new-spotify",
	}
	changed := ApplyMetadata(a, u, NFOImport, MergeOptions{})
	if !changed {
		t.Error("expected change")
	}
	if a.DiscogsID != "new-discogs" {
		t.Errorf("DiscogsID = %q, want %q", a.DiscogsID, "new-discogs")
	}
	if a.WikidataID != "new-wikidata" {
		t.Errorf("WikidataID = %q, want %q", a.WikidataID, "new-wikidata")
	}
	if a.DeezerID != "new-deezer" {
		t.Errorf("DeezerID = %q, want %q", a.DeezerID, "new-deezer")
	}
	if a.SpotifyID != "new-spotify" {
		t.Errorf("SpotifyID = %q, want %q", a.SpotifyID, "new-spotify")
	}
}

func TestNFOImport_ProviderIDsPreservedWhenEmpty(t *testing.T) {
	a := &Artist{MusicBrainzID: "existing-mbid", DiscogsID: "existing-discogs"}
	u := &MetadataUpdate{} // all empty
	ApplyMetadata(a, u, NFOImport, MergeOptions{})
	if a.MusicBrainzID != "existing-mbid" {
		t.Errorf("MusicBrainzID should be preserved, got %q", a.MusicBrainzID)
	}
	if a.DiscogsID != "existing-discogs" {
		t.Errorf("DiscogsID should be preserved, got %q", a.DiscogsID)
	}
}

func TestNFOImport_ClassificationUnconditional(t *testing.T) {
	a := &Artist{Type: "person", Gender: "male", Disambiguation: "UK"}
	u := &MetadataUpdate{Type: "", Gender: "", Disambiguation: ""}
	changed := ApplyMetadata(a, u, NFOImport, MergeOptions{})
	if !changed {
		t.Error("expected change")
	}
	if a.Type != "" {
		t.Errorf("type should be cleared unconditionally, got %q", a.Type)
	}
	if a.Gender != "" {
		t.Errorf("gender should be cleared unconditionally, got %q", a.Gender)
	}
	if a.Disambiguation != "" {
		t.Errorf("disambiguation should be cleared unconditionally, got %q", a.Disambiguation)
	}
}

func TestNFOImport_ListsAndDatesUnconditional(t *testing.T) {
	a := &Artist{
		Genres: []string{"rock"}, Styles: []string{"grunge"}, Moods: []string{"angry"},
		Born: "1970", Formed: "1980", Died: "2020", Disbanded: "2000", YearsActive: "1980-2000",
	}
	u := &MetadataUpdate{} // all zero values
	changed := ApplyMetadata(a, u, NFOImport, MergeOptions{})
	if !changed {
		t.Error("expected change")
	}
	if a.Genres != nil {
		t.Errorf("genres should be cleared, got %v", a.Genres)
	}
	if a.Born != "" {
		t.Errorf("born should be cleared, got %q", a.Born)
	}
	if a.YearsActive != "" {
		t.Errorf("years_active should be cleared, got %q", a.YearsActive)
	}
}

// --- SnapshotRestore tests ---

func TestSnapshotRestore_AllFieldsUnconditional(t *testing.T) {
	a := &Artist{
		Name: "Old", SortName: "Old", Type: "person", Gender: "male",
		Disambiguation: "UK", MusicBrainzID: "old-mbid", Biography: "old bio",
		Genres: []string{"rock"}, Born: "1970",
	}
	u := &MetadataUpdate{
		Name: "New", SortName: "New", Type: "group", Gender: "",
		Disambiguation: "", MusicBrainzID: "new-mbid", Biography: "",
		Genres: []string{"jazz"}, Born: "",
	}
	changed := ApplyMetadata(a, u, SnapshotRestore, MergeOptions{})
	if !changed {
		t.Error("expected change")
	}
	if a.Name != "New" {
		t.Errorf("name = %q, want %q", a.Name, "New")
	}
	if a.Gender != "" {
		t.Errorf("gender should be cleared, got %q", a.Gender)
	}
	if a.MusicBrainzID != "new-mbid" {
		t.Errorf("MBID = %q, want %q", a.MusicBrainzID, "new-mbid")
	}
	if a.Biography != "" {
		t.Errorf("biography should be cleared, got %q", a.Biography)
	}
	if a.Born != "" {
		t.Errorf("born should be cleared, got %q", a.Born)
	}
}

func TestSnapshotRestore_PreservesNonMetadataFields(t *testing.T) {
	a := &Artist{
		ID: "test-id", Path: "/music/Test", LibraryID: "lib-1",
		NFOExists: true, ThumbExists: true, HealthScore: 85.5,
	}
	u := &MetadataUpdate{Name: "Restored"}
	ApplyMetadata(a, u, SnapshotRestore, MergeOptions{})
	if a.ID != "test-id" {
		t.Errorf("ID should be preserved, got %q", a.ID)
	}
	if a.Path != "/music/Test" {
		t.Errorf("Path should be preserved, got %q", a.Path)
	}
	if a.LibraryID != "lib-1" {
		t.Errorf("LibraryID should be preserved, got %q", a.LibraryID)
	}
	if !a.NFOExists {
		t.Error("NFOExists should be preserved")
	}
	if a.HealthScore != 85.5 {
		t.Errorf("HealthScore should be preserved, got %f", a.HealthScore)
	}
}

// --- Changed return value ---

func TestApplyMetadata_ReturnsFalseWhenNoChange(t *testing.T) {
	a := &Artist{Name: "Same", Type: "group", Biography: "bio"}
	u := &MetadataUpdate{Name: "Same", Type: "group", Biography: "bio"}
	if ApplyMetadata(a, u, SnapshotRestore, MergeOptions{}) {
		t.Error("expected no change when values are identical")
	}
}

// --- FilterDatesByArtistType ---

func TestFilterDatesByArtistType(t *testing.T) {
	tests := []struct {
		name              string
		artistType        string
		wantBornCleared   bool
		wantFormedCleared bool
	}{
		{"solo", "solo", false, true},
		{"person", "person", false, true},
		{"character", "character", false, true},
		{"group", "group", true, false},
		{"orchestra", "orchestra", true, false},
		{"choir", "choir", true, false},
		{"empty", "", false, false},
		{"unknown", "unknown", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &Artist{Type: tt.artistType, Born: "1970", Formed: "1980", Died: "2020", Disbanded: "2000"}
			FilterDatesByArtistType(a)
			if tt.wantBornCleared && a.Born != "" {
				t.Errorf("born should be cleared for type %q", tt.artistType)
			}
			if !tt.wantBornCleared && a.Born != "1970" {
				t.Errorf("born should be preserved for type %q", tt.artistType)
			}
			if tt.wantFormedCleared && a.Formed != "" {
				t.Errorf("formed should be cleared for type %q", tt.artistType)
			}
			if !tt.wantFormedCleared && a.Formed != "1980" {
				t.Errorf("formed should be preserved for type %q", tt.artistType)
			}
		})
	}
}

// --- FetchResultToUpdate ---

func TestFetchResultToUpdate(t *testing.T) {
	result := &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			Name: "Test", SortName: "Test, The", Type: "group", Gender: "mixed",
			Disambiguation: "UK", MusicBrainzID: "mbid", AudioDBID: "audiodb",
			DiscogsID: "discogs", WikidataID: "wikidata", DeezerID: "deezer",
			SpotifyID: "spotify", Biography: "A band.", Genres: []string{"rock"},
			Styles: []string{"indie"}, Moods: []string{"energetic"},
			YearsActive: "2000-", Born: "1980", Formed: "2000",
			Died: "", Disbanded: "",
		},
	}
	u := FetchResultToUpdate(result)
	if u == nil {
		t.Fatal("expected non-nil update")
	}
	if u.Name != "Test" {
		t.Errorf("Name = %q, want %q", u.Name, "Test")
	}
	if u.SpotifyID != "spotify" {
		t.Errorf("SpotifyID = %q, want %q", u.SpotifyID, "spotify")
	}
	if len(u.Genres) != 1 || u.Genres[0] != "rock" {
		t.Errorf("Genres = %v, want [rock]", u.Genres)
	}
	// Gender should be cleared for non-individual types.
	if u.Gender != "" {
		t.Errorf("Gender = %q, want empty for group type", u.Gender)
	}
}

func TestFetchResultToUpdate_IndividualKeepsGender(t *testing.T) {
	result := &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			Name: "Solo Artist", Type: "solo", Gender: "female",
		},
	}
	u := FetchResultToUpdate(result)
	if u.Gender != "female" {
		t.Errorf("Gender = %q, want %q for solo type", u.Gender, "female")
	}
}

func TestFetchResultToUpdate_UnknownTypeKeepsGender(t *testing.T) {
	result := &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			Name: "Unknown", Type: "", Gender: "male",
		},
	}
	u := FetchResultToUpdate(result)
	if u.Gender != "male" {
		t.Errorf("Gender = %q, want %q for unknown type", u.Gender, "male")
	}
}

func TestFetchResultToUpdate_NilMetadata(t *testing.T) {
	if FetchResultToUpdate(nil) != nil {
		t.Error("expected nil for nil result")
	}
	if FetchResultToUpdate(&provider.FetchResult{}) != nil {
		t.Error("expected nil for nil metadata")
	}
}
