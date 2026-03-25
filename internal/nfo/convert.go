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

// ToMetadataUpdate converts an ArtistNFO into a MetadataUpdate suitable for
// passing to artist.ApplyMetadata. All NFO metadata fields are mapped; the
// caller chooses the MergeStrategy.
func ToMetadataUpdate(n *ArtistNFO) *artist.MetadataUpdate {
	return &artist.MetadataUpdate{
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
		Biography:      n.Biography,
		Genres:         n.Genres,
		Styles:         n.Styles,
		Moods:          n.Moods,
		YearsActive:    n.YearsActive,
		Born:           n.Born,
		Formed:         n.Formed,
		Died:           n.Died,
		Disbanded:      n.Disbanded,
	}
}

// ApplyNFOToArtist maps ArtistNFO fields onto an existing Artist,
// preserving non-NFO fields (ID, Path, LibraryID, image flags, timestamps, etc.).
// Uses SnapshotRestore strategy (unconditional overwrite of all metadata fields).
func ApplyNFOToArtist(n *ArtistNFO, a *artist.Artist) {
	u := ToMetadataUpdate(n)
	artist.ApplyMetadata(a, u, artist.SnapshotRestore, artist.MergeOptions{})
}

// FromArtist converts a domain Artist model to an ArtistNFO using the default
// (Kodi-compatible) field mapping. Maps all provider IDs (MusicBrainz, AudioDB,
// Discogs, Wikidata, Deezer, Spotify) into their NFO XML element equivalents.
// Unknown ID elements are safely ignored by media platforms.
func FromArtist(a *artist.Artist) *ArtistNFO {
	return FromArtistWithFieldMap(a, DefaultFieldMap())
}

// FromArtistWithFieldMap converts a domain Artist model to an ArtistNFO,
// applying the given NFOFieldMap to determine how genres, styles, and moods
// are mapped to NFO XML elements. This enables platform-specific output
// (e.g., writing moods as <style> for Emby/Jellyfin visibility).
func FromArtistWithFieldMap(a *artist.Artist, fm NFOFieldMap) *ArtistNFO {
	var fanart *Fanart
	// Fanart is not stored on the Artist model; callers can set it directly.

	nfoGenres, nfoStyles, nfoMoods := ApplyFieldMap(fm, a.Genres, a.Styles, a.Moods)

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
		Genres:              nfoGenres,
		Styles:              nfoStyles,
		Moods:               nfoMoods,
		YearsActive:         a.YearsActive,
		Born:                a.Born,
		Formed:              a.Formed,
		Died:                a.Died,
		Disbanded:           a.Disbanded,
		Biography:           a.Biography,
		Fanart:              fanart,
	}
}
