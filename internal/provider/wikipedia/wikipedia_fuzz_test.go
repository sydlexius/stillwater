package wikipedia

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// FuzzWikipediaResponses feeds arbitrary JSON to the five Wikipedia/Wikidata
// response-decoder paths that json.Unmarshal bytes from the wire:
//
//   - sparqlResponse   (MBID-to-title SPARQL, wikipedia.go:539)
//   - wbEntityResponse (localized sitelink lookup, wikipedia.go:630)
//   - wbEntityResponse (Q-ID to title via sitelinks, wikipedia.go:699)
//   - extractResponse  (Wikipedia article extract, wikipedia.go:786)
//   - parseResponse    (MediaWiki wikitext parse, wikipedia.go:854)
//
// The response types are unexported, so this file uses package wikipedia
// (not package wikipedia_test) to access them directly. The decoders must
// not panic regardless of input; returning an error is acceptable and
// expected for malformed JSON.
func FuzzWikipediaResponses(f *testing.F) {
	// Seed from testdata corpus (text files; read as raw bytes for the fuzzer).
	textFiles, err := filepath.Glob("testdata/*.txt")
	if err != nil {
		f.Fatalf("globbing testdata: %v", err)
	}
	for _, path := range textFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			f.Fatalf("reading seed file %s: %v", path, err)
		}
		f.Add(data)
	}

	// Crafted edge cases from the issue body.
	f.Add([]byte(`{}`))
	// Minimal valid SPARQL response (sparqlResponse).
	f.Add([]byte(`{"results":{"bindings":[]}}`))
	// SPARQL binding with item and article.
	f.Add([]byte(`{"results":{"bindings":[{"item":{"value":"http://www.wikidata.org/entity/Q44190"},"article":{"value":"https://en.wikipedia.org/wiki/Radiohead"}}]}}`))
	// Multiple bindings.
	f.Add([]byte(`{"results":{"bindings":[{"item":{"value":"http://www.wikidata.org/entity/Q1"},"article":{"value":"https://en.wikipedia.org/wiki/A"}},{"item":{"value":"http://www.wikidata.org/entity/Q2"},"article":{"value":"https://en.wikipedia.org/wiki/B"}}]}}`))
	// wbEntityResponse (sitelinks).
	f.Add([]byte(`{"entities":{}}`))
	f.Add([]byte(`{"entities":{"Q44190":{"sitelinks":{"enwiki":{"title":"Radiohead"},"jawiki":{"title":"レディオヘッド"}}}}}`))
	f.Add([]byte(`{"entities":{"Q44190":{"sitelinks":{}}}}`))
	// extractResponse.
	f.Add([]byte(`{"query":{"pages":{}}}`))
	f.Add([]byte(`{"query":{"pages":{"-1":{"pageid":-1,"title":"Missing","extract":""}}}}`))
	f.Add([]byte(`{"query":{"pages":{"44190":{"pageid":44190,"title":"Radiohead","extract":"Radiohead are an English rock band..."}}}}`))
	// parseResponse (wikitext).
	f.Add([]byte(`{"parse":{"title":"Radiohead","pageid":44190,"wikitext":{"*":"{{Infobox musical artist|name=Radiohead}}"}}}`))
	f.Add([]byte(`{"parse":{"title":"","pageid":0,"wikitext":{"*":""}}}`))
	// Fields with wrong types.
	f.Add([]byte(`{"results":"not-an-object"}`))
	f.Add([]byte(`{"entities":"not-an-object"}`))
	f.Add([]byte(`{"query":{"pages":"not-an-object"}}`))
	f.Add([]byte(`{"parse":"not-an-object"}`))
	// Responses with deeply nested / alias-style circular id refs.
	f.Add([]byte(`{"entities":{"Q1":{"sitelinks":{"enwiki":{"title":"A"}}},"Q2":{"sitelinks":{"enwiki":{"title":"A"}}}}}`))
	// Empty and null.
	f.Add([]byte(``))
	f.Add([]byte(`null`))
	f.Add([]byte(`[]`))

	f.Fuzz(func(t *testing.T, data []byte) {
		// sparqlResponse -- covers wikipedia.go:539.
		var sparql sparqlResponse
		_ = json.Unmarshal(data, &sparql)

		// wbEntityResponse -- covers wikipedia.go:630 and wikipedia.go:699.
		var wbResp wbEntityResponse
		_ = json.Unmarshal(data, &wbResp)

		// extractResponse -- covers wikipedia.go:786.
		var extResp extractResponse
		_ = json.Unmarshal(data, &extResp)

		// parseResponse -- covers wikipedia.go:854.
		var parseResp parseResponse
		_ = json.Unmarshal(data, &parseResp)
	})
}
