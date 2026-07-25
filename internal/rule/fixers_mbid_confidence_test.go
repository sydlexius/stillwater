package rule

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// Real-shaped MusicBrainz IDs. isValidMBID only accepts a 36-character UUID, so
// the fixture IDs have to be genuine UUIDs or every candidate is filtered out
// before the confidence gates ever run and each test below passes vacuously.
const (
	mbidRadiohead = "a74b1b7f-71a5-4011-9441-d0b5e4122711"
	mbidRival     = "8538e728-ca0b-4321-b7e5-cff6565dd4c0"
	mbidThird     = "b071f9fa-14b0-4217-8e97-eb41da73f598"
)

// stubMBIDSearchOrchestrator is a test-only metadataOrchestrator that returns a
// fixed search result set. Only Search is exercised by the nfo_has_mbid fixer.
type stubMBIDSearchOrchestrator struct {
	results []provider.ArtistSearchResult
	err     error
}

func (s *stubMBIDSearchOrchestrator) Search(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
	return s.results, s.err
}

func (s *stubMBIDSearchOrchestrator) FetchMetadata(_ context.Context, _, _ string, _ map[provider.ProviderName]string) (*provider.FetchResult, error) {
	return nil, nil
}

func (s *stubMBIDSearchOrchestrator) FetchFieldFromProviders(_ context.Context, _, _, _ string, _ map[provider.ProviderName]string) ([]provider.FieldProviderResult, error) {
	return nil, nil
}

func mbidFixer(results ...provider.ArtistSearchResult) *MetadataFixer {
	return &MetadataFixer{orchestrator: &stubMBIDSearchOrchestrator{results: results}, logger: testLogger()}
}

func fixMBIDFor(t *testing.T, f *MetadataFixer, a *artist.Artist) *FixResult {
	t.Helper()
	fr, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleNFOHasMBID})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if fr == nil {
		t.Fatal("Fix returned a nil FixResult")
	}
	return fr
}

// TestFixMBID_AdoptsConfidentUncontestedMatch is the happy path: one clear,
// high-scoring, name-matching hit gets adopted. It also pins the provenance
// content, since the FixResult message is what the pipeline writes into
// metadata_changes as the audit record of how the ID was obtained.
func TestFixMBID_AdoptsConfidentUncontestedMatch(t *testing.T) {
	f := mbidFixer(
		provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: 100, Source: "musicbrainz"},
		provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: 92, Source: "lastfm"},
	)
	a := &artist.Artist{Name: "Radiohead"}

	fr := fixMBIDFor(t, f, a)

	if !fr.Fixed {
		t.Fatalf("expected Fixed=true, got false (message: %q)", fr.Message)
	}
	if a.MusicBrainzID != mbidRadiohead {
		t.Errorf("a.MusicBrainzID = %q, want %q", a.MusicBrainzID, mbidRadiohead)
	}
	// Provenance: the message must name the ID, the matched name, the provider
	// that supplied it, the confidence, and the absence of a rival. A future
	// reader of metadata_changes uses exactly these to judge trustworthiness.
	for _, want := range []string{mbidRadiohead, `"Radiohead"`, "musicbrainz", "confidence 100", "no rival"} {
		if !strings.Contains(fr.Message, want) {
			t.Errorf("provenance message %q is missing %q", fr.Message, want)
		}
	}
}

// TestFixMBID_RejectsBelowScoreFloor covers the core defect: a low-confidence
// top hit used to be adopted verbatim. The fixture's name is an EXACT match, so
// the name-similarity gate cannot fire; only the score floor can reject it, and
// a mutation that removes the score floor makes this test go red.
func TestFixMBID_RejectsBelowScoreFloor(t *testing.T) {
	f := mbidFixer(
		provider.ArtistSearchResult{Name: "Nadja", MusicBrainzID: mbidRadiohead, Score: 55, Source: "discogs"},
	)
	a := &artist.Artist{Name: "Nadja"}

	fr := fixMBIDFor(t, f, a)

	if fr.Fixed {
		t.Errorf("expected Fixed=false for a hit below the score floor, got true (%q)", fr.Message)
	}
	if a.MusicBrainzID != "" {
		t.Errorf("a.MusicBrainzID = %q, want empty: a below-floor hit must not be adopted", a.MusicBrainzID)
	}
	if !strings.Contains(fr.Message, "confidence floor") {
		t.Errorf("message %q should say the score floor was the reason", fr.Message)
	}
}

// TestFixMBID_RejectsNameMismatchDespiteHighProviderScore is the case a score
// floor alone gets wrong. MusicBrainz's score is a relevance rank that folds in
// popularity, so a famous artist can be returned at 100 for a query that barely
// resembles its name. Score is 100 here, well clear of the floor, and there is
// no rival, so the ONLY gate that can reject this is the local name-similarity
// check.
func TestFixMBID_RejectsNameMismatchDespiteHighProviderScore(t *testing.T) {
	f := mbidFixer(
		provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: 100, Source: "musicbrainz"},
	)
	a := &artist.Artist{Name: "Radio Birdman"}

	fr := fixMBIDFor(t, f, a)

	if fr.Fixed {
		t.Errorf("expected Fixed=false when the hit's name does not match the artist, got true (%q)", fr.Message)
	}
	if a.MusicBrainzID != "" {
		t.Errorf("a.MusicBrainzID = %q, want empty", a.MusicBrainzID)
	}
	if !strings.Contains(fr.Message, "matches the artist name only") {
		t.Errorf("message %q should say the name-similarity gate was the reason", fr.Message)
	}
}

// TestFixMBID_RejectsAmbiguousRivalIdentities covers two distinct artists that
// share a name: both come back as exact matches at the same score, so a floor
// plus a name check both pass and only the ambiguity margin can reject. This is
// the case the "just add a floor" over-correction gets wrong.
func TestFixMBID_RejectsAmbiguousRivalIdentities(t *testing.T) {
	f := mbidFixer(
		provider.ArtistSearchResult{Name: "Nirvana", MusicBrainzID: mbidRadiohead, Score: 100, Source: "musicbrainz"},
		provider.ArtistSearchResult{Name: "Nirvana", MusicBrainzID: mbidRival, Score: 98, Source: "musicbrainz"},
	)
	a := &artist.Artist{Name: "Nirvana"}

	fr := fixMBIDFor(t, f, a)

	if fr.Fixed {
		t.Errorf("expected Fixed=false for two same-name identities within the margin, got true (%q)", fr.Message)
	}
	if a.MusicBrainzID != "" {
		t.Errorf("a.MusicBrainzID = %q, want empty: an ambiguous search is not evidence", a.MusicBrainzID)
	}
	if !strings.Contains(fr.Message, "ambiguous") {
		t.Errorf("message %q should say ambiguity was the reason", fr.Message)
	}
}

// TestFixMBID_AdoptsWhenRivalIsClearlyBehind is the counterweight to the
// ambiguity test: a rival exists but trails by more than the margin, so the top
// hit is still adopted. Without this, an over-correction that rejects ANY
// result set containing a second MBID would pass the ambiguity test above and
// silently stop fixing the common case where a search returns near-name noise.
func TestFixMBID_AdoptsWhenRivalIsClearlyBehind(t *testing.T) {
	f := mbidFixer(
		provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: 100, Source: "musicbrainz"},
		provider.ArtistSearchResult{Name: "Radiohead Tribute", MusicBrainzID: mbidRival, Score: 70, Source: "musicbrainz"},
	)
	a := &artist.Artist{Name: "Radiohead"}

	fr := fixMBIDFor(t, f, a)

	if !fr.Fixed {
		t.Fatalf("expected Fixed=true when the rival trails by more than the margin, got false (%q)", fr.Message)
	}
	if a.MusicBrainzID != mbidRadiohead {
		t.Errorf("a.MusicBrainzID = %q, want %q", a.MusicBrainzID, mbidRadiohead)
	}
	// The rival must be named in the provenance: "a rival existed and lost" is
	// materially different evidence from "nothing else was found".
	if !strings.Contains(fr.Message, "next rival "+mbidRival) {
		t.Errorf("provenance message %q should record the losing rival and its score", fr.Message)
	}
}

// TestFixMBID_DuplicateIDAcrossProvidersIsNotARival guards the split-by-ID
// choice in bestMBIDCandidates. Three providers agreeing on ONE id is the
// strongest possible corroboration; if the runner-up were taken by list
// position instead, the second row would be read as a rival at a 2-point gap
// and this well-corroborated match would be rejected.
func TestFixMBID_DuplicateIDAcrossProvidersIsNotARival(t *testing.T) {
	f := mbidFixer(
		provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: 100, Source: "musicbrainz"},
		provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: 98, Source: "lastfm"},
		provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: 97, Source: "audiodb"},
	)
	a := &artist.Artist{Name: "Radiohead"}

	fr := fixMBIDFor(t, f, a)

	if !fr.Fixed {
		t.Fatalf("expected Fixed=true when every provider agrees on one MBID, got false (%q)", fr.Message)
	}
	if a.MusicBrainzID != mbidRadiohead {
		t.Errorf("a.MusicBrainzID = %q, want %q", a.MusicBrainzID, mbidRadiohead)
	}
	if !strings.Contains(fr.Message, "no rival") {
		t.Errorf("message %q: agreeing duplicates must not be reported as a rival", fr.Message)
	}
}

// TestFixMBID_RejectsMalformedMBID guards the shape check. A provider returning
// a non-UUID in the MusicBrainzID field must not have it written through: the
// candidate is skipped entirely, and since it is the only hit the fixer reports
// "no results with a usable MusicBrainz ID".
func TestFixMBID_RejectsMalformedMBID(t *testing.T) {
	f := mbidFixer(
		provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: "not-a-uuid", Score: 100, Source: "lastfm"},
	)
	a := &artist.Artist{Name: "Radiohead"}

	fr := fixMBIDFor(t, f, a)

	if fr.Fixed {
		t.Errorf("expected Fixed=false for a malformed MBID, got true (%q)", fr.Message)
	}
	if a.MusicBrainzID != "" {
		t.Errorf("a.MusicBrainzID = %q, want empty: a non-UUID must never be adopted", a.MusicBrainzID)
	}
}

// TestFixMBID_SkipsMalformedAndAdoptsValidRunner verifies the shape filter
// rejects only the malformed row rather than abandoning the whole search: a
// valid lower-scoring hit behind a malformed top hit is still adopted.
func TestFixMBID_SkipsMalformedAndAdoptsValidRunner(t *testing.T) {
	f := mbidFixer(
		provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: "", Score: 100, Source: "discogs"},
		provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: "12345", Score: 99, Source: "genius"},
		provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: 95, Source: "musicbrainz"},
	)
	a := &artist.Artist{Name: "Radiohead"}

	fr := fixMBIDFor(t, f, a)

	if !fr.Fixed {
		t.Fatalf("expected Fixed=true: a valid hit exists behind the unusable ones (%q)", fr.Message)
	}
	if a.MusicBrainzID != mbidRadiohead {
		t.Errorf("a.MusicBrainzID = %q, want %q", a.MusicBrainzID, mbidRadiohead)
	}
}

// TestFixMBID_NoResults and the error case preserve the pre-existing contract:
// an empty result set is a non-fix, and a provider failure is an error with the
// artist left untouched.
func TestFixMBID_NoResults(t *testing.T) {
	f := mbidFixer()
	a := &artist.Artist{Name: "Radiohead"}

	fr := fixMBIDFor(t, f, a)

	if fr.Fixed {
		t.Errorf("expected Fixed=false with no search results, got true")
	}
	if a.MusicBrainzID != "" {
		t.Errorf("a.MusicBrainzID = %q, want empty", a.MusicBrainzID)
	}
}

func TestFixMBID_SearchErrorPropagates(t *testing.T) {
	wantErr := errors.New("provider failure")
	stub := &stubMBIDSearchOrchestrator{err: wantErr}
	f := &MetadataFixer{orchestrator: stub, logger: testLogger()}

	a := &artist.Artist{Name: "Radiohead"}
	fr, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleNFOHasMBID})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected the search failure to propagate, got %v", err)
	}
	if fr != nil {
		t.Fatalf("expected a nil FixResult on error, got %+v", fr)
	}
	if a.MusicBrainzID != "" {
		t.Errorf("a.MusicBrainzID = %q, want empty", a.MusicBrainzID)
	}
}

// TestEvaluateMBIDCandidate_BoundaryScores pins the floors as inclusive: a hit
// scoring exactly the floor is adopted, one point below is not. A mutation that
// flips < to <= (or moves a constant by one) fails here.
func TestEvaluateMBIDCandidate_BoundaryScores(t *testing.T) {
	tests := []struct {
		name       string
		best       provider.ArtistSearchResult
		runnerUp   *provider.ArtistSearchResult
		wantReject bool
	}{
		{
			name: "score exactly at the floor is accepted",
			best: provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: mbidMinProviderScore},
		},
		{
			name:       "score one below the floor is rejected",
			best:       provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: mbidMinProviderScore - 1},
			wantReject: true,
		},
		{
			name:     "rival exactly at the margin is accepted",
			best:     provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: 100},
			runnerUp: &provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRival, Score: 100 - mbidAmbiguityMargin},
		},
		{
			name:       "rival one inside the margin is rejected",
			best:       provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: 100},
			runnerUp:   &provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRival, Score: 100 - mbidAmbiguityMargin + 1},
			wantReject: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rej := evaluateMBIDCandidate("Radiohead", &tc.best, tc.runnerUp)
			if tc.wantReject && rej == nil {
				t.Error("expected a rejection, got nil")
			}
			if !tc.wantReject && rej != nil {
				t.Errorf("expected acceptance, got rejection: %s", rej.reason)
			}
		})
	}
}

// TestBestMBIDCandidates_PicksHighestPerIdentity verifies the runner-up is the
// best-scoring hit carrying a DIFFERENT id, not merely the next row. The
// fixture deliberately orders the rows so list position and score order
// disagree for the rival: the weaker rival (mbidThird, 60) appears BEFORE the
// stronger one (mbidRival, 80). A "take the next row" implementation reports
// the 60 and passes a laxer check, so the assertion on the score is what gives
// this test teeth.
func TestBestMBIDCandidates_PicksHighestPerIdentity(t *testing.T) {
	results := []provider.ArtistSearchResult{
		{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: 90},
		{Name: "Other", MusicBrainzID: mbidThird, Score: 60},
		{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: 95},
		{Name: "Another", MusicBrainzID: mbidRival, Score: 80},
	}

	best, runnerUp := bestMBIDCandidates(results)

	if best == nil || best.MusicBrainzID != mbidRadiohead || best.Score != 95 {
		t.Fatalf("best = %+v, want the mbidRadiohead row scoring 95", best)
	}
	if runnerUp == nil {
		t.Fatal("runnerUp = nil, want the highest-scoring different identity")
	}
	if runnerUp.MusicBrainzID != mbidRival || runnerUp.Score != 80 {
		t.Errorf("runnerUp = %s at %d, want %s at 80 (the strongest rival, not the next row)",
			runnerUp.MusicBrainzID, runnerUp.Score, mbidRival)
	}
}
