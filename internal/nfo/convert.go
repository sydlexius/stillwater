package nfo

import "github.com/sydlexius/stillwater/internal/artist"

// ToArtist converts an ArtistNFO to a domain Artist model.
// The caller is responsible for setting ID, Path, LibraryID, and other
// non-NFO fields (image flags, health score, timestamps, etc.).
func ToArtist(n *ArtistNFO) *artist.Artist {
	return &artist.Artist{
		Name:           n.Name,
		SortName:       n.SortName,
		Type:           n.Type,
		Gender:         n.Gender,
		Disambiguation: n.Disambiguation,
		MusicBrainzID:  n.MusicBrainzArtistID,
		AudioDBID:      n.AudioDBArtistID,
		DiscogsID:      n.DiscogsArtistID,
		WikidataID:     n.WikidataID,
		DeezerID:       n.DeezerArtistID,
		SpotifyID:      n.SpotifyArtistID,
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

// ApplyNFOToArtist maps ArtistNFO fields onto an existing Artist,
// preserving non-NFO fields (ID, Path, LibraryID, image flags, timestamps, etc.).
func ApplyNFOToArtist(n *ArtistNFO, a *artist.Artist) {
	a.Name = n.Name
	a.SortName = n.SortName
	a.Type = n.Type
	a.Gender = n.Gender
	a.Disambiguation = n.Disambiguation
	a.MusicBrainzID = n.MusicBrainzArtistID
	a.AudioDBID = n.AudioDBArtistID
	a.DiscogsID = n.DiscogsArtistID
	a.WikidataID = n.WikidataID
	a.DeezerID = n.DeezerArtistID
	a.SpotifyID = n.SpotifyArtistID
	a.Genres = n.Genres
	a.Styles = n.Styles
	a.Moods = n.Moods
	a.YearsActive = n.YearsActive
	a.Born = n.Born
	a.Formed = n.Formed
	a.Died = n.Died
	a.Disbanded = n.Disbanded
	a.Biography = n.Biography
}

// FromArtist converts a domain Artist model to an ArtistNFO.
// Maps all provider IDs (MusicBrainz, AudioDB, Discogs, Wikidata, Deezer, Spotify)
// into their NFO XML element equivalents. Unknown ID elements are safely ignored
// by media platforms.
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
		DiscogsArtistID:     a.DiscogsID,
		WikidataID:          a.WikidataID,
		DeezerArtistID:      a.DeezerID,
		SpotifyArtistID:     a.SpotifyID,
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
