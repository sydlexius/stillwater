package genius

// Genius API response types.

// SearchResponse is the top-level response from /search.
type SearchResponse struct {
	Response SearchResponseBody `json:"response"`
}

// SearchResponseBody holds the response body.
type SearchResponseBody struct {
	Hits []SearchHit `json:"hits"`
}

// SearchHit is a single search hit.
type SearchHit struct {
	Type   string    `json:"type"`
	Result HitResult `json:"result"`
}

// HitResult is the result object inside a search hit.
type HitResult struct {
	ID            int           `json:"id"`
	Title         string        `json:"title"`
	URL           string        `json:"url"`
	PrimaryArtist PrimaryArtist `json:"primary_artist"`
}

// PrimaryArtist is the artist referenced in a search hit.
type PrimaryArtist struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// ArtistResponse is the top-level response from /artists/{id}.
type ArtistResponse struct {
	Response ArtistResponseBody `json:"response"`
}

// ArtistResponseBody holds the response body.
type ArtistResponseBody struct {
	Artist ArtistDetail `json:"artist"`
}

// ArtistDetail is the full artist detail from /artists/{id}.
type ArtistDetail struct {
	ID             int        `json:"id"`
	Name           string     `json:"name"`
	URL            string     `json:"url"`
	Description    ArtistDesc `json:"description"`
	AlternateNames []string   `json:"alternate_names"`
}

// ArtistDesc holds the description in different formats.
type ArtistDesc struct {
	Plain string `json:"plain"`
}
