package nfo

import "github.com/sydlexius/stillwater/internal/artist"

// ToArtist converts an ArtistNFO to a domain Artist model.
// The caller is responsible for setting ID, Path, and file existence flags.
func ToArtist(n *ArtistNFO) *artist.Artist {
	return &artist.Artist{
		Name:           n.Name,
		SortName:       n.SortName,
		Type:           n.Type,
		Gender:         n.Gender,
		Disambiguation: n.Disambiguation,
		MusicBrainzID:  n.MusicBrainzArtistID,
		AudioDBID:      n.AudioDBArtistID,
		Genres:         n.Genres,
		Styles:         n.Styles,
		Moods:          n.Moods,
		YearsActive:    n.YearsActive,
		Born:           n.Born,
		Formed:         n.Formed,
		Died:           n.Died,
		Disbanded:      n.Disbanded,
		Biography:      n.Biography,
	}
}

// FromArtist converts a domain Artist model to an ArtistNFO.
// Only writes musicbrainzartistid and audiodbartistid to the NFO
// (DiscogsID and WikidataID are not read by any media platform from NFO files).
func FromArtist(a *artist.Artist) *ArtistNFO {
	var fanart *Fanart
	// Fanart is not stored on the Artist model; callers can set it directly.

	return &ArtistNFO{
		Name:                a.Name,
		SortName:            a.SortName,
		Type:                a.Type,
		Gender:              a.Gender,
		Disambiguation:      a.Disambiguation,
		MusicBrainzArtistID: a.MusicBrainzID,
		AudioDBArtistID:     a.AudioDBID,
		Genres:              a.Genres,
		Styles:              a.Styles,
		Moods:               a.Moods,
		YearsActive:         a.YearsActive,
		Born:                a.Born,
		Formed:              a.Formed,
		Died:                a.Died,
		Disbanded:           a.Disbanded,
		Biography:           a.Biography,
		Fanart:              fanart,
	}
}
