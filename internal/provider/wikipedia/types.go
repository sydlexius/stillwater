package wikipedia

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

// extractPage is a single page entry inside an extractResponse.
type extractPage struct {
	PageID  int    `json:"pageid"`
	Title   string `json:"title"`
	Extract string `json:"extract"`
}

// extractResponse is the response from the MediaWiki Action API
// action=query&prop=extracts&explaintext=true endpoint.
type extractResponse struct {
	Query struct {
		Pages map[string]extractPage `json:"pages"`
	} `json:"query"`
}

// parseResponse is the response from the MediaWiki Action API
// action=parse&prop=wikitext&section=0 endpoint.
type parseResponse struct {
	Parse struct {
		Title    string `json:"title"`
		PageID   int    `json:"pageid"`
		Wikitext struct {
			Text string `json:"*"`
		} `json:"wikitext"`
	} `json:"parse"`
}

// wbSitelink is a single sitelink entry in a Wikidata entity.
type wbSitelink struct {
	Title string `json:"title"`
}

// wbEntity is a single entity in a Wikidata wbgetentities response.
type wbEntity struct {
	Sitelinks map[string]wbSitelink `json:"sitelinks"`
}

// wbEntityResponse is the response from the Wikidata wbgetentities API
// used to resolve Q-IDs to Wikipedia article titles via sitelinks.
type wbEntityResponse struct {
	Entities map[string]wbEntity `json:"entities"`
}
