package wikipedia

// summaryResponse is the response from the MediaWiki REST API page summary endpoint.
// https://en.wikipedia.org/api/rest_v1/#/Page%20content/get_page_summary__title_
type summaryResponse struct {
	Title       string `json:"title"`
	DisplayName string `json:"displaytitle"`
	Extract     string `json:"extract"`
}

// sparqlResponse is the SPARQL query result for resolving MBID to Wikipedia title.
type sparqlResponse struct {
	Results struct {
		Bindings []struct {
			Article struct {
				Value string `json:"value"`
			} `json:"article"`
		} `json:"bindings"`
	} `json:"results"`
}
