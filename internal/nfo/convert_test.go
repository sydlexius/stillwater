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
