package provider

import "testing"

// TestBestNameSimilarity covers the selection rule: an artist has more than one
// true name, and matching ANY of them is positive evidence.
func TestBestNameSimilarity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		query      string
		candidates []string
		want       int
	}{
		{
			// The reported shape: the operator's folder uses the spaced form,
			// MusicBrainz's primary name is unspaced, and the spaced form is
			// carried as an alias. Real data from the MusicBrainz entity.
			name:       "alias matches exactly where the primary name does not",
			query:      "Barlow Girl",
			candidates: []string{"BarlowGirl", "BarlowGirl", "Barlow Girl"},
			want:       100,
		},
		{
			// Sort-name form. Scoring the primary name alone would penalize a
			// library organized the way MusicBrainz itself sorts.
			name:       "sort-name matches where the primary name does not",
			query:      "Beatles, The",
			candidates: []string{"The Beatles", "Beatles, The"},
			want:       100,
		},
		{
			// The #2285 case: a Latin-script alias for an artist whose primary
			// name is in another script. Without alias scoring this is 0.
			name:       "latin-script alias for a non-latin primary name",
			query:      "Taizo Takemoto",
			candidates: []string{"竹本泰蔵", "Takemoto, Taizo", "Taizo Takemoto"},
			want:       100,
		},
		{
			name:       "primary name still wins when it is the match",
			query:      "Avalon",
			candidates: []string{"Avalon", "Avalon Rising", "Avalon Quartet"},
			want:       100,
		},
		{
			// An artist with many aliases must not be penalized for the ones
			// that do not match: this is a MAX, not an average.
			name:       "many non-matching aliases do not dilute one good match",
			query:      "Beatles",
			candidates: []string{"The Beatles", "披头士乐队", "披頭四樂團", "Ｔｈｅ Ｂｅａｔｌｅｓ", "Beatles"},
			want:       100,
		},
		{
			name:       "empty candidates are skipped, not scored as matches",
			query:      "Avalon",
			candidates: []string{"", "", ""},
			want:       0,
		},
		{
			name:       "no candidates at all",
			query:      "Avalon",
			candidates: nil,
			want:       0,
		},
		{
			name:       "an empty query does not match a real name",
			query:      "",
			candidates: []string{"Avalon"},
			want:       0,
		},
		{
			// Unrelated names still score above zero -- Levenshtein over short
			// strings shares a few characters -- so the value is pinned rather
			// than asserted to be 0. What matters is that it stays far below
			// the DefaultNameSimilarityThreshold of 60.
			name:       "genuinely unrelated names score far below the threshold",
			query:      "Avalon",
			candidates: []string{"Metallica", "Slayer"},
			want:       23,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := BestNameSimilarity(tc.query, tc.candidates...); got != tc.want {
				t.Errorf("BestNameSimilarity(%q, %q) = %d, want %d", tc.query, tc.candidates, got, tc.want)
			}
		})
	}
}

// TestBestNameSimilarity_MatchesNameSimilarityForOneCandidate pins the helper
// against NameSimilarity for a single NON-EMPTY candidate, so the callers
// migrated onto it in this change produce identical scores.
//
// The empty-candidate case is deliberately excluded and is NOT a generalization
// gap to be "fixed": NameSimilarity("", "") is 100 because empty equals empty,
// but an empty candidate here means "this artist has no sort-name" or "this
// alias carries no name", and scoring that as a perfect match would rank a
// candidate with missing data above one that genuinely matches. Skipping is
// the correct reading, and it is why this helper is not a drop-in for every
// NameSimilarity call site.
func TestBestNameSimilarity_MatchesNameSimilarityForOneCandidate(t *testing.T) {
	t.Parallel()

	pairs := [][2]string{
		{"Avalon", "Avalon"},
		{"Avalon", "Avalon Rising"},
		{"The Beatles", "Beatles"},
		{"!!!", "!!!"},
		{"Avalon", "Metallica"},
	}
	for _, p := range pairs {
		want := NameSimilarity(p[0], p[1])
		if got := BestNameSimilarity(p[0], p[1]); got != want {
			t.Errorf("BestNameSimilarity(%q, %q) = %d, but NameSimilarity = %d", p[0], p[1], got, want)
		}
	}
}
