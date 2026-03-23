package nfo

import (
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

func TestToArtist(t *testing.T) {
	n := &ArtistNFO{
		Name:                "Nirvana",
		SortName:            "Nirvana",
		Type:                "group",
		MusicBrainzArtistID: "5b11f4ce-a62d-471e-81fc-a69a8278c7da",
		AudioDBArtistID:     "111239",
		DiscogsArtistID:     "125246",
		WikidataID:          "Q11649",
		Genres:              []string{"Rock", "Grunge"},
		Styles:              []string{"Grunge"},
		Moods:               []string{"Aggressive"},
		YearsActive:         "1987 - 1994",
		Formed:              "1987",
		Disbanded:           "1994",
		Biography:           "American rock band.",
	}

	a := ToArtist(n)

	if a.Name != "Nirvana" {
		t.Errorf("Name = %q, want %q", a.Name, "Nirvana")
	}
	if a.MusicBrainzID != "5b11f4ce-a62d-471e-81fc-a69a8278c7da" {
		t.Errorf("MusicBrainzID = %q", a.MusicBrainzID)
	}
	if a.AudioDBID != "111239" {
		t.Errorf("AudioDBID = %q", a.AudioDBID)
	}
	if a.DiscogsID != "125246" {
		t.Errorf("DiscogsID = %q, want %q", a.DiscogsID, "125246")
	}
	if a.WikidataID != "Q11649" {
		t.Errorf("WikidataID = %q, want %q", a.WikidataID, "Q11649")
	}
	if len(a.Genres) != 2 {
		t.Errorf("Genres count = %d, want 2", len(a.Genres))
	}
	if a.Formed != "1987" {
		t.Errorf("Formed = %q, want %q", a.Formed, "1987")
	}
}

func TestFromArtist(t *testing.T) {
	a := ToArtist(&ArtistNFO{
		Name:                "Radiohead",
		MusicBrainzArtistID: "a74b1b7f-71a5-4011-9441-d0b5e4122711",
		AudioDBArtistID:     "111234",
		Genres:              []string{"Alternative Rock"},
		Biography:           "English rock band.",
	})
	// Set fields that only exist on Artist, not on NFO
	a.DiscogsID = "3840"
	a.WikidataID = "Q44191"

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
	original := &ArtistNFO{
		Name:                "Pink Floyd",
		SortName:            "Pink Floyd",
		Type:                "group",
		Disambiguation:      "English rock band",
		MusicBrainzArtistID: "83d91898-7763-47d7-b03b-b92132375c47",
		DiscogsArtistID:     "60317",
		WikidataID:          "Q2306",
		Genres:              []string{"Progressive Rock", "Psychedelic Rock"},
		Styles:              []string{"Art Rock"},
		Moods:               []string{"Atmospheric"},
		YearsActive:         "1965 - 1995, 2005, 2012 - 2014",
		Formed:              "1965",
		Disbanded:           "1995",
		Biography:           "Pink Floyd were an English rock band.",
	}

	a := ToArtist(original)
	result := FromArtist(a)

	if result.Name != original.Name {
		t.Errorf("Name mismatch: %q vs %q", result.Name, original.Name)
	}
	if result.MusicBrainzArtistID != original.MusicBrainzArtistID {
		t.Errorf("MBID mismatch")
	}
	if result.DiscogsArtistID != original.DiscogsArtistID {
		t.Errorf("DiscogsArtistID mismatch: %q vs %q", result.DiscogsArtistID, original.DiscogsArtistID)
	}
	if result.WikidataID != original.WikidataID {
		t.Errorf("WikidataID mismatch: %q vs %q", result.WikidataID, original.WikidataID)
	}
	if len(result.Genres) != len(original.Genres) {
		t.Errorf("Genres count mismatch: %d vs %d", len(result.Genres), len(original.Genres))
	}
	if result.YearsActive != original.YearsActive {
		t.Errorf("YearsActive mismatch: %q vs %q", result.YearsActive, original.YearsActive)
	}
}

func TestApplyNFOToArtist(t *testing.T) {
	// Start with an artist that has non-NFO fields set.
	a := &artist.Artist{
		ID:        "keep-id",
		Path:      "/music/original",
		LibraryID: "lib-1",
		Name:      "Old Name",
		SortName:  "Old Name",
		// Image flags that should be preserved.
		ThumbExists:  true,
		FanartExists: true,
	}

	n := &ArtistNFO{
		Name:                "Nirvana",
		SortName:            "Nirvana",
		Type:                "group",
		Gender:              "male",
		Disambiguation:      "American rock band",
		MusicBrainzArtistID: "5b11f4ce-a62d-471e-81fc-a69a8278c7da",
		AudioDBArtistID:     "111239",
		DiscogsArtistID:     "125246",
		WikidataID:          "Q11649",
		DeezerArtistID:      "412",
		SpotifyArtistID:     "6olE6TJLqED3rqDCT0FyPh",
		Genres:              []string{"Rock", "Grunge"},
		Styles:              []string{"Grunge"},
		Moods:               []string{"Aggressive"},
		YearsActive:         "1987 - 1994",
		Born:                "",
		Formed:              "1987",
		Died:                "",
		Disbanded:           "1994",
		Biography:           "American rock band.",
	}

	ApplyNFOToArtist(n, a)

	// NFO fields should be updated.
	if a.Name != "Nirvana" {
		t.Errorf("Name = %q, want %q", a.Name, "Nirvana")
	}
	if a.Type != "group" {
		t.Errorf("Type = %q, want %q", a.Type, "group")
	}
	if a.MusicBrainzID != "5b11f4ce-a62d-471e-81fc-a69a8278c7da" {
		t.Errorf("MusicBrainzID = %q", a.MusicBrainzID)
	}
	if a.DeezerID != "412" {
		t.Errorf("DeezerID = %q, want %q", a.DeezerID, "412")
	}
	if a.SpotifyID != "6olE6TJLqED3rqDCT0FyPh" {
		t.Errorf("SpotifyID = %q", a.SpotifyID)
	}
	if len(a.Genres) != 2 {
		t.Errorf("Genres count = %d, want 2", len(a.Genres))
	}
	if a.Disbanded != "1994" {
		t.Errorf("Disbanded = %q, want %q", a.Disbanded, "1994")
	}

	// Non-NFO fields should be preserved.
	if a.ID != "keep-id" {
		t.Errorf("ID = %q, want %q (should be preserved)", a.ID, "keep-id")
	}
	if a.Path != "/music/original" {
		t.Errorf("Path = %q, want %q (should be preserved)", a.Path, "/music/original")
	}
	if a.LibraryID != "lib-1" {
		t.Errorf("LibraryID = %q, want %q (should be preserved)", a.LibraryID, "lib-1")
	}
	if !a.ThumbExists {
		t.Error("ThumbExists should be preserved as true")
	}
	if !a.FanartExists {
		t.Error("FanartExists should be preserved as true")
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
}
