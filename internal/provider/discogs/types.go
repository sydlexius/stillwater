package discogs

// Discogs API response types.

// SearchResponse is the top-level response from the search endpoint.
type SearchResponse struct {
	Results    []SearchResult `json:"results"`
	Pagination Pagination     `json:"pagination"`
}

// SearchResult represents a single search hit.
type SearchResult struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	Type        string `json:"type"`
	Thumb       string `json:"thumb"`
	CoverImage  string `json:"cover_image"`
	ResourceURL string `json:"resource_url"`
}

// Pagination holds pagination info.
type Pagination struct {
	Page    int `json:"page"`
	Pages   int `json:"pages"`
	PerPage int `json:"per_page"`
	Items   int `json:"items"`
}

// ArtistDetail is the full artist response from Discogs.
type ArtistDetail struct {
	ID          int         `json:"id"`
	Name        string      `json:"name"`
	Realname    string      `json:"realname"`
	Profile     string      `json:"profile"`
	URLs        []string    `json:"urls"`
	Aliases     []ArtistRef `json:"aliases"`
	Members     []ArtistRef `json:"members"`
	Images      []Image     `json:"images"`
	DataQuality string      `json:"data_quality"`
}

// ArtistRef is a reference to another artist.
type ArtistRef struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Active      bool   `json:"active"`
	ResourceURL string `json:"resource_url"`
}

// Image represents a Discogs image.
type Image struct {
	Type   string `json:"type"` // "primary" or "secondary"
	URI    string `json:"uri"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}
