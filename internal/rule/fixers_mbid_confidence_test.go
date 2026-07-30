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

// TestFixMBID_StampsMachinePickedProvenance is the enumerability half of #2715.
// The audit message records how strong a given match was; this marker is what
// makes the SET of machine-picked artists findable, which is the recovery path
// for anything this rule already misidentified. Asserts the map entry the
// artist carries into persistence, not a flag.
func TestFixMBID_StampsMachinePickedProvenance(t *testing.T) {
	f := mbidFixer(
		provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: 100, Source: "musicbrainz"},
	)
	a := &artist.Artist{Name: "Radiohead"}

	fr := fixMBIDFor(t, f, a)

	if !fr.Fixed {
		t.Fatalf("expected Fixed=true, got false (%q)", fr.Message)
	}
	got := a.MetadataSources[artist.SourceKeyMusicBrainzID]
	if got != artist.SourceMachinePicked {
		t.Errorf("MetadataSources[%q] = %q, want %q: an adopted MBID must be enumerable as machine-picked",
			artist.SourceKeyMusicBrainzID, got, artist.SourceMachinePicked)
	}
}

// TestFixMBID_DoesNotStampProvenanceOnDecline is the negative half, and it is
// the one that catches an unconditional stamp. A test asserting only the
// positive case passes just as happily against an implementation that stamps
// every artist it looks at, including the ones it deliberately refused to
// touch -- which would poison the very query the marker exists to serve.
//
// The fixture is the ambiguous-rivals case: gates 1 and 2 pass, only the
// ambiguity margin rejects, so the fixer definitely reached the decision point
// rather than bailing early for some unrelated reason.
func TestFixMBID_DoesNotStampProvenanceOnDecline(t *testing.T) {
	f := mbidFixer(
		provider.ArtistSearchResult{Name: "Nirvana", MusicBrainzID: mbidRadiohead, Score: 100, Source: "musicbrainz"},
		provider.ArtistSearchResult{Name: "Nirvana", MusicBrainzID: mbidRival, Score: 98, Source: "musicbrainz"},
	)
	a := &artist.Artist{Name: "Nirvana"}

	fr := fixMBIDFor(t, f, a)

	if fr.Fixed {
		t.Fatalf("fixture precondition failed: expected a decline, got Fixed=true (%q)", fr.Message)
	}
	if _, ok := a.MetadataSources[artist.SourceKeyMusicBrainzID]; ok {
		t.Errorf("MetadataSources[%q] = %q, want ABSENT: a declined artist must not be marked machine-picked",
			artist.SourceKeyMusicBrainzID, a.MetadataSources[artist.SourceKeyMusicBrainzID])
	}
}

// TestFixMBID_ProvenanceStampPreservesOtherSources guards the map write itself.
// MetadataSources is shared with ApplyMetadata, which stores a provider name
// per metadata field; stamping the identity key must not clobber or replace
// that map. A naive implementation assigning a fresh map passes every other
// test here and silently destroys the artist's field-level source record.
func TestFixMBID_ProvenanceStampPreservesOtherSources(t *testing.T) {
	f := mbidFixer(
		provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: 100, Source: "musicbrainz"},
	)
	a := &artist.Artist{
		Name:            "Radiohead",
		MetadataSources: map[string]string{"biography": "lastfm", "genres": "audiodb"},
	}

	fixMBIDFor(t, f, a)

	if got := a.MetadataSources["biography"]; got != "lastfm" {
		t.Errorf("MetadataSources[biography] = %q, want %q: the stamp must not disturb field sources", got, "lastfm")
	}
	if got := a.MetadataSources["genres"]; got != "audiodb" {
		t.Errorf("MetadataSources[genres] = %q, want %q", got, "audiodb")
	}
	if got := a.MetadataSources[artist.SourceKeyMusicBrainzID]; got != artist.SourceMachinePicked {
		t.Errorf("MetadataSources[%q] = %q, want %q", artist.SourceKeyMusicBrainzID, got, artist.SourceMachinePicked)
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
// choice in artist.BestMBIDCandidates. Three providers agreeing on ONE id is the
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
			best: provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: artist.MBIDMinProviderScore},
		},
		{
			name:       "score one below the floor is rejected",
			best:       provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: artist.MBIDMinProviderScore - 1},
			wantReject: true,
		},
		{
			name:     "rival exactly at the margin is accepted",
			best:     provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: 100},
			runnerUp: &provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRival, Score: 100 - artist.MBIDAmbiguityMargin},
		},
		{
			name:       "rival one inside the margin is rejected",
			best:       provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: 100},
			runnerUp:   &provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRival, Score: 100 - artist.MBIDAmbiguityMargin + 1},
			wantReject: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rej := artist.EvaluateMBIDCandidate("Radiohead", &tc.best, tc.runnerUp)
			if tc.wantReject && rej == nil {
				t.Error("expected a rejection, got nil")
			}
			if !tc.wantReject && rej != nil {
				t.Errorf("expected acceptance, got rejection: %s", rej.Reason)
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
		{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: 90, Source: "musicbrainz"},
		{Name: "Other", MusicBrainzID: mbidThird, Score: 60, Source: "musicbrainz"},
		{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: 95, Source: "musicbrainz"},
		{Name: "Another", MusicBrainzID: mbidRival, Score: 80, Source: "musicbrainz"},
	}

	best, runnerUp := artist.BestMBIDCandidates(results)

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

// TestFixMBID_ProvenanceMarkerPersistsThroughPipeline is the end-to-end proof.
// Every other marker test asserts the in-memory Artist struct, which says
// nothing about whether the value survives the write. MetadataSources is
// persisted as a marshaled string map on the artists row, so a marker that
// the pipeline's writeback drops would leave the enumeration query returning
// nothing while all the unit tests stayed green.
//
// This runs the real pipeline against real SQLite, then RELOADS the artist and
// asserts the marker came back off the row.
func TestFixMBID_ProvenanceMarkerPersistsThroughPipeline(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding default rules: %v", err)
	}
	// RuleNFOHasMBID seeds enabled+auto already; isolate it so nothing else
	// mutates the artist during the pass.
	disableAllRulesExcept(t, db, RuleNFOHasMBID)

	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	engine.SetProviderAvailability(&stubProviderAvailability{available: allThreeAvailable()})

	metadataFixer := NewMetadataFixer(nil, testLogger())
	metadataFixer.orchestrator = &stubMBIDSearchOrchestrator{
		results: []provider.ArtistSearchResult{
			{Name: "Persisted Artist", MusicBrainzID: mbidRadiohead, Score: 100, Source: "musicbrainz"},
		},
	}
	p := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{metadataFixer}, nil, testLogger())

	a := &artist.Artist{
		Name:     "Persisted Artist",
		SortName: "Persisted Artist",
		Path:     t.TempDir(),
		// No MusicBrainzID: nfo_has_mbid must fire.
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	if _, err := p.RunAllScoped(ctx, RunScopeAll); err != nil {
		t.Fatalf("RunAllScoped: %v", err)
	}

	reloaded, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	// Precondition: without this the marker assertion below could pass
	// vacuously on an artist the fixer never actually touched.
	if reloaded.MusicBrainzID != mbidRadiohead {
		t.Fatalf("precondition failed: MusicBrainzID = %q, want %q (the fix did not run)",
			reloaded.MusicBrainzID, mbidRadiohead)
	}
	if got := reloaded.MetadataSources[artist.SourceKeyMusicBrainzID]; got != artist.SourceMachinePicked {
		t.Errorf("reloaded MetadataSources[%q] = %q, want %q: the marker must survive persistence or the re-review query finds nothing",
			artist.SourceKeyMusicBrainzID, got, artist.SourceMachinePicked)
	}
}

// TestFixMBID_PersistsLowercasedMBID is the case-normalization half of #2715.
// pathinfer.go keys MBID lookups on strings.ToLower(strings.TrimSpace(...)),
// so an adopted ID stored in whatever case a provider returned it in would
// silently fail those lookups. The fixture supplies a mixed-case UUID from
// the provider and asserts the PERSISTED row -- read back out of SQLite, not
// the in-memory struct -- comes back lowercased.
func TestFixMBID_PersistsLowercasedMBID(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding default rules: %v", err)
	}
	disableAllRulesExcept(t, db, RuleNFOHasMBID)

	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	engine.SetProviderAvailability(&stubProviderAvailability{available: allThreeAvailable()})

	mixedCaseMBID := "A74B1b7F-71a5-4011-9441-D0B5E4122711"
	if !strings.EqualFold(mixedCaseMBID, mbidRadiohead) || mixedCaseMBID == strings.ToLower(mixedCaseMBID) {
		t.Fatalf("fixture bug: %q must be a mixed-case variant of %q", mixedCaseMBID, mbidRadiohead)
	}

	metadataFixer := NewMetadataFixer(nil, testLogger())
	metadataFixer.orchestrator = &stubMBIDSearchOrchestrator{
		results: []provider.ArtistSearchResult{
			{Name: "Cased Artist", MusicBrainzID: mixedCaseMBID, Score: 100, Source: "musicbrainz"},
		},
	}
	p := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{metadataFixer}, nil, testLogger())

	a := &artist.Artist{
		Name:     "Cased Artist",
		SortName: "Cased Artist",
		Path:     t.TempDir(),
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	if _, err := p.RunAllScoped(ctx, RunScopeAll); err != nil {
		t.Fatalf("RunAllScoped: %v", err)
	}

	reloaded, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	// Precondition: without this the case assertion below could pass
	// vacuously on an artist the fixer never actually touched.
	if reloaded.MusicBrainzID == "" {
		t.Fatalf("precondition failed: MusicBrainzID is empty (the fix did not run)")
	}
	if reloaded.MusicBrainzID != mbidRadiohead {
		t.Errorf("persisted MusicBrainzID = %q, want %q (lowercased): a mixed-case id stored verbatim silently breaks the case-insensitive lookup keys in pathinfer.go",
			reloaded.MusicBrainzID, mbidRadiohead)
	}
}

// TestFixMBID_NonMusicBrainzHitIsNotARival is the regression guard for the
// most common configuration (MusicBrainz + Last.fm both enabled). Every
// non-MusicBrainz adapter sets Score = provider.NameSimilarity and carries an
// MBID it did not author, which is routinely stale or points at a merged
// entity. Here Last.fm reports a DIFFERENT id for the SAME artist at
// similarity 100; read as a rival that is a zero-point gap and the fix
// declines with the factually wrong reason "are different artists".
//
// The fixture is deliberately worst-case: identical scores, so if the Last.fm
// row is rival-eligible at all the ambiguity gate MUST reject. Adoption is
// therefore only possible when the rival filter is source-aware.
func TestFixMBID_NonMusicBrainzHitIsNotARival(t *testing.T) {
	f := mbidFixer(
		provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: 100, Source: "musicbrainz"},
		provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRival, Score: 100, Source: "lastfm"},
	)
	a := &artist.Artist{Name: "Radiohead"}

	fr := fixMBIDFor(t, f, a)

	if !fr.Fixed {
		t.Fatalf("expected Fixed=true: a stale Last.fm MBID for the same artist is not ambiguity evidence (%q)", fr.Message)
	}
	if a.MusicBrainzID != mbidRadiohead {
		t.Errorf("a.MusicBrainzID = %q, want %q (the MusicBrainz-issued id)", a.MusicBrainzID, mbidRadiohead)
	}
	if !strings.Contains(fr.Message, "no rival") {
		t.Errorf("message %q: a non-MusicBrainz hit must not be reported as a rival", fr.Message)
	}
}

// TestFixMBID_MusicBrainzRivalStillGatesAmbiguity is the counterweight: the
// source filter must narrow WHO can be a rival, not disable the gate. Same
// fixture shape as the test above, but the disagreeing row is MusicBrainz's
// own -- two genuinely different entries in one relevance ordering -- so the
// margin gate must still reject. Without this, deleting the rival loop
// entirely would pass every other test here.
func TestFixMBID_MusicBrainzRivalStillGatesAmbiguity(t *testing.T) {
	f := mbidFixer(
		provider.ArtistSearchResult{Name: "Nirvana", MusicBrainzID: mbidRadiohead, Score: 100, Source: "musicbrainz"},
		provider.ArtistSearchResult{Name: "Nirvana", MusicBrainzID: mbidRival, Score: 100, Source: "musicbrainz"},
	)
	a := &artist.Artist{Name: "Nirvana"}

	fr := fixMBIDFor(t, f, a)

	if fr.Fixed {
		t.Fatalf("expected Fixed=false: two MusicBrainz entries at the same score are ambiguous (%q)", fr.Message)
	}
	if !strings.Contains(fr.Message, "ambiguous") {
		t.Errorf("message %q should say ambiguity was the reason", fr.Message)
	}
}

// TestFixMBID_UppercaseMBIDIsNotItsOwnRival guards the case-sensitivity of the
// rival compare. artist.IsValidMBID accepts A-F as well as a-f, so a provider
// returning an uppercase UUID clears the shape filter and then, under a
// case-SENSITIVE ==, compares unequal to the identical id in lowercase and
// becomes a rival of itself at a 2-point gap -- declining a match every
// provider actually agrees on.
func TestFixMBID_UppercaseMBIDIsNotItsOwnRival(t *testing.T) {
	f := mbidFixer(
		provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: 100, Source: "musicbrainz"},
		provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: strings.ToUpper(mbidRadiohead), Score: 98, Source: "musicbrainz"},
	)
	a := &artist.Artist{Name: "Radiohead"}

	fr := fixMBIDFor(t, f, a)

	if !fr.Fixed {
		t.Fatalf("expected Fixed=true: the same id in a different case is the same identity (%q)", fr.Message)
	}
	if a.MusicBrainzID != mbidRadiohead {
		t.Errorf("a.MusicBrainzID = %q, want %q", a.MusicBrainzID, mbidRadiohead)
	}
	if !strings.Contains(fr.Message, "no rival") {
		t.Errorf("message %q: a case variant of the winning id is not a rival", fr.Message)
	}
}

// TestFixMBID_RejectsLiteralJustBelowScoreFloor pins MBIDMinProviderScore to a
// LITERAL. The boundary table uses the constant symbolically, so its cases
// move with any change to it, and the other reject fixture sits at 55 -- far
// enough below that the floor could be lowered a long way and stay green. 84
// is one point under the intended floor, so lowering the constant at all makes
// this test go red. The name is an exact match and there is no rival, so the
// score floor is the only gate that can fire.
func TestFixMBID_RejectsLiteralJustBelowScoreFloor(t *testing.T) {
	f := mbidFixer(
		provider.ArtistSearchResult{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: 84, Source: "musicbrainz"},
	)
	a := &artist.Artist{Name: "Radiohead"}

	fr := fixMBIDFor(t, f, a)

	if fr.Fixed {
		t.Fatalf("expected Fixed=false: 84 is below the intended 85 confidence floor (%q)", fr.Message)
	}
	if a.MusicBrainzID != "" {
		t.Errorf("a.MusicBrainzID = %q, want empty", a.MusicBrainzID)
	}
	if !strings.Contains(fr.Message, "confidence floor") {
		t.Errorf("message %q should say the score floor was the reason", fr.Message)
	}
}

// TestBestMBIDCandidates_RivalMustBeMusicBrainzSourced exercises the selector
// directly: a stronger non-MusicBrainz disagreeing hit must be passed over in
// favor of a weaker MusicBrainz one, which a mere "skip lastfm" hack that
// returned the first eligible row would get wrong.
func TestBestMBIDCandidates_RivalMustBeMusicBrainzSourced(t *testing.T) {
	results := []provider.ArtistSearchResult{
		{Name: "Radiohead", MusicBrainzID: mbidRadiohead, Score: 100, Source: "musicbrainz"},
		{Name: "Radiohead", MusicBrainzID: mbidThird, Score: 95, Source: "audiodb"},
		{Name: "Radiohead", MusicBrainzID: mbidRival, Score: 60, Source: "musicbrainz"},
	}

	best, runnerUp := artist.BestMBIDCandidates(results)

	if best == nil || best.MusicBrainzID != mbidRadiohead {
		t.Fatalf("best = %+v, want the mbidRadiohead row", best)
	}
	if runnerUp == nil {
		t.Fatal("runnerUp = nil, want the MusicBrainz-sourced rival")
	}
	if runnerUp.MusicBrainzID != mbidRival || runnerUp.Score != 60 {
		t.Errorf("runnerUp = %s at %d, want %s at 60: the higher-scoring audiodb row is not rival-eligible",
			runnerUp.MusicBrainzID, runnerUp.Score, mbidRival)
	}
}
