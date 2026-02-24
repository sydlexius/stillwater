package deezer

// searchResponse is the JSON response from the Deezer artist search endpoint.
type searchResponse struct {
	Data  []artistResult `json:"data"`
	Total int            `json:"total"`
	Next  string         `json:"next,omitempty"`
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
