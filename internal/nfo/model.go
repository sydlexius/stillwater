package nfo

import "encoding/xml"

// ArtistNFO represents the XML structure of a Kodi-compatible artist.nfo file.
type ArtistNFO struct {
	XMLName             xml.Name        `xml:"artist"`
	Name                string          `xml:"name,omitempty"`
	SortName            string          `xml:"sortname,omitempty"`
	Type                string          `xml:"type,omitempty"`
	Gender              string          `xml:"gender,omitempty"`
	Disambiguation      string          `xml:"disambiguation,omitempty"`
	MusicBrainzArtistID string          `xml:"musicbrainzartistid,omitempty"`
	AudioDBArtistID     string          `xml:"audiodbartistid,omitempty"`
	DiscogsArtistID     string          `xml:"discogsartistid,omitempty"`
	WikidataID          string          `xml:"wikidataid,omitempty"`
	DeezerArtistID      string          `xml:"deezerartistid,omitempty"`
	SpotifyArtistID     string          `xml:"spotifyartistid,omitempty"`
	Genres              []string        `xml:"genre,omitempty"`
	Styles              []string        `xml:"style,omitempty"`
	Moods               []string        `xml:"mood,omitempty"`
	YearsActive         string          `xml:"yearsactive,omitempty"`
	Born                string          `xml:"born,omitempty"`
	Formed              string          `xml:"formed,omitempty"`
	Died                string          `xml:"died,omitempty"`
	Disbanded           string          `xml:"disbanded,omitempty"`
	Biography           string          `xml:"biography,omitempty"`
	Thumbs              []Thumb         `xml:"thumb,omitempty"`
	Fanart              *Fanart         `xml:"fanart,omitempty"`
	LockData            bool            `xml:"lockdata,omitempty"`
	Stillwater          *StillwaterMeta `xml:"stillwater,omitempty"`
	ExtraElements       []RawElement    `xml:"-"`
}

// Thumb represents a thumbnail image reference.
type Thumb struct {
	Aspect  string `xml:"aspect,attr,omitempty"`
	Preview string `xml:"preview,attr,omitempty"`
	Value   string `xml:",chardata"`
}

// Fanart contains fanart image references.
type Fanart struct {
	Thumbs []Thumb `xml:"thumb,omitempty"`
}

// StillwaterVersion is the current schema version for the NFO provenance element.
const StillwaterVersion = "1"

// StillwaterMeta holds provenance metadata embedded in NFO files.
// The element records which tool wrote the NFO and when, enabling
// detection of external overwrites by platforms like Emby or Jellyfin.
type StillwaterMeta struct {
	Version string `xml:"version,attr"`
	Written string `xml:"written,attr"` // RFC 3339 timestamp
}

// RawElement stores an unrecognized XML element for round-trip preservation.
type RawElement struct {
	Name string
	Raw  []byte
}
