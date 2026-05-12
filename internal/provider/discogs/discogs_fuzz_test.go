package discogs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// FuzzDiscogsResponses feeds arbitrary JSON to the four Discogs
// response-decoder paths that json.Unmarshal bytes from the wire:
//
//   - SearchResponse        (artist search, discogs.go:78)
//   - ArtistDetail          (artist detail by ID, discogs.go:148)
//   - ArtistDetail          (artist images, discogs.go:211)
//   - ArtistReleasesResponse (release list, discogs.go:309)
//   - MasterRelease          (master release genres/styles, discogs.go:329)
//
// The decoders must not panic regardless of input; returning an error is
// acceptable and expected for malformed JSON.
func FuzzDiscogsResponses(f *testing.F) {
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
	// Search response shapes.
	f.Add([]byte(`{"results":[],"pagination":{"page":1,"pages":0,"per_page":50,"items":0}}`))
	f.Add([]byte(`{"results":[{"id":3840,"title":"Radiohead","type":"artist"}]}`))
	// ArtistDetail with deeply nested aliases.
	f.Add([]byte(`{"id":1,"name":"Test","aliases":[{"id":2,"name":"Alt","active":true},{"id":3,"name":"Old","active":false}]}`))
	// ArtistDetail with members array.
	f.Add([]byte(`{"members":[{"id":10,"name":"Member A","active":true},{"id":11,"name":"Member B","active":false}]}`))
	// Fields with wrong types (string where object/array expected).
	f.Add([]byte(`{"images":"not-an-array"}`))
	f.Add([]byte(`{"aliases":{"id":1}}`))
	f.Add([]byte(`{"pagination":"not-an-object"}`))
	f.Add([]byte(`{"id":"should-be-int"}`))
	// ArtistReleasesResponse.
	f.Add([]byte(`{"pagination":{"page":1,"pages":1},"releases":[{"id":1,"title":"Album","type":"master","role":"Main","year":2001}]}`))
	// MasterRelease.
	f.Add([]byte(`{"id":5001,"title":"OK Computer","genres":["Alternative Rock"],"styles":["Art Rock","Post-Britpop"]}`))
	f.Add([]byte(`{"genres":[],"styles":[]}`))
	// Responses with circular id refs (Discogs doesn't emit them, but fuzz must not panic).
	f.Add([]byte(`{"id":1,"aliases":[{"id":1}]}`))
	// Empty and null.
	f.Add([]byte(``))
	f.Add([]byte(`null`))
	f.Add([]byte(`[]`))

	f.Fuzz(func(t *testing.T, data []byte) {
		// SearchResponse -- covers discogs.go:78.
		var sr SearchResponse
		_ = json.Unmarshal(data, &sr)

		// ArtistDetail (artist detail + images) -- covers discogs.go:148 and discogs.go:211.
		var detail ArtistDetail
		_ = json.Unmarshal(data, &detail)

		// ArtistReleasesResponse -- covers discogs.go:309.
		var releases ArtistReleasesResponse
		_ = json.Unmarshal(data, &releases)

		// MasterRelease -- covers discogs.go:329.
		var master MasterRelease
		_ = json.Unmarshal(data, &master)
	})
}
