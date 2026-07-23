package nfo

import (
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

func TestFromArtist(t *testing.T) {
	a := &artist.Artist{
		Name:          "Radiohead",
		MusicBrainzID: "a74b1b7f-71a5-4011-9441-d0b5e4122711",
		AudioDBID:     "111234",
		Genres:        []string{"Alternative Rock"},
		Biography:     "English rock band.",
		DiscogsID:     "3840",
		WikidataID:    "Q44191",
	}

	n := FromArtist(a)

	if n.Name != "Radiohead" {
		t.Errorf("Name = %q, want %q", n.Name, "Radiohead")
	}
	if n.MusicBrainzArtistID != "a74b1b7f-71a5-4011-9441-d0b5e4122711" {
		t.Errorf("MusicBrainzArtistID = %q", n.MusicBrainzArtistID)
	}
	if n.AudioDBArtistID != "111234" {
		t.Errorf("AudioDBArtistID = %q", n.AudioDBArtistID)
	}
	if n.DiscogsArtistID != "3840" {
		t.Errorf("DiscogsArtistID = %q, want %q", n.DiscogsArtistID, "3840")
	}
	if n.WikidataID != "Q44191" {
		t.Errorf("WikidataID = %q, want %q", n.WikidataID, "Q44191")
	}
	if len(n.Genres) != 1 || n.Genres[0] != "Alternative Rock" {
		t.Errorf("Genres = %v", n.Genres)
	}
}

func TestConversionRoundTrip(t *testing.T) {
	a := &artist.Artist{
		Name:           "Pink Floyd",
		SortName:       "Pink Floyd",
		Type:           "group",
		Disambiguation: "English rock band",
		MusicBrainzID:  "83d91898-7763-47d7-b03b-b92132375c47",
		DiscogsID:      "60317",
		WikidataID:     "Q2306",
		Genres:         []string{"Progressive Rock", "Psychedelic Rock"},
		Styles:         []string{"Art Rock"},
		Moods:          []string{"Atmospheric"},
		YearsActive:    "1965 - 1995, 2005, 2012 - 2014",
		Formed:         "1965",
		Disbanded:      "1995",
		Biography:      "Pink Floyd were an English rock band.",
	}

	result := FromArtist(a)

	if result.Name != a.Name {
		t.Errorf("Name mismatch: %q vs %q", result.Name, a.Name)
	}
	if result.MusicBrainzArtistID != a.MusicBrainzID {
		t.Errorf("MBID mismatch")
	}
	if result.DiscogsArtistID != a.DiscogsID {
		t.Errorf("DiscogsArtistID mismatch: %q vs %q", result.DiscogsArtistID, a.DiscogsID)
	}
	if result.WikidataID != a.WikidataID {
		t.Errorf("WikidataID mismatch: %q vs %q", result.WikidataID, a.WikidataID)
	}
	if len(result.Genres) != len(a.Genres) {
		t.Errorf("Genres count mismatch: %d vs %d", len(result.Genres), len(a.Genres))
	}
	if result.YearsActive != a.YearsActive {
		t.Errorf("YearsActive mismatch: %q vs %q", result.YearsActive, a.YearsActive)
	}
}

func TestFromArtist_GenderSuppressedForGroups(t *testing.T) {
	tests := []struct {
		artistType string
		gender     string
		wantGender string
	}{
		{"solo", "male", "male"},
		{"person", "female", "female"},
		{"character", "male", "male"},
		{"group", "male", ""},
		{"orchestra", "female", ""},
		{"choir", "male", ""},
		{"", "male", "male"}, // unknown type preserves gender
	}
	for _, tt := range tests {
		t.Run(tt.artistType, func(t *testing.T) {
			a := &artist.Artist{Type: tt.artistType, Gender: tt.gender}
			n := FromArtist(a)
			if n.Gender != tt.wantGender {
				t.Errorf("FromArtist(%q).Gender = %q, want %q",
					tt.artistType, n.Gender, tt.wantGender)
			}
		})
	}
}

func TestIsIndividualType(t *testing.T) {
	for _, tt := range []struct {
		t    string
		want bool
	}{
		{"solo", true},
		{"person", true},
		{"character", true},
		{"group", false},
		{"orchestra", false},
		{"choir", false},
		{"", false},
	} {
		if got := isIndividualType(tt.t); got != tt.want {
			t.Errorf("isIndividualType(%q) = %v, want %v", tt.t, got, tt.want)
		}
	}
}

func TestToMetadataUpdate(t *testing.T) {
	n := &ArtistNFO{
		Name:                "Nirvana",
		SortName:            "Nirvana",
		Type:                "group",
		Gender:              "male",
		Disambiguation:      "American rock band",
		MusicBrainzArtistID: "mbid-123",
		AudioDBArtistID:     "audiodb-456",
		DiscogsArtistID:     "discogs-789",
		WikidataID:          "Q11649",
		DeezerArtistID:      "412",
		SpotifyArtistID:     "spotify-xyz",
		Biography:           "A band.",
		Genres:              []string{"Rock"},
		Styles:              []string{"Grunge"},
		Moods:               []string{"Aggressive"},
		YearsActive:         "1987-1994",
		Born:                "",
		Formed:              "1987",
		Died:                "",
		Disbanded:           "1994",
	}

	u := ToMetadataUpdate(n)

	if u.Name != "Nirvana" {
		t.Errorf("Name = %q, want %q", u.Name, "Nirvana")
	}
	if u.MusicBrainzID != "mbid-123" {
		t.Errorf("MusicBrainzID = %q, want %q", u.MusicBrainzID, "mbid-123")
	}
	if u.DiscogsID != "discogs-789" {
		t.Errorf("DiscogsID = %q, want %q", u.DiscogsID, "discogs-789")
	}
	if u.SpotifyID != "spotify-xyz" {
		t.Errorf("SpotifyID = %q, want %q", u.SpotifyID, "spotify-xyz")
	}
	if len(u.Genres) != 1 || u.Genres[0] != "Rock" {
		t.Errorf("Genres = %v, want [Rock]", u.Genres)
	}
	if u.Formed != "1987" {
		t.Errorf("Formed = %q, want %q", u.Formed, "1987")
	}
	if u.Disbanded != "1994" {
		t.Errorf("Disbanded = %q, want %q", u.Disbanded, "1994")
	}
	// Gender should be cleared for non-individual types.
	if u.Gender != "" {
		t.Errorf("Gender = %q, want empty for group type", u.Gender)
	}
}

func TestToMetadataUpdate_GenderKeptForSolo(t *testing.T) {
	n := &ArtistNFO{Name: "Adele", Type: "person", Gender: "female"}
	u := ToMetadataUpdate(n)
	if u.Gender != "female" {
		t.Errorf("Gender = %q, want %q for person type", u.Gender, "female")
	}
}

// TestFromArtist_Discography verifies discography entries round-trip back to NFO.
func TestFromArtist_Discography(t *testing.T) {
	a := &artist.Artist{
		Name: "Nirvana",
		Type: "group",
		Discography: []artist.DiscographyAlbum{
			{Title: "Bleach", Year: "1989"},
		},
	}
	out := FromArtist(a)
	if len(out.Albums) != 1 {
		t.Fatalf("Albums count = %d, want 1", len(out.Albums))
	}
	want := DiscographyAlbum{Title: "Bleach", Year: "1989"}
	if out.Albums[0] != want {
		t.Errorf("album differs: %+v vs %+v", out.Albums[0], want)
	}
}

// TestRoundTrip_GenderSurvivesNFOImport is the operator-journey proof for
// issue #2748: an NFO that carries no <gender> element must not erase the
// operator's stored Gender. Before the fix the import cleared the field simply
// because the file was silent about it.
//
// The artist here is a solo type, so the NFO's silence is the ONLY reason
// gender is absent. That matters: for a group type the gender would be cleared
// anyway by the post-merge type-consistency pass, and this test could then pass
// without proving anything about absence
// (TestRoundTrip_GroupGenderClearedOnNFOImport covers that case instead).
func TestRoundTrip_GenderSurvivesNFOImport(t *testing.T) {
	stored := &artist.Artist{
		Name:   "Some Singer",
		Type:   "solo",
		Gender: "female",
	}

	written := FromArtist(stored)
	// Precondition: the writer keeps gender for an individual type, so the
	// silence below is something this test creates deliberately rather than
	// something the writer did for us.
	if written.Gender != "female" {
		t.Fatalf("precondition failed: FromArtist dropped gender %q for a solo type",
			written.Gender)
	}

	// A third-party NFO that simply has no <gender> element.
	written.Gender = ""

	reimported := ToMetadataUpdate(written)
	if reimported.Gender != "" {
		t.Fatalf("precondition failed: re-imported update carries gender %q, want empty",
			reimported.Gender)
	}
	if reimported.Type != "solo" {
		t.Fatalf("precondition failed: re-imported type = %q, want %q; a non-individual "+
			"type would clear gender for a different reason", reimported.Type, "solo")
	}

	if artist.ApplyMetadata(stored, reimported, artist.NFOImport, artist.MergeOptions{}) {
		t.Error("changed = true; re-importing an NFO that adds nothing must be a no-op")
	}
	if stored.Gender != "female" {
		t.Errorf("gender = %q, want %q; the NFO was silent about gender and the "+
			"import erased the stored value", stored.Gender, "female")
	}
}

// TestRoundTrip_GroupGenderClearedOnNFOImport guards the other meaning of an
// empty Gender on the wire. Stillwater's own NFO writer suppresses <gender> for
// non-individual types, and ToMetadataUpdate does the same, so a group's update
// carries Gender: "" whether or not the source said anything. That blank is a
// deliberate "a group has no gender", and it must still take effect even though
// the #2748 fix made absence non-destructive in general.
//
// Without the post-merge gender pass this artist ends up type="group" with a
// gender still set -- a state IsIndividualType's callers treat as impossible.
func TestRoundTrip_GroupGenderClearedOnNFOImport(t *testing.T) {
	stored := &artist.Artist{
		Name:   "Some Singer",
		Type:   "solo",
		Gender: "female",
	}

	// The incoming NFO reclassifies the artist as a group and, correctly,
	// carries no <gender>.
	incoming := &ArtistNFO{Name: "Some Group", Type: "group"}

	update := ToMetadataUpdate(incoming)
	if update.Type != "group" {
		t.Fatalf("precondition failed: update type = %q, want %q", update.Type, "group")
	}
	if update.Gender != "" {
		t.Fatalf("precondition failed: update carries gender %q; this test is about "+
			"the empty-gender case", update.Gender)
	}

	if !artist.ApplyMetadata(stored, update, artist.NFOImport, artist.MergeOptions{}) {
		t.Error("changed = false; the type change and the gender clear are both changes")
	}
	if stored.Type != "group" {
		t.Errorf("type = %q, want %q", stored.Type, "group")
	}
	if stored.Gender != "" {
		t.Errorf("gender = %q, want empty; a %s cannot carry a gender",
			stored.Gender, stored.Type)
	}
}
