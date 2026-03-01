package musicbrainz

// MusicBrainz API response types.

// SearchResponse is the top-level response from the artist search endpoint.
type SearchResponse struct {
	Created string     `json:"created"`
	Count   int        `json:"count"`
	Offset  int        `json:"offset"`
	Artists []MBArtist `json:"artists"`
}

// MBArtist represents a MusicBrainz artist entity.
type MBArtist struct {
	ID             string       `json:"id"`
	Name           string       `json:"name"`
	SortName       string       `json:"sort-name"`
	Type           string       `json:"type"`
	Gender         string       `json:"gender"`
	Disambiguation string       `json:"disambiguation"`
	Country        string       `json:"country"`
	Score          int          `json:"score"`
	LifeSpan       MBLifeSpan   `json:"life-span"`
	Aliases        []MBAlias    `json:"aliases"`
	Tags           []MBTag      `json:"tags"`
	Genres         []MBGenre    `json:"genres"`
	Relations      []MBRelation `json:"relations"`
	URLs           []MBRelation `json:"url-rels"`
}

// MBLifeSpan represents the begin/end dates of an artist.
type MBLifeSpan struct {
	Begin string `json:"begin"`
	End   string `json:"end"`
	Ended bool   `json:"ended"`
}

// MBAlias represents an alternative name for an artist.
type MBAlias struct {
	Name     string `json:"name"`
	SortName string `json:"sort-name"`
	Type     string `json:"type"`
	Locale   string `json:"locale"`
	Primary  bool   `json:"primary"`
}

// MBTag represents a user-submitted tag.
type MBTag struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// MBGenre represents a genre classification.
type MBGenre struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// MBRelation represents a relationship between entities.
type MBRelation struct {
	Type       string         `json:"type"`
	TargetType string         `json:"target-type"`
	Direction  string         `json:"direction"`
	Attributes []string       `json:"attributes"`
	Begin      string         `json:"begin"`
	End        string         `json:"end"`
	Ended      bool           `json:"ended"`
	Artist     *MBArtist      `json:"artist,omitempty"`
	URL        *MBRelationURL `json:"url,omitempty"`
}

// MBRelationURL holds URL data within a relation.
type MBRelationURL struct {
	ID       string `json:"id"`
	Resource string `json:"resource"`
}

// MBReleaseGroupSearchResponse is the top-level response from the release-group browse endpoint.
type MBReleaseGroupSearchResponse struct {
	ReleaseGroupCount  int              `json:"release-group-count"`
	ReleaseGroupOffset int              `json:"release-group-offset"`
	ReleaseGroups      []MBReleaseGroup `json:"release-groups"`
}

// MBReleaseGroup represents a MusicBrainz release group entity.
type MBReleaseGroup struct {
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	PrimaryType      string   `json:"primary-type"`
	SecondaryTypes   []string `json:"secondary-types"`
	FirstReleaseDate string   `json:"first-release-date"`
}
