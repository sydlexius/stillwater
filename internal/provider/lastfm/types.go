package lastfm

// Last.fm API response types.

// SearchResponse is the top-level response from artist.search.
type SearchResponse struct {
	Results SearchResults `json:"results"`
}

// SearchResults holds the search result set.
type SearchResults struct {
	ArtistMatches ArtistMatches `json:"artistmatches"`
	TotalResults  string        `json:"opensearch:totalResults"`
}

// ArtistMatches wraps the artist array.
type ArtistMatches struct {
	Artist []SearchArtist `json:"artist"`
}

// SearchArtist is a single search result.
type SearchArtist struct {
	Name      string `json:"name"`
	Listeners string `json:"listeners"`
	MBID      string `json:"mbid"`
	URL       string `json:"url"`
}

// InfoResponse is the top-level response from artist.getinfo.
type InfoResponse struct {
	Artist ArtistInfo `json:"artist"`
}

// ArtistInfo is the full artist info from artist.getinfo.
type ArtistInfo struct {
	Name    string       `json:"name"`
	MBID    string       `json:"mbid"`
	URL     string       `json:"url"`
	Stats   ArtistStats  `json:"stats"`
	Bio     ArtistBio    `json:"bio"`
	Tags    ArtistTags   `json:"tags"`
	Similar SimilarGroup `json:"similar"`
}

// ArtistStats holds listener/playcount.
type ArtistStats struct {
	Listeners string `json:"listeners"`
	Playcount string `json:"playcount"`
}

// ArtistBio holds the biography.
type ArtistBio struct {
	Summary string `json:"summary"`
	Content string `json:"content"`
}

// ArtistTags wraps the tag array.
type ArtistTags struct {
	Tag []Tag `json:"tag"`
}

// Tag is a single tag.
type Tag struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// SimilarGroup wraps the similar artists array.
type SimilarGroup struct {
	Artist []SimilarArtist `json:"artist"`
}

// SimilarArtist is a single similar artist.
type SimilarArtist struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}
