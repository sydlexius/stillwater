package nfo

import "github.com/sydlexius/stillwater/internal/artist"

// fromArtistDiscography converts artist-domain discography entries back to
// the NFO album slice for serialization.
func fromArtistDiscography(albums []artist.DiscographyAlbum) []DiscographyAlbum {
	if len(albums) == 0 {
		return nil
	}
	out := make([]DiscographyAlbum, 0, len(albums))
	for _, a := range albums {
		out = append(out, DiscographyAlbum{
			Title:                     a.Title,
			Year:                      a.Year,
			MusicBrainzReleaseGroupID: a.MusicBrainzReleaseGroupID,
		})
	}
	return out
}

// ToMetadataUpdate converts an ArtistNFO into a MetadataUpdate suitable for
// passing to artist.ApplyMetadata. All NFO metadata fields are mapped; the
// caller chooses the MergeStrategy.
// Gender is cleared for non-individual types (group, orchestra, choir).
func ToMetadataUpdate(n *ArtistNFO) *artist.MetadataUpdate {
	gender := n.Gender
	if n.Type != "" && !artist.IsIndividualType(n.Type) {
		gender = ""
	}
	return &artist.MetadataUpdate{
		Name:           n.Name,
		SortName:       n.SortName,
		Type:           n.Type,
		Gender:         gender,
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

// isIndividualType delegates to artist.IsIndividualType. Kept as a local
// shorthand used by FromArtistWithFieldMap.
func isIndividualType(t string) bool {
	return artist.IsIndividualType(t)
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

	gender := a.Gender
	if a.Type != "" && !isIndividualType(a.Type) {
		gender = ""
	}

	return &ArtistNFO{
		Name:                a.Name,
		SortName:            a.SortName,
		Type:                a.Type,
		Gender:              gender,
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
		Albums:              fromArtistDiscography(a.Discography),
	}
}
