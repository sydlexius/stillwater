package wikidata

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// FuzzWikidataResponses feeds arbitrary JSON to the two Wikidata
// response-decoder paths that json.Unmarshal bytes from the wire:
//
//   - SPARQLResponse  (SPARQL artist query, wikidata.go:280)
//   - CommonsResponse (Wikimedia Commons image lookup, wikidata.go:497)
//
// The decoders must not panic regardless of input; returning an error is
// acceptable and expected for malformed JSON.
func FuzzWikidataResponses(f *testing.F) {
	// Seed from testdata JSON corpus.
	jsonFiles, err := filepath.Glob("testdata/*.json")
	if err != nil {
		f.Fatalf("globbing testdata: %v", err)
	}
	for _, path := range jsonFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			f.Fatalf("reading seed file %s: %v", path, err)
		}
		f.Add(data)
	}

	// Crafted edge cases from the issue body.
	f.Add([]byte(`{}`))
	// Minimal valid SPARQL response.
	f.Add([]byte(`{"results":{"bindings":[]}}`))
	// Full SPARQL binding row.
	f.Add([]byte(`{"results":{"bindings":[{"item":{"type":"uri","value":"http://www.wikidata.org/entity/Q44190"},"itemLabel":{"type":"literal","value":"Radiohead"},"inception":{"type":"literal","value":"1985-01-01T00:00:00Z"},"dissolved":{"type":"literal","value":""},"countryLabel":{"type":"literal","value":"United Kingdom"},"genreLabel":{"type":"literal","value":"alternative rock"}}]}}`))
	// Deeply nested / repeated bindings.
	f.Add([]byte(`{"results":{"bindings":[{"item":{"type":"uri","value":"http://www.wikidata.org/entity/Q1"}},{"item":{"type":"uri","value":"http://www.wikidata.org/entity/Q2"}}]}}`))
	// Fields with wrong types (string where object expected).
	f.Add([]byte(`{"results":"not-an-object"}`))
	f.Add([]byte(`{"results":{"bindings":"not-an-array"}}`))
	f.Add([]byte(`{"results":{"bindings":[{"item":"should-be-object"}]}}`))
	// CommonsResponse shapes.
	f.Add([]byte(`{"query":{"pages":{}}}`))
	f.Add([]byte(`{"query":{"pages":{"-1":{"pageid":-1,"title":"File:Missing.jpg","imageinfo":[]}}}}`))
	f.Add([]byte(`{"query":{"pages":{"12345":{"pageid":12345,"title":"File:Radiohead.jpg","imageinfo":[{"url":"https://commons.example/Radiohead.jpg","width":1200,"height":800}]}}}}`))
	// Responses with circular-style repeated ids.
	f.Add([]byte(`{"query":{"pages":{"1":{"pageid":1},"2":{"pageid":1}}}}`))
	// Empty and null.
	f.Add([]byte(``))
	f.Add([]byte(`null`))
	f.Add([]byte(`[]`))

	f.Fuzz(func(t *testing.T, data []byte) {
		// SPARQLResponse -- covers wikidata.go:280.
		var sparql SPARQLResponse
		_ = json.Unmarshal(data, &sparql)

		// CommonsResponse -- covers wikidata.go:497.
		var commons CommonsResponse
		_ = json.Unmarshal(data, &commons)
	})
}
