package nfo

import "testing"

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
