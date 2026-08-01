package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/web/templates"
)

// TestEnrichWithAlbumComparison_CorrectCandidateOutsideFirstThree reproduces
// the defect in #2885 with the shape the maintainer hit: the correct artist is
// returned by the provider outside the first three results, and the local
// library has a healthy set of albums that match it.
//
// Before the fix, enrichment ran over the first three candidates in provider
// order, so the correct match got no album comparison at all and rendered as
// though it had nothing in common with the library, while three arbitrary
// candidates carried badges. It must now be scored AND rank first.
func TestEnrichWithAlbumComparison_CorrectCandidateOutsideFirstThree(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	const local1, local2, local3 = "A Maze of Grace", "Oxygen", "Stand"

	// The correct match sits LAST, behind strictly MORE decoys than the
	// evidence budget allows. That is load-bearing: with fewer candidates than
	// albumEvidenceBudget every row gets scored no matter how the list is
	// ordered, and the test passes without any ranking at all -- which is
	// exactly how an earlier version of this fixture passed while the pre-fetch
	// sort was deleted. Only a correct candidate positioned OUTSIDE the budget
	// window can prove the ranking runs before the budget is spent.
	//
	// Each decoy also carries a HIGHER provider score than the correct match,
	// so a ranking that leaned on the provider's own number, or that sorted
	// ascending, would strand the right answer outside the window too.
	decoyNames := []string{
		"Avalon Blues", "Avalon Rising", "The Avalon Faction", "Avalon Quartet",
		"Avalon Sessions", "Avalon Underground", "Avalon Strings", "Avalon Six",
		"Avalon Community Choir", "Avalon Reunion Band",
	}
	results := make([]provider.ArtistSearchResult, 0, len(decoyNames)+1)
	for i, n := range decoyNames {
		results = append(results, provider.ArtistSearchResult{
			Name:          n,
			MusicBrainzID: fmt.Sprintf("mbid-decoy-%02d", i),
			Score:         100,
		})
	}
	results = append(results, provider.ArtistSearchResult{
		Name: "Avalon", MusicBrainzID: "mbid-correct", Score: 50,
	})
	if len(decoyNames) < albumEvidenceBudget {
		t.Fatalf("fixture defect: %d decoys does not exceed the budget of %d, so every candidate would be scored regardless of order",
			len(decoyNames), albumEvidenceBudget)
	}

	installAudioDBOrchestrator(t, r, nil, func(_ context.Context, mbid string) ([]provider.ReleaseGroupInfo, error) {
		if mbid == "mbid-correct" {
			return []provider.ReleaseGroupInfo{
				{Title: local1}, {Title: local2}, {Title: local3},
			}, nil
		}
		return []provider.ReleaseGroupInfo{{Title: "Something Else Entirely"}}, nil
	})

	got := r.enrichWithAlbumComparison(context.Background(), "Avalon", results, foundAlbums(local1, local2, local3))
	if len(got) != len(results) {
		t.Fatalf("len = %d, want %d (ranking must not drop candidates)", len(got), len(results))
	}

	// The correct candidate must have been scored despite its provider
	// position -- the core of the issue.
	var correct *int
	for i := range got {
		if got[i].Result.MusicBrainzID == "mbid-correct" {
			idx := i
			correct = &idx
		}
	}
	if correct == nil {
		t.Fatal("correct candidate missing from results")
	}
	if got[*correct].AlbumComparison == nil {
		t.Fatal("correct candidate has no AlbumComparison: it fell outside the evidence budget, which is the defect")
	}
	if pct := got[*correct].AlbumComparison.MatchPercent; pct != 100 {
		t.Errorf("correct candidate MatchPercent = %d, want 100", pct)
	}
	// And it must rank first, so the operator sees it without scrolling.
	if *correct != 0 {
		t.Errorf("correct candidate at index %d, want 0 (best album overlap must rank first)", *correct)
	}
	if got[0].Result.MusicBrainzID != "mbid-correct" {
		t.Errorf("top candidate = %q, want mbid-correct", got[0].Result.MusicBrainzID)
	}
}

// TestEnrichWithAlbumComparison_BudgetIsBoundedAndMarked pins the latency
// budget. Each comparison is a rate-limited provider call, so the number of
// fetches must stay bounded no matter how many candidates a provider returns,
// and the candidates that were skipped must say so rather than rendering
// identically to one that was measured and found not to match.
func TestEnrichWithAlbumComparison_BudgetIsBoundedAndMarked(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	results := make([]provider.ArtistSearchResult, 20)
	for i := range results {
		results[i] = provider.ArtistSearchResult{
			Name:          fmt.Sprintf("Candidate %02d", i),
			MusicBrainzID: fmt.Sprintf("mbid-%02d", i),
		}
	}

	var mu sync.Mutex
	fetches := 0
	installAudioDBOrchestrator(t, r, nil, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
		mu.Lock()
		fetches++
		mu.Unlock()
		return []provider.ReleaseGroupInfo{{Title: "Unrelated"}}, nil
	})

	got := r.enrichWithAlbumComparison(context.Background(), "Candidate 00", results, foundAlbums("Local Album"))

	mu.Lock()
	defer mu.Unlock()
	if fetches != albumEvidenceBudget {
		t.Errorf("fetches = %d, want %d (the budget bounds provider traffic)", fetches, albumEvidenceBudget)
	}
	// A literal ceiling as well as the constant-relative check above. Each
	// fetch is a rate-limited provider call inside a synchronous request, so
	// an accidental bump to some much larger budget is a latency regression
	// that the self-referential comparison alone would happily accept.
	const maxTolerableFetches = 12
	if fetches > maxTolerableFetches {
		t.Errorf("fetches = %d, which exceeds %d: at roughly a second per call this blocks the request long enough for a proxy to sever it",
			fetches, maxTolerableFetches)
	}

	scored, notScored := 0, 0
	for _, c := range got {
		if c.AlbumComparison != nil {
			scored++
		}
		if c.AlbumsNotScored {
			notScored++
		}
	}
	if scored != albumEvidenceBudget {
		t.Errorf("scored = %d, want %d", scored, albumEvidenceBudget)
	}
	if notScored != len(results)-albumEvidenceBudget {
		t.Errorf("notScored = %d, want %d (every skipped candidate must be marked)", notScored, len(results)-albumEvidenceBudget)
	}
	// A skipped candidate must never be mistaken for one that could not be read.
	for _, c := range got {
		if c.AlbumsNotScored && c.AlbumsUnavailable {
			t.Errorf("candidate %q marked both not-scored and unavailable; those are different states",
				c.Result.MusicBrainzID)
		}
	}
}

// TestEnrichWithAlbumComparison_MeasuredZeroOutranksUnscored pins a deliberate
// ordering choice: a candidate measured against the library and found to share
// nothing still outranks one that was never measured. The first is a finding;
// the second is unknown, and presenting unknown as though it were worse than a
// known-bad match would mislead the operator.
func TestEnrichWithAlbumComparison_MeasuredZeroOutranksUnscored(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	// The uncompared candidate is uncompared for a reason INDEPENDENT of its
	// rank: it carries no MusicBrainz ID, so there is nothing to look up no
	// matter how highly it ranks. That independence is what makes the tier
	// observable at all.
	//
	// Two earlier fixtures failed to test this and passed anyway. The first
	// gave every candidate the same name and score, so the estimate tied and
	// a stable sort alone produced the expected order. The second ranked the
	// intended tail HIGHER, but the budget always attempts the top-K of the
	// pre-fetch order, so those high-ranking candidates were exactly the ones
	// that got compared and the "unscored tail" never existed. Both passed
	// with the tier deleted.
	//
	// Here the MBID-less candidate is named to match the query EXACTLY, so it
	// wins the rank estimate outright, while the compared candidate has a
	// weaker name and measures 0% overlap. Only the scored-above-unscored
	// tier can put the 0% candidate first.
	results := []provider.ArtistSearchResult{
		{Name: "Same Name", Score: 100}, // no MBID: never comparable
		{Name: "Same Name Extended Edition", MusicBrainzID: "mbid-scored", Score: 100},
	}

	// Precondition: the uncomparable candidate must genuinely out-rank the
	// comparable one on the network-free estimate, or this passes vacuously.
	if rankScore("Same Name", results[0]) <= rankScore("Same Name", results[1]) {
		t.Fatalf("fixture defect: the uncomparable candidate (%d) does not out-rank the comparable one (%d), so the album-evidence tier is not the deciding signal",
			rankScore("Same Name", results[0]), rankScore("Same Name", results[1]))
	}

	// The compared candidate shares nothing with the library, so it measures
	// 0%: known-bad, versus the other candidate's unknown.
	installAudioDBOrchestrator(t, r, nil, func(_ context.Context, mbid string) ([]provider.ReleaseGroupInfo, error) {
		if mbid != "mbid-scored" {
			t.Errorf("unexpected fetch for %q", mbid)
		}
		return []provider.ReleaseGroupInfo{{Title: "Nothing In Common"}}, nil
	})

	got := r.enrichWithAlbumComparison(context.Background(), "Same Name", results, foundAlbums("Local Album"))
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}

	if got[0].Result.MusicBrainzID != "mbid-scored" {
		t.Errorf("top candidate = %q, want mbid-scored: a candidate measured at 0%% is a FINDING and must outrank one that was never compared, even though the latter has the better name",
			got[0].Result.Name)
	}
	if got[0].AlbumComparison == nil || got[0].AlbumComparison.MatchPercent != 0 {
		t.Errorf("top candidate comparison = %+v, want a measured 0%%", got[0].AlbumComparison)
	}
	if got[0].AlbumsNotScored {
		t.Error("a measured candidate must not be marked not-compared")
	}
	if !got[1].AlbumsNotScored {
		t.Error("the uncomparable candidate must be marked not-compared")
	}
	if got[1].AlbumComparison != nil {
		t.Errorf("candidate without an MBID has a comparison: %+v", got[1].AlbumComparison)
	}
}

// TestEnrichWithAlbumComparison_EvidenceOverridesNameRanking pins the
// post-fetch re-sort, which is the step that lets measured evidence overturn
// the name-similarity guess.
//
// The two signals are deliberately put in conflict: the candidate whose name
// matches the query best owns NONE of the local albums, while a candidate
// with a weaker name owns all of them. Without the re-sort the name-based
// order survives and the operator is shown a 0%-overlap candidate above a
// 100% one. Every other test in this file has the two signals AGREEING, which
// is why the re-sort could previously be deleted with the suite still green.
func TestEnrichWithAlbumComparison_EvidenceOverridesNameRanking(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	const local1, local2 = "Local One", "Local Two"

	results := []provider.ArtistSearchResult{
		// Exact name match, owns nothing the operator has.
		{Name: "Avalon", MusicBrainzID: "mbid-name-twin", Score: 100},
		// Weaker name, owns the whole library.
		{Name: "Avalon Family Band", MusicBrainzID: "mbid-real-match", Score: 10},
	}

	// Precondition: the name-only ranking must genuinely prefer the WRONG
	// candidate, or the re-sort is not the deciding factor and this passes
	// vacuously.
	if rankScore("Avalon", results[0]) <= rankScore("Avalon", results[1]) {
		t.Fatalf("fixture defect: the name ranking does not favor the twin (%d vs %d), so the re-sort is not what this test measures",
			rankScore("Avalon", results[0]), rankScore("Avalon", results[1]))
	}

	installAudioDBOrchestrator(t, r, nil, func(_ context.Context, mbid string) ([]provider.ReleaseGroupInfo, error) {
		if mbid == "mbid-real-match" {
			return []provider.ReleaseGroupInfo{{Title: local1}, {Title: local2}}, nil
		}
		return []provider.ReleaseGroupInfo{{Title: "Nothing In Common"}}, nil
	})

	got := r.enrichWithAlbumComparison(context.Background(), "Avalon", results, foundAlbums(local1, local2))
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Result.MusicBrainzID != "mbid-real-match" {
		t.Errorf("top candidate = %q, want mbid-real-match: a measured 100%% album overlap must outrank a better name with 0%% overlap",
			got[0].Result.MusicBrainzID)
	}
	if got[0].AlbumComparison == nil || got[0].AlbumComparison.MatchPercent != 100 {
		t.Errorf("top candidate comparison = %+v, want a 100%% match", got[0].AlbumComparison)
	}
}

// rendersAlbumState mirrors the template's badge condition in
// web/templates/artist_refresh.templ: a candidate shows an album badge only
// when it has a real comparison, or is flagged unavailable, or is flagged
// not-scored. Anything else renders NOTHING, which an operator reads as a
// measured zero rather than as an absent measurement.
func rendersAlbumState(c templates.DisambiguationCandidate) bool {
	return (c.AlbumComparison != nil && c.AlbumComparison.LocalCount > 0) ||
		c.AlbumsUnavailable || c.AlbumsNotScored
}

// TestEnrichWithAlbumComparison_EveryCandidateExplainsItself is the invariant
// behind the whole badge: no candidate may come back silent. A row with no
// comparison and no flag renders bare, and bare is indistinguishable from
// "measured against your library and matched nothing" -- the false claim this
// work exists to prevent.
//
// The three ways to end up without a comparison are covered together: ranked
// below the budget, carrying no MusicBrainz ID (every Discogs result), and a
// provider fetch that failed.
func TestEnrichWithAlbumComparison_EveryCandidateExplainsItself(t *testing.T) {
	t.Parallel()

	t.Run("no mbid, over budget, and fetch errors", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)

		var results []provider.ArtistSearchResult
		// Enough MBID-bearing candidates to overflow the budget.
		for i := 0; i < albumEvidenceBudget+2; i++ {
			results = append(results, provider.ArtistSearchResult{
				Name:          fmt.Sprintf("With MBID %02d", i),
				MusicBrainzID: fmt.Sprintf("mbid-%02d", i),
				Source:        "musicbrainz",
			})
		}
		// Plus candidates with no MBID at all, as Discogs returns.
		for i := 0; i < 3; i++ {
			results = append(results, provider.ArtistSearchResult{
				Name:   fmt.Sprintf("No MBID %02d", i),
				Source: "discogs",
			})
		}

		// Half the fetches fail, so the error branch is exercised alongside
		// the budget and no-MBID branches in one pass.
		installAudioDBOrchestrator(t, r, nil, func(_ context.Context, mbid string) ([]provider.ReleaseGroupInfo, error) {
			if strings.HasSuffix(mbid, "1") || strings.HasSuffix(mbid, "3") {
				return nil, errors.New("provider unavailable")
			}
			return []provider.ReleaseGroupInfo{{Title: "Unrelated"}}, nil
		})

		got := r.enrichWithAlbumComparison(context.Background(), "With MBID 00", results, foundAlbums("Local Album"))
		if len(got) != len(results) {
			t.Fatalf("len = %d, want %d", len(got), len(results))
		}
		for _, c := range got {
			if !rendersAlbumState(c) {
				t.Errorf("candidate %q (mbid %q) renders no album state at all: an operator cannot tell it apart from a measured 0%% match",
					c.Result.Name, c.Result.MusicBrainzID)
			}
		}
	})

	// The early returns are the ones a marking-on-the-way-out implementation
	// misses: they leave the function before the budget loop runs at all.
	// EvidenceNone is the sharpest -- the folder WAS read and genuinely held
	// nothing, so AlbumsUnavailable is correctly false and every row would
	// render bare.
	t.Run("local albums genuinely empty", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		results := []provider.ArtistSearchResult{
			{Name: "A", MusicBrainzID: "mbid-a"},
			{Name: "B"},
		}
		installAudioDBOrchestrator(t, r, nil, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
			t.Error("no fetch may run when the artist has no local albums")
			return nil, nil
		})
		got := r.enrichWithAlbumComparison(context.Background(), "A", results, foundNoAlbums())
		for _, c := range got {
			if !rendersAlbumState(c) {
				t.Errorf("candidate %q renders no album state on the EvidenceNone early return", c.Result.Name)
			}
			if c.AlbumsUnavailable {
				t.Errorf("candidate %q flagged unavailable, but the folder was read successfully and was empty", c.Result.Name)
			}
		}
	})

	t.Run("no provider registry", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		r.providerRegistry = nil
		results := []provider.ArtistSearchResult{{Name: "A", MusicBrainzID: "mbid-a"}}
		got := r.enrichWithAlbumComparison(context.Background(), "A", results, foundAlbums("Local Album"))
		for _, c := range got {
			if !rendersAlbumState(c) {
				t.Errorf("candidate %q renders no album state when no provider registry is configured", c.Result.Name)
			}
		}
	})

	t.Run("local albums unreadable", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		results := []provider.ArtistSearchResult{
			{Name: "A", MusicBrainzID: "mbid-a"},
			{Name: "B"},
		}
		installAudioDBOrchestrator(t, r, nil, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
			t.Error("no fetch may run when the local album set is unknown")
			return nil, nil
		})
		got := r.enrichWithAlbumComparison(context.Background(), "A", results, unknownAlbums())
		for _, c := range got {
			if !rendersAlbumState(c) {
				t.Errorf("candidate %q renders no album state", c.Result.Name)
			}
			if c.AlbumsUnavailable && c.AlbumsNotScored {
				t.Errorf("candidate %q is flagged both unavailable and not-scored; those are different states",
					c.Result.Name)
			}
		}
	})
}

// TestRankScore_PrefersNameOverProviderScore pins the weighting: a candidate
// whose name actually matches the query outranks one the provider happened to
// score highly, because the operator's question is "is this the artist I
// typed", not "which row did MusicBrainz like".
func TestRankScore_PrefersNameOverProviderScore(t *testing.T) {
	t.Parallel()

	// Chosen so the 2:1 weighting is the deciding factor rather than incidental:
	// at weight 1 the loose candidate WINS this pairing (sim 47 + 100 = 147 vs
	// 100 + 50 = 150 is close, and the margin inverts as the provider gap
	// widens), so a silent drop of the doubling flips the assertion instead of
	// leaving it green.
	exact := provider.ArtistSearchResult{Name: "Avalon", Score: 1}
	loose := provider.ArtistSearchResult{Name: "Avalon Rising Quartet", Score: 100}

	if rankScore("Avalon", exact) <= rankScore("Avalon", loose) {
		t.Errorf("exact name match (%d) must outrank a loose match with a higher provider score (%d)",
			rankScore("Avalon", exact), rankScore("Avalon", loose))
	}

	// Pin the weighting itself, so the relationship above cannot survive a
	// change to how the two signals are combined.
	nameOnly := provider.ArtistSearchResult{Name: "Avalon", Score: 0}
	if got, want := rankScore("Avalon", nameOnly), 200; got != want {
		t.Errorf("rankScore for an exact name with no provider score = %d, want %d (name weighted 2:1)", got, want)
	}
}

// TestRankScore_UsesSortName covers the half of #2820 that needs no fetch:
// SortName is already carried on every search result, so an artist listed as
// "Beatles, The" must not be penalized against a query of "The Beatles".
func TestRankScore_UsesSortName(t *testing.T) {
	t.Parallel()

	withSort := provider.ArtistSearchResult{Name: "Beatles, The", SortName: "The Beatles"}
	withoutSort := provider.ArtistSearchResult{Name: "Beatles, The"}

	if rankScore("The Beatles", withSort) <= rankScore("The Beatles", withoutSort) {
		t.Error("a matching SortName must raise the rank score")
	}
}
