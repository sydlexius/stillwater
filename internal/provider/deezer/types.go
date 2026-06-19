package deezer

// searchResponse is the JSON response from the Deezer artist search endpoint.
type searchResponse struct {
	Data  []artistResult `json:"data"`
	Total int            `json:"total"`
	Next  string         `json:"next,omitempty"`
}

// albumsResponse is the JSON response from Deezer's /artist/{id}/albums endpoint.
// Deezer paginates with index/limit query params and returns a "next" URL plus a
// "total" count, mirroring the search wrapper.
type albumsResponse struct {
	Data  []albumResult `json:"data"`
	Total int           `json:"total"`
	Next  string        `json:"next,omitempty"`
}

// albumResult is a single album entry from Deezer's /artist/{id}/albums endpoint.
// Deezer names the album category field "record_type" (values: album, single, ep,
// compilation) and the release date "release_date".
type albumResult struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	ReleaseDate string `json:"release_date"`
	RecordType  string `json:"record_type"`
}

// artistResult is a single artist entry from a Deezer search or artist endpoint.
type artistResult struct {
	ID            int    `json:"id"`
	Name          string `json:"name"`
	Link          string `json:"link"`
	Picture       string `json:"picture"`
	PictureSmall  string `json:"picture_small"`
	PictureMedium string `json:"picture_medium"`
	PictureBig    string `json:"picture_big"`
	PictureXL     string `json:"picture_xl"`
	NbAlbum       int    `json:"nb_album"`
	NbFan         int    `json:"nb_fan"`
	Radio         bool   `json:"radio"`
	Tracklist     string `json:"tracklist"`
	Type          string `json:"type"`
}
