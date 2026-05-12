package musicbrainz

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// FuzzMusicBrainzResponses feeds arbitrary JSON to the four MusicBrainz
// response-decoder paths that json.Unmarshal bytes from the wire:
//
//   - SearchResponse      (artist search, musicbrainz.go:74)
//   - MBArtist            (artist detail, musicbrainz.go:130)
//   - MBArtist            (member alias lookup, musicbrainz.go:373)
//   - MBReleaseGroupSearchResponse (release-group browse, musicbrainz.go:446)
//
// The decoders must not panic regardless of input; returning an error is
// acceptable and expected for malformed JSON.
func FuzzMusicBrainzResponses(f *testing.F) {
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
	f.Add([]byte(`{"artists":[]}`))
	f.Add([]byte(`{"artists":[{"id":"00000000-0000-0000-0000-000000000000","name":"Test","score":100}]}`))
	// Deeply nested aliases array.
	f.Add([]byte(`{"aliases":[{"name":"A","sort-name":"A","type":"Artist name","locale":"en","primary":true},{"name":"B","sort-name":"B","type":"Search hint","locale":"ja","primary":false}]}`))
	// Deeply nested relations array.
	f.Add([]byte(`{"relations":[{"type":"member of band","target-type":"artist","direction":"backward","attributes":[],"artist":{"id":"00000000-0000-0000-0000-000000000001","name":"Member"}}]}`))
	// Fields with wrong types (string where object expected).
	f.Add([]byte(`{"life-span":"not-an-object"}`))
	f.Add([]byte(`{"tags":"not-an-array"}`))
	f.Add([]byte(`{"genres":{"id":"x"}}`))
	f.Add([]byte(`{"relations":[{"artist":"should-be-object"}]}`))
	// Circular-style id references.
	f.Add([]byte(`{"id":"aa","relations":[{"artist":{"id":"aa","relations":[]}}]}`))
	// Release-group response shapes.
	f.Add([]byte(`{"release-group-count":0,"release-group-offset":0,"release-groups":[]}`))
	f.Add([]byte(`{"release-groups":[{"id":"rg1","title":"Album","primary-type":"Album","first-release-date":"2001-01-01"}]}`))
	// Empty and whitespace.
	f.Add([]byte(``))
	f.Add([]byte(`null`))
	f.Add([]byte(`[]`))

	f.Fuzz(func(t *testing.T, data []byte) {
		// SearchResponse -- covers musicbrainz.go:74.
		var sr SearchResponse
		_ = json.Unmarshal(data, &sr)

		// MBArtist (artist detail and member alias lookup) -- covers
		// musicbrainz.go:130 and musicbrainz.go:373.
		var artist MBArtist
		_ = json.Unmarshal(data, &artist)

		// MBReleaseGroupSearchResponse -- covers musicbrainz.go:446.
		var rgr MBReleaseGroupSearchResponse
		_ = json.Unmarshal(data, &rgr)
	})
}

// FuzzMusicBrainzDiffSnapshot feeds arbitrary strings to normalizeSliceJSON,
// the JSON-array decoder inside diff.go:175. The function must never panic;
// it falls back to direct string comparison for non-JSON inputs.
func FuzzMusicBrainzDiffSnapshot(f *testing.F) {
	// Valid snapshot JSON arrays.
	f.Add(`["rock","alternative","indie"]`)
	f.Add(`["Pop","Rock","Electronic"]`)
	f.Add(`[]`)

	// Mixed / tricky element types expressed as strings.
	f.Add(`["a","b","a"]`)
	f.Add(`["","  ","null"]`)

	// Snapshots with NaN / +Inf (not valid JSON, exercises the fallback path).
	f.Add(`[NaN, Infinity, -Infinity]`)

	// Snapshots with extremely large element counts (10,000 items).
	largeArray := make([]byte, 0, 10000*4+2)
	largeArray = append(largeArray, '[')
	for i := 0; i < 10000; i++ {
		if i > 0 {
			largeArray = append(largeArray, ',')
		}
		largeArray = append(largeArray, '"', 'a', '"')
	}
	largeArray = append(largeArray, ']')
	f.Add(string(largeArray))

	// Non-JSON inputs that trigger the fallback.
	f.Add(``)
	f.Add(`not json`)
	f.Add(`{}`)

	// Wrong-type inputs.
	f.Add(`"scalar"`)
	f.Add(`42`)

	f.Fuzz(func(t *testing.T, s string) {
		// normalizeSliceJSON must never panic. It returns a string in all cases.
		_ = normalizeSliceJSON(s)
	})
}
