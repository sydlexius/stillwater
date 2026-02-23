package duckduckgo

// imageSearchResponse is the JSON response from the DuckDuckGo image search endpoint.
type imageSearchResponse struct {
	Results []imageHit `json:"results"`
	Next    string     `json:"next,omitempty"`
}

// imageHit is a single image result from DuckDuckGo.
type imageHit struct {
	Image     string `json:"image"`     // Full-size image URL
	Thumbnail string `json:"thumbnail"` // Thumbnail URL
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Title     string `json:"title"`
	Source    string `json:"source"` // Source website URL
}
