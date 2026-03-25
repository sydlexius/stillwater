package wikidata

// Wikidata SPARQL response types.

// SPARQLResponse is the top-level response from the SPARQL endpoint.
type SPARQLResponse struct {
	Results SPARQLResults `json:"results"`
}

// SPARQLResults wraps the bindings array.
type SPARQLResults struct {
	Bindings []SPARQLBinding `json:"bindings"`
}

// SPARQLBinding represents a single result row.
type SPARQLBinding struct {
	Item      SPARQLValue `json:"item"`
	ItemLabel SPARQLValue `json:"itemLabel"`
	Inception SPARQLValue `json:"inception"`
	Dissolved SPARQLValue `json:"dissolved"`
	Country   SPARQLValue `json:"countryLabel"`
	Genre     SPARQLValue `json:"genreLabel"`
	Image     SPARQLValue `json:"image"`
	Logo      SPARQLValue `json:"logo"`
}

// SPARQLValue represents a single SPARQL value.
type SPARQLValue struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// Wikimedia Commons API response types.

// CommonsResponse is the top-level response from the Wikimedia Commons API.
type CommonsResponse struct {
	Query CommonsQuery `json:"query"`
}

// CommonsQuery wraps the pages map.
type CommonsQuery struct {
	Pages map[string]CommonsPage `json:"pages"`
}

// CommonsPage represents a single page in the Commons API response.
type CommonsPage struct {
	PageID    int                `json:"pageid"`
	Title     string             `json:"title"`
	ImageInfo []CommonsImageInfo `json:"imageinfo"`
}

// CommonsImageInfo holds the resolved URL and dimensions for a Commons file.
type CommonsImageInfo struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}
