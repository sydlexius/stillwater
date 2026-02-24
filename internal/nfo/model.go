package nfo

import "encoding/xml"

// ArtistNFO represents the XML structure of a Kodi-compatible artist.nfo file.
type ArtistNFO struct {
	XMLName             xml.Name     `xml:"artist"`
	Name                string       `xml:"name,omitempty"`
	SortName            string       `xml:"sortname,omitempty"`
	Type                string       `xml:"type,omitempty"`
	Gender              string       `xml:"gender,omitempty"`
	Disambiguation      string       `xml:"disambiguation,omitempty"`
	MusicBrainzArtistID string       `xml:"musicbrainzartistid,omitempty"`
	AudioDBArtistID     string       `xml:"audiodbartistid,omitempty"`
	DiscogsArtistID     string       `xml:"discogsartistid,omitempty"`
	WikidataID          string       `xml:"wikidataid,omitempty"`
	Genres              []string     `xml:"genre,omitempty"`
	Styles              []string     `xml:"style,omitempty"`
	Moods               []string     `xml:"mood,omitempty"`
	YearsActive         string       `xml:"yearsactive,omitempty"`
	Born                string       `xml:"born,omitempty"`
	Formed              string       `xml:"formed,omitempty"`
	Died                string       `xml:"died,omitempty"`
	Disbanded           string       `xml:"disbanded,omitempty"`
	Biography           string       `xml:"biography,omitempty"`
	Thumbs              []Thumb      `xml:"thumb,omitempty"`
	Fanart              *Fanart      `xml:"fanart,omitempty"`
	ExtraElements       []RawElement `xml:"-"`
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

// RawElement stores an unrecognized XML element for round-trip preservation.
type RawElement struct {
	Name string
	Raw  []byte
}
