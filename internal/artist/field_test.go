package artist

import (
	"context"
	"testing"
)

// --- FieldValueFromArtist ---

func TestFieldValueFromArtist_NewFields(t *testing.T) {
	a := &Artist{
		Name:           "Radiohead",
		SortName:       "Radiohead",
		Disambiguation: "UK band",
		MusicBrainzID:  "a74b1b7f-71a5-4011-9441-d0b5e4122711",
		AudioDBID:      "111239",
		DiscogsID:      "54321",
		WikidataID:     "Q161103",
		DeezerID:       "5432",
	}

	tests := []struct {
		field string
		want  string
	}{
		{"name", "Radiohead"},
		{"sort_name", "Radiohead"},
		{"disambiguation", "UK band"},
		{"musicbrainz_id", "a74b1b7f-71a5-4011-9441-d0b5e4122711"},
		{"audiodb_id", "111239"},
		{"discogs_id", "54321"},
		{"wikidata_id", "Q161103"},
		{"deezer_id", "5432"},
	}

	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			got := FieldValueFromArtist(a, tt.field)
			if got != tt.want {
				t.Errorf("FieldValueFromArtist(%q) = %q, want %q", tt.field, got, tt.want)
			}
		})
	}
}

func TestFieldValueFromArtist_UnknownField(t *testing.T) {
	a := &Artist{Name: "Test"}
	got := FieldValueFromArtist(a, "nonexistent")
	if got != "" {
		t.Errorf("FieldValueFromArtist(nonexistent) = %q, want empty string", got)
	}
}

// --- IsEditableField ---

func TestIsEditableField_NewFields(t *testing.T) {
	editable := []string{
		"name", "sort_name", "disambiguation",
		"musicbrainz_id", "audiodb_id", "discogs_id", "wikidata_id", "deezer_id",
		// existing fields must still be editable
		"biography", "genres", "styles", "moods", "formed", "born",
		"disbanded", "died", "years_active", "type", "gender",
	}
	for _, f := range editable {
		if !IsEditableField(f) {
			t.Errorf("IsEditableField(%q) = false, want true", f)
		}
	}

	if IsEditableField("library_id") {
		t.Error("IsEditableField(library_id) = true, want false (non-editable column)")
	}
	if IsEditableField("path") {
		t.Error("IsEditableField(path) = true, want false (non-editable column)")
	}
}

// --- IsProviderIDField ---

func TestIsProviderIDField(t *testing.T) {
	providerFields := []string{
		"musicbrainz_id", "audiodb_id", "discogs_id", "wikidata_id", "deezer_id",
	}
	for _, f := range providerFields {
		if !IsProviderIDField(f) {
			t.Errorf("IsProviderIDField(%q) = false, want true", f)
		}
	}

	nonProviderFields := []string{
		"biography", "name", "sort_name", "disambiguation",
		"formed", "born", "type", "gender",
	}
	for _, f := range nonProviderFields {
		if IsProviderIDField(f) {
			t.Errorf("IsProviderIDField(%q) = true, want false", f)
		}
	}
}

// --- ValidateFieldUpdate ---

func TestValidateFieldUpdate(t *testing.T) {
	tests := []struct {
		name    string
		field   string
		value   string
		wantErr bool
	}{
		// name validation
		{name: "name non-empty", field: "name", value: "Radiohead", wantErr: false},
		{name: "name empty string", field: "name", value: "", wantErr: true},
		{name: "name whitespace only", field: "name", value: "   ", wantErr: true},
		// musicbrainz_id validation
		{name: "mbid valid uuid", field: "musicbrainz_id", value: "a74b1b7f-71a5-4011-9441-d0b5e4122711", wantErr: false},
		{name: "mbid empty clears", field: "musicbrainz_id", value: "", wantErr: false},
		{name: "mbid invalid format", field: "musicbrainz_id", value: "not-a-uuid", wantErr: true},
		{name: "mbid too short", field: "musicbrainz_id", value: "a74b1b7f", wantErr: true},
		{name: "mbid bad chars", field: "musicbrainz_id", value: "zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz", wantErr: true},
		// other fields are free-form
		{name: "sort_name any value", field: "sort_name", value: "", wantErr: false},
		{name: "disambiguation any value", field: "disambiguation", value: "some note", wantErr: false},
		{name: "audiodb_id any value", field: "audiodb_id", value: "12345", wantErr: false},
		{name: "biography empty ok", field: "biography", value: "", wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateFieldUpdate(tt.field, tt.value)
			if tt.wantErr && err == nil {
				t.Errorf("ValidateFieldUpdate(%q, %q) = nil, want error", tt.field, tt.value)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("ValidateFieldUpdate(%q, %q) = %v, want nil", tt.field, tt.value, err)
			}
		})
	}
}

// --- isValidMBID ---

func TestIsValidMBID(t *testing.T) {
	valid := []string{
		"a74b1b7f-71a5-4011-9441-d0b5e4122711",
		"5b11f4ce-a62d-471e-81fc-a69a8278c7da",
		"A74B1B7F-71A5-4011-9441-D0B5E4122711", // uppercase accepted
	}
	for _, v := range valid {
		if !isValidMBID(v) {
			t.Errorf("isValidMBID(%q) = false, want true", v)
		}
	}

	invalid := []string{
		"",
		"not-a-uuid",
		"a74b1b7f71a5-4011-9441-d0b5e4122711",   // missing dash
		"a74b1b7f-71a5-4011-9441-d0b5e41227110", // too long
		"a74b1b7f-71a5-4011-9441-d0b5e412271",   // too short
		"g74b1b7f-71a5-4011-9441-d0b5e4122711",  // 'g' not hex
	}
	for _, v := range invalid {
		if isValidMBID(v) {
			t.Errorf("isValidMBID(%q) = true, want false", v)
		}
	}
}

// --- UpdateProviderField / ClearProviderField ---

func TestUpdateProviderField(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Test Artist", "/music/test")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	tests := []struct {
		field    string
		value    string
		validate func(*Artist) bool
	}{
		{"musicbrainz_id", "a74b1b7f-71a5-4011-9441-d0b5e4122711", func(a *Artist) bool {
			return a.MusicBrainzID == "a74b1b7f-71a5-4011-9441-d0b5e4122711"
		}},
		{"audiodb_id", "111239", func(a *Artist) bool { return a.AudioDBID == "111239" }},
		{"discogs_id", "54321", func(a *Artist) bool { return a.DiscogsID == "54321" }},
		{"wikidata_id", "Q161103", func(a *Artist) bool { return a.WikidataID == "Q161103" }},
		{"deezer_id", "5432", func(a *Artist) bool { return a.DeezerID == "5432" }},
	}

	for _, tt := range tests {
		t.Run("update_"+tt.field, func(t *testing.T) {
			if err := svc.UpdateProviderField(ctx, a.ID, tt.field, tt.value); err != nil {
				t.Fatalf("UpdateProviderField(%q): %v", tt.field, err)
			}
			got, err := svc.GetByID(ctx, a.ID)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			if !tt.validate(got) {
				t.Errorf("after UpdateProviderField(%q, %q): field not set as expected", tt.field, tt.value)
			}
		})
	}
}

func TestClearProviderField(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Test Artist", "/music/test")
	a.MusicBrainzID = "a74b1b7f-71a5-4011-9441-d0b5e4122711"
	a.DiscogsID = "54321"
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := svc.ClearProviderField(ctx, a.ID, "musicbrainz_id"); err != nil {
		t.Fatalf("ClearProviderField(musicbrainz_id): %v", err)
	}

	got, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.MusicBrainzID != "" {
		t.Errorf("after clear: MusicBrainzID = %q, want empty", got.MusicBrainzID)
	}
	// Discogs should be untouched
	if got.DiscogsID != "54321" {
		t.Errorf("DiscogsID = %q after clearing musicbrainz_id, want 54321", got.DiscogsID)
	}
}

// --- FieldPickerOptions ---

func TestFieldPickerOptions(t *testing.T) {
	wantGenders := []string{"male", "female", "non-binary", "other", "unknown"}
	if got := FieldPickerOptions("gender"); !slicesEqual(got, wantGenders) {
		t.Errorf("FieldPickerOptions(gender) = %v, want %v", got, wantGenders)
	}

	wantTypes := []string{"person", "group", "orchestra", "choir", "character", "other"}
	if got := FieldPickerOptions("type"); !slicesEqual(got, wantTypes) {
		t.Errorf("FieldPickerOptions(type) = %v, want %v", got, wantTypes)
	}

	if got := FieldPickerOptions("name"); got != nil {
		t.Errorf("FieldPickerOptions(name) = %v, want nil", got)
	}
	if got := FieldPickerOptions("biography"); got != nil {
		t.Errorf("FieldPickerOptions(biography) = %v, want nil", got)
	}
}

func slicesEqual(a, b []string) bool {
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

func TestUpdateField_NameAndSortName(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Original Name", "/music/test")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := svc.UpdateField(ctx, a.ID, "name", "New Name"); err != nil {
		t.Fatalf("UpdateField(name): %v", err)
	}
	if err := svc.UpdateField(ctx, a.ID, "sort_name", "Name, New"); err != nil {
		t.Fatalf("UpdateField(sort_name): %v", err)
	}
	if err := svc.UpdateField(ctx, a.ID, "disambiguation", "the famous one"); err != nil {
		t.Fatalf("UpdateField(disambiguation): %v", err)
	}

	got, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name != "New Name" {
		t.Errorf("Name = %q, want 'New Name'", got.Name)
	}
	if got.SortName != "Name, New" {
		t.Errorf("SortName = %q, want 'Name, New'", got.SortName)
	}
	if got.Disambiguation != "the famous one" {
		t.Errorf("Disambiguation = %q, want 'the famous one'", got.Disambiguation)
	}
}
