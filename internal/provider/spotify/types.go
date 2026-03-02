package spotify

// spotifyCredentials holds the OAuth client credentials parsed from the
// stored API key JSON.
type spotifyCredentials struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// tokenResponse is the JSON response from the Spotify token endpoint.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

// searchResponse is the top-level JSON response from the Spotify search endpoint.
type searchResponse struct {
	Artists searchArtists `json:"artists"`
}

// searchArtists wraps the paginated artist results inside a search response.
type searchArtists struct {
	Items []spotifyArtist `json:"items"`
	Total int             `json:"total"`
}

// spotifyArtist represents an artist object from the Spotify API.
type spotifyArtist struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Genres       []string          `json:"genres"`
	Images       []spotifyImage    `json:"images"`
	ExternalURLs map[string]string `json:"external_urls"`
	URI          string            `json:"uri"`
	Type         string            `json:"type"`
}

// spotifyImage represents an image object returned by the Spotify API.
type spotifyImage struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}
