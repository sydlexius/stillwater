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
}

// SPARQLValue represents a single SPARQL value.
type SPARQLValue struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}
