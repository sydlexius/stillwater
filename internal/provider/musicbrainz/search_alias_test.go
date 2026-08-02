package musicbrainz

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// aliasSearchServer serves one canned /artist?query= response so the test
// controls the alias payload exactly. The shape mirrors what a live
// MusicBrainz search returns: aliases arrive INLINE on the search response,
// which is why alias scoring costs no extra request.
func aliasSearchServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/artist" || r.URL.Query().Get("query") == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("writing response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSearchArtist_ScoresAliases covers #2820 end to end at the adapter: a
// query that matches only an ALIAS must produce a high score, where before it
// was measured against the primary name alone and looked unrelated.
//
// The payloads are the real shapes observed from a live MusicBrainz search,
// including the deliberately LOW native "score" that the API returns for these
// entries -- the whole point is that our own name similarity has to lift it.
func TestSearchArtist_ScoresAliases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		query    string
		body     string
		wantMin  int
		wantName string
	}{
		{
			// The reported case: the operator's folder is "Barlow Girl", the
			// primary name is unspaced, and the spaced form is an alias.
			name:  "alias match lifts a low provider score",
			query: "Barlow Girl",
			body: `{"count":1,"offset":0,"artists":[{
				"id":"mbid-barlowgirl","name":"BarlowGirl","sort-name":"BarlowGirl","score":40,
				"aliases":[{"name":"Barlow Girl","sort-name":"Barlow Girl"}]}]}`,
			wantMin:  100,
			wantName: "BarlowGirl",
		},
		{
			// The #2285 shape: a Latin-script alias for an artist whose
			// primary name is in another script. Scoring the primary name
			// alone gives 0 here, so this is the case that most needs aliases.
			name:  "latin-script alias for a non-latin primary name",
			query: "Taizo Takemoto",
			body: `{"count":1,"offset":0,"artists":[{
				"id":"mbid-takemoto","name":"竹本泰蔵","sort-name":"Takemoto, Taizo","score":30,
				"aliases":[{"name":"Taizo Takemoto","sort-name":"Takemoto, Taizo"}]}]}`,
			wantMin:  100,
			wantName: "竹本泰蔵",
		},
		{
			// Sort-name only, no aliases at all.
			name:  "sort-name match with no aliases present",
			query: "Beatles, The",
			body: `{"count":1,"offset":0,"artists":[{
				"id":"mbid-beatles","name":"The Beatles","sort-name":"Beatles, The","score":50}]}`,
			wantMin:  100,
			wantName: "The Beatles",
		},
		{
			// An artist carrying many non-matching aliases must not be
			// dragged down by them.
			name:  "many unrelated aliases do not dilute the match",
			query: "Beatles",
			body: `{"count":1,"offset":0,"artists":[{
				"id":"mbid-beatles","name":"The Beatles","sort-name":"Beatles, The","score":20,
				"aliases":[{"name":"披头士乐队"},{"name":"披頭四樂團"},{"name":"Beatles"},{"name":"Los Beatles"}]}]}`,
			wantMin:  100,
			wantName: "The Beatles",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := aliasSearchServer(t, tc.body)
			a := newTestAdapter(t, srv.URL)

			results, err := a.SearchArtist(context.Background(), tc.query)
			if err != nil {
				t.Fatalf("SearchArtist: %v", err)
			}
			if len(results) != 1 {
				t.Fatalf("results = %d, want 1", len(results))
			}
			if results[0].Name != tc.wantName {
				t.Errorf("Name = %q, want %q (the PRIMARY name is still what is displayed)",
					results[0].Name, tc.wantName)
			}
			if results[0].Score < tc.wantMin {
				t.Errorf("Score = %d, want >= %d: the query matches a name this artist is known by, so it must not read as unrelated",
					results[0].Score, tc.wantMin)
			}
		})
	}
}

// TestSearchArtist_AliasesNeverLowerTheScore pins that alias scoring is
// strictly additive. The adapter keeps the HIGHER of the provider's native
// score and our similarity, so an artist whose aliases are all irrelevant must
// still retain whatever the API gave it.
func TestSearchArtist_AliasesNeverLowerTheScore(t *testing.T) {
	t.Parallel()

	const nativeScore = 88
	body := `{"count":1,"offset":0,"artists":[{
		"id":"mbid-x","name":"Some Artist","sort-name":"Artist, Some","score":88,
		"aliases":[{"name":"Totally Unrelated"},{"name":"Nothing Like It"}]}]}`

	srv := aliasSearchServer(t, body)
	a := newTestAdapter(t, srv.URL)

	results, err := a.SearchArtist(context.Background(), "Some Artist")
	if err != nil {
		t.Fatalf("SearchArtist: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].Score < nativeScore {
		t.Errorf("Score = %d, want >= %d: irrelevant aliases must never drag a score below the provider's own",
			results[0].Score, nativeScore)
	}
}
