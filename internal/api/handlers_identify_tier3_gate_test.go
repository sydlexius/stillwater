package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// tier2SearchFailsRouter builds a server whose TIER 2 search fails while its
// TIER 3 search succeeds, which is how an artist WITH readable local albums
// legitimately reaches the name-only tier in production (a provider hiccup on
// the folder-name query, then a successful artist-name query).
//
// This routing is what makes the two tests below non-vacuous. With a healthy
// Tier 2, a zero-overlap candidate is already refused by the pre-existing 30%
// review floor, so a test that stopped there would prove the OLD threshold
// works, not the new gate. Reaching Tier 3 with EvidenceFound puts the
// candidate in front of the album gate with nothing else standing in the way.
//
// The two tiers are told apart by their query: Tier 2 searches on the artist
// FOLDER name and Tier 3 on the artist's NAME.
func tier2SearchFailsRouter(t *testing.T, artistName string, tier3Hit provider.ArtistSearchResult, releaseGroups func(ctx context.Context, mbid string) ([]provider.ReleaseGroupInfo, error)) (*Router, *artist.Service) {
	t.Helper()
	return newIdentifyTestServer(t,
		func(_ context.Context, query string) ([]provider.ArtistSearchResult, error) {
			if query != artistName {
				// The Tier 2 (folder-name) query.
				return nil, errors.New("simulated provider failure on the tier 2 query")
			}
			return []provider.ArtistSearchResult{tier3Hit}, nil
		},
		releaseGroups,
	)
}

// TestIdentifyDeclinesWhenAlbumEvidenceIsUnknown is THE test of issue #2828 on
// the write path.
//
// The candidate is deliberately the strongest one the pipeline can see: an
// EXACT name match at score 100, uncontested, from MusicBrainz itself. It
// clears artist.MBIDMinProviderScore, artist.MBIDMinNameSimilarity and
// artist.MBIDAmbiguityMargin -- every #2813 gate. It is also a real entity with
// a full catalogue, so the zero-release guard is not what stops it.
//
// The ONLY thing standing between it and the artist's row is that the artist
// has NO PATH, so the local album set is EvidenceUnknown. Before #2828 that
// state was not merely a failure to object: it ROUTED the artist to Tier 3,
// which performed no catalogue check at all, so "we could not look" acted as
// permission. In the production snapshot 43% of artists take that route.
func TestIdentifyDeclinesWhenAlbumEvidenceIsUnknown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	r, artistSvc := newIdentifyTestServer(t,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			return []provider.ArtistSearchResult{{
				Name:          "Gate Unknown",
				MusicBrainzID: mbidGateRealArtist,
				Score:         100,
				Source:        string(provider.NameMusicBrainz),
			}}, nil
		},
		func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
			// A real, well-populated catalogue: the candidate is not an empty
			// stub, so nothing but the local evidence state can refuse it.
			return []provider.ReleaseGroupInfo{
				{Title: "First Record"},
				{Title: "Second Record"},
				{Title: "Third Record"},
			}, nil
		},
	)

	// No Path at all: FilesystemAlbumSource reports EvidenceUnknown.
	a := seedBlankArtist(t, artistSvc, "Gate Unknown", "")

	got := r.identifyArtist(ctx, a, nil)
	if got.Outcome == outcomeAutoLinked {
		t.Errorf("Outcome = autoLinked, want anything else: an artist whose albums were never read must not be written unattended")
	}
	// THE ROW is the assertion. An outcome counter can be wrong in either
	// direction independently of what landed in the database.
	if mbid := reloadMBID(t, artistSvc, a.ID); mbid != "" {
		t.Errorf("stored MBID = %q, want blank; the album-evidence gate did not stop the write", mbid)
	}
}

// TestIdentifySkipToTier3PathIsClosed reproduces the exact routing defect.
//
// The fixture is the OLD Tier 3 auto-link condition verbatim: an artist with no
// local albums plus exactly ONE search result scoring >= 90. The candidate also
// carries ZERO release groups, which is the shape all 18 measured wrong
// adoptions had. Before this change the artist reached Tier 3 precisely BECAUSE
// it had no albums, and Tier 3 wrote the ID without ever asking about a
// catalogue.
func TestIdentifySkipToTier3PathIsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var releaseGroupCalls int
	r, artistSvc := newIdentifyTestServer(t,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			// Exactly one result at score >= 90: the old auto-link condition.
			return []provider.ArtistSearchResult{{
				Name:          "Gate Skip",
				MusicBrainzID: mbidGateEmptyStub,
				Score:         95,
				Source:        string(provider.NameMusicBrainz),
			}}, nil
		},
		func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
			releaseGroupCalls++
			return nil, nil // an empty MusicBrainz stub
		},
	)

	a := seedBlankArtist(t, artistSvc, "Gate Skip", "")

	got := r.identifyArtist(ctx, a, nil)
	if got.Outcome == outcomeAutoLinked {
		t.Errorf("Outcome = autoLinked, want anything else: the skip-to-Tier-3 path must no longer auto-link")
	}
	if mbid := reloadMBID(t, artistSvc, a.ID); mbid != "" {
		t.Errorf("stored MBID = %q, want blank", mbid)
	}
	// With no local album determination there is nothing to compare a catalogue
	// against, so the gate declines before spending a provider round-trip. This
	// is a cost assertion, not a correctness one -- the decision is identical
	// either way -- but it pins the short-circuit so a later refactor cannot
	// quietly reintroduce a per-candidate fetch on the 43% path.
	if releaseGroupCalls != 0 {
		t.Errorf("GetReleaseGroups calls = %d, want 0: an Unknown album set has nothing to compare", releaseGroupCalls)
	}
}

// TestIdentifyDeclinesZeroReleaseCandidateAtFullNameMatch isolates the measured
// 18/18 case: the artist HAS a readable album directory, the candidate matches
// its name perfectly, and the candidate's MusicBrainz entity carries no release
// groups whatsoever.
//
// This is the shape a name-only gate structurally cannot catch. An entity with
// an empty catalogue matches every name query it is returned for and owns
// nothing that could contradict the match.
//
// It routes through tier2SearchFailsRouter so the candidate is judged by the
// ALBUM GATE rather than by the pre-existing 30% review floor -- see that
// helper's comment for why a Tier 2 fixture would prove the wrong thing.
func TestIdentifyDeclinesZeroReleaseCandidateAtFullNameMatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	r, artistSvc := tier2SearchFailsRouter(t, "Gate Empty",
		provider.ArtistSearchResult{
			Name:          "Gate Empty",
			MusicBrainzID: mbidGateEmptyStub,
			Score:         100,
			Source:        string(provider.NameMusicBrainz),
		},
		func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
			return nil, nil // zero release groups
		},
	)

	// A real album directory, so the local side is a positive determination and
	// the ONLY thing wrong is the candidate's empty catalogue.
	dir := albumDir(t, "First Record", "Second Record")
	a := seedBlankArtist(t, artistSvc, "Gate Empty", dir)

	got := r.identifyArtist(ctx, a, nil)
	if got.Outcome == outcomeAutoLinked {
		t.Errorf("Outcome = autoLinked, want anything else: a candidate with no release groups corroborates nothing")
	}
	if mbid := reloadMBID(t, artistSvc, a.ID); mbid != "" {
		t.Errorf("stored MBID = %q, want blank", mbid)
	}

	// Discriminator: the identical routing with a candidate that HAS a matching
	// catalogue must auto-link, so the decline above is attributable to the
	// empty catalogue rather than to the Tier 2 search failure.
	r2, artistSvc2 := tier2SearchFailsRouter(t, "Gate Empty",
		provider.ArtistSearchResult{
			Name:          "Gate Empty",
			MusicBrainzID: mbidGateRealArtist,
			Score:         100,
			Source:        string(provider.NameMusicBrainz),
		},
		func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
			return []provider.ReleaseGroupInfo{{Title: "First Record"}, {Title: "Second Record"}}, nil
		},
	)
	b := seedBlankArtist(t, artistSvc2, "Gate Empty", albumDir(t, "First Record", "Second Record"))
	if got := r2.identifyArtist(ctx, b, nil); got.Outcome != outcomeAutoLinked {
		t.Fatalf("precondition failed: same routing with a populated catalogue = %v, want autoLinked", got.Outcome)
	}
}

// TestIdentifyDeclinesWhenBothSidesAreEmpty covers the EvidenceNone row on the
// live path: the artist's directory was READ and holds no albums, which is a
// positive determination rather than a failure to look.
//
// An empty local catalogue is not evidence for ANY candidate. There is nothing
// on the local side to agree with, so an automated pass must not adopt an
// identity no matter how good the candidate looks.
//
// THIS IS DEFENSE IN DEPTH, NOT THE DIRECT GUARD. It exercises EvidenceNone
// through the whole live write path, which is worth having, but it cannot
// isolate the gate's `case EvidenceNone` arm: deleting that arm sends execution
// to the default arm, which also declines, so the outcome here is unchanged.
// The direct guard is TestEvaluateAlbumGateEvidenceNoneDeclines in
// internal/artist/albumgate_test.go, which asserts the REASON and so tells the
// EvidenceNone branch apart from the default one. Keep both: this one proves
// the state survives the plumbing, that one proves the branch exists.
//
// The candidate deliberately carries a REAL catalogue so the fixture
// discriminates along the axis being tested. Pairing EvidenceNone with a
// zero-release candidate (as this test first did) lets the zero-release guard
// refuse first, which would make the decline attributable to the wrong check.
func TestIdentifyDeclinesWhenBothSidesAreEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	r, artistSvc := newIdentifyTestServer(t,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			return []provider.ArtistSearchResult{{
				Name:          "Gate Both Empty",
				MusicBrainzID: mbidGateRealArtist,
				Score:         100,
				Source:        string(provider.NameMusicBrainz),
			}}, nil
		},
		func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
			// A REAL catalogue, so the zero-release guard cannot be what
			// refuses this candidate. The local side's EvidenceNone has to do
			// the work, which is the axis under test.
			return []provider.ReleaseGroupInfo{
				{Title: "First Record"},
				{Title: "Second Record"},
			}, nil
		},
	)

	// An EXISTING but EMPTY directory: read successfully, held nothing. This is
	// EvidenceNone, and it is materially different from the no-path fixture
	// above -- that one is EvidenceUnknown. Both decline, for different stated
	// reasons, and the pair is what keeps the tri-state from collapsing.
	a := seedBlankArtist(t, artistSvc, "Gate Both Empty", albumDir(t))

	got := r.identifyArtist(ctx, a, nil)
	if got.Outcome == outcomeAutoLinked {
		t.Errorf("Outcome = autoLinked, want anything else: two empty catalogues are not agreement")
	}
	if mbid := reloadMBID(t, artistSvc, a.ID); mbid != "" {
		t.Errorf("stored MBID = %q, want blank", mbid)
	}
}

// TestIdentifyTier3DeclinesWhenARivalCatalogueAlsoClearsTheFloor is the
// CONTESTED case, and it is the one Tier 3 could not reach before.
//
// The setup is two DISTINCT MusicBrainz entities sharing a name, scored 100 and
// 85, BOTH carrying the local artist's exact records:
//
//   - The 15-point spread deliberately CLEARS artist.MBIDAmbiguityMargin (10),
//     so the #2813 name gates raise no objection. That is the whole point: the
//     margin compares provider RELEVANCE SCORES, whereas the question that
//     matters here is whether more than one catalogue AGREES. They are different
//     questions, and the score gate passing tells you nothing about the second.
//   - Tier 2 in this exact situation REFUSES, because its caller guards
//     `len(above70) == 1` and two candidates clear the floor. Tier 3 used to
//     hardcode UncontestedBest: true, which made the gate's contested branch
//     structurally unreachable, so the identical evidence produced opposite
//     answers depending only on which tier the artist happened to route through.
//
// Two entities both carrying the artist's records are not corroboration of
// either. The album evidence does not discriminate, so neither may be adopted
// unattended; the operator still sees both in the review queue.
func TestIdentifyTier3DeclinesWhenARivalCatalogueAlsoClearsTheFloor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Both entities carry the artist's records, which is what makes the
	// catalogue evidence non-discriminating.
	sharedCatalogue := []provider.ReleaseGroupInfo{
		{Title: "First Record"},
		{Title: "Second Record"},
	}

	// tier3Contested builds the fixture with a caller-chosen RIVAL catalogue.
	// Only the rival's catalogue varies between the two runs below; the local
	// albums, both names, both scores and the best candidate's catalogue are
	// held identical, so a divergence in outcome is attributable to that axis
	// alone.
	tier3Contested := func(t *testing.T, rivalCatalogue []provider.ReleaseGroupInfo) (*Router, *artist.Service) {
		t.Helper()
		return newIdentifyTestServer(t,
			func(_ context.Context, query string) ([]provider.ArtistSearchResult, error) {
				if query != "Gate Contested" {
					// The Tier 2 (folder-name) query fails, which is how an
					// artist WITH readable albums legitimately reaches Tier 3.
					return nil, errors.New("simulated provider failure on the tier 2 query")
				}
				return []provider.ArtistSearchResult{
					{
						Name:          "Gate Contested",
						MusicBrainzID: mbidGateRealArtist,
						Score:         100,
						Source:        string(provider.NameMusicBrainz),
					},
					{
						// 15 points back: clears the 10-point ambiguity margin,
						// so the name gates let the best candidate through.
						Name:          "Gate Contested",
						MusicBrainzID: mbidGateRival,
						Score:         85,
						Source:        string(provider.NameMusicBrainz),
					},
				}, nil
			},
			func(_ context.Context, mbid string) ([]provider.ReleaseGroupInfo, error) {
				if mbid == mbidGateRival {
					return rivalCatalogue, nil
				}
				return sharedCatalogue, nil
			},
		)
	}

	// PRECONDITION on the name gates. If the ambiguity margin were what refused
	// this fixture, the assertion below would prove nothing about the album
	// gate -- it would just be re-testing #2813.
	best := &provider.ArtistSearchResult{Name: "Gate Contested", MusicBrainzID: mbidGateRealArtist, Score: 100, Source: string(provider.NameMusicBrainz)}
	rival := &provider.ArtistSearchResult{Name: "Gate Contested", MusicBrainzID: mbidGateRival, Score: 85, Source: string(provider.NameMusicBrainz)}
	if rej := artist.EvaluateMBIDCandidate("Gate Contested", best, rival); rej != nil {
		t.Fatalf("precondition failed: the name gates already rejected this fixture (%s); "+
			"the album gate is then never consulted and the test is vacuous", rej.Reason)
	}

	r, artistSvc := tier3Contested(t, sharedCatalogue)
	a := seedBlankArtist(t, artistSvc, "Gate Contested", albumDir(t, "First Record", "Second Record"))

	got := r.identifyArtist(ctx, a, nil)
	if got.Outcome == outcomeAutoLinked {
		t.Errorf("Outcome = autoLinked, want anything else: two catalogues clearing the overlap floor do not discriminate between two identities")
	}
	// THE ROW is the assertion. The outcome counter can be wrong in either
	// direction independently of what landed in the database.
	if mbid := reloadMBID(t, artistSvc, a.ID); mbid != "" {
		t.Errorf("stored MBID = %q, want blank; the contested-catalogue branch did not stop the write", mbid)
	}

	// DISCRIMINATOR. Same two candidates, same scores, same best-candidate
	// catalogue -- only the rival's catalogue no longer overlaps, so nothing
	// contests. This must auto-link. Without it the decline above would pass
	// just as well against a Tier 3 that had been broken into refusing
	// everything, or against a hardcoded UncontestedBest: false.
	r2, artistSvc2 := tier3Contested(t, []provider.ReleaseGroupInfo{
		{Title: "Unrelated Record"},
		{Title: "Another Unrelated Record"},
	})
	b := seedBlankArtist(t, artistSvc2, "Gate Contested", albumDir(t, "First Record", "Second Record"))
	if got := r2.identifyArtist(ctx, b, nil); got.Outcome != outcomeAutoLinked {
		t.Fatalf("precondition failed: an UNCONTESTED rival = %v, want autoLinked; "+
			"the decline above proves nothing if this fixture never links", got.Outcome)
	}
	if mbid := reloadMBID(t, artistSvc2, b.ID); mbid != mbidGateRealArtist {
		t.Fatalf("precondition failed: uncontested candidate stored MBID = %q, want %q", mbid, mbidGateRealArtist)
	}
}

// TestIdentifyTier3TreatsAnUnfetchableRivalAsContesting pins the second of the
// two ways a rival contests, and it is the inversion-prone one.
//
// A rival whose catalogue could not be RETRIEVED is an UNMEASURED rival, not an
// absent one. Scoring it as absent would be the same could-not-look-so-nothing-
// objected inversion this whole issue exists to remove, just relocated to the
// rival side of the comparison. So the best candidate is treated as contested
// and the unattended write is withheld.
//
// Note what is deliberately NOT the cause of the decline here: the BEST
// candidate's own catalogue fetches fine and overlaps the local albums
// perfectly, so neither the zero-release guard nor the overlap floor is doing
// the work. Only the rival's failed fetch is.
func TestIdentifyTier3TreatsAnUnfetchableRivalAsContesting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	r, artistSvc := newIdentifyTestServer(t,
		func(_ context.Context, query string) ([]provider.ArtistSearchResult, error) {
			if query != "Gate Rival Fail" {
				return nil, errors.New("simulated provider failure on the tier 2 query")
			}
			return []provider.ArtistSearchResult{
				{Name: "Gate Rival Fail", MusicBrainzID: mbidGateRealArtist, Score: 100, Source: string(provider.NameMusicBrainz)},
				{Name: "Gate Rival Fail", MusicBrainzID: mbidGateRival, Score: 85, Source: string(provider.NameMusicBrainz)},
			}, nil
		},
		func(_ context.Context, mbid string) ([]provider.ReleaseGroupInfo, error) {
			if mbid == mbidGateRival {
				return nil, context.DeadlineExceeded
			}
			// The BEST candidate is a real entity whose catalogue matches the
			// local albums exactly: it clears every other leg of the gate.
			return []provider.ReleaseGroupInfo{{Title: "First Record"}, {Title: "Second Record"}}, nil
		},
	)

	a := seedBlankArtist(t, artistSvc, "Gate Rival Fail", albumDir(t, "First Record", "Second Record"))

	got := r.identifyArtist(ctx, a, nil)
	if got.Outcome == outcomeAutoLinked {
		t.Errorf("Outcome = autoLinked, want anything else: a rival catalogue that was never retrieved cannot be scored as an absent rival")
	}
	if mbid := reloadMBID(t, artistSvc, a.ID); mbid != "" {
		t.Errorf("stored MBID = %q, want blank; an unmeasured rival was treated as no rival", mbid)
	}
}

// TestIdentifyDeclinesWhenCandidateCatalogueCannotBeFetched pins the
// candidate-side twin of the Unknown rule on the live path: a release-group
// fetch that FAILED must not be read as "this candidate has no releases", and
// certainly not as "nothing objected".
//
// Routed through tier2SearchFailsRouter for the same reason as the
// zero-release test: it puts the album gate, rather than the 30% review floor,
// in the position of refusing.
func TestIdentifyDeclinesWhenCandidateCatalogueCannotBeFetched(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	r, artistSvc := tier2SearchFailsRouter(t, "Gate Fetch Fail",
		provider.ArtistSearchResult{
			Name:          "Gate Fetch Fail",
			MusicBrainzID: mbidGateRealArtist,
			Score:         100,
			Source:        string(provider.NameMusicBrainz),
		},
		func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
			return nil, context.DeadlineExceeded
		},
	)

	dir := albumDir(t, "First Record", "Second Record")
	a := seedBlankArtist(t, artistSvc, "Gate Fetch Fail", dir)

	got := r.identifyArtist(ctx, a, nil)
	if got.Outcome == outcomeAutoLinked {
		t.Errorf("Outcome = autoLinked, want anything else: a catalogue that was never retrieved cannot corroborate anything")
	}
	if mbid := reloadMBID(t, artistSvc, a.ID); mbid != "" {
		t.Errorf("stored MBID = %q, want blank", mbid)
	}
}

// TestIdentifyTier3AutoLinksOnCorroboratingCatalogue is the POSITIVE Tier 3
// case, and it is the test that could not exist before this PR.
//
// Every other Tier 3 test in this file asserts that nothing was written, and a
// pipeline broken into refusing everything would pass all of them. This one
// pins the other side: Tier 3 still auto-links when the catalogue genuinely
// corroborates. The gate withholds unattended writes, it does not stop them.
//
// WHY IT BELONGS HERE AND NOT IN THE TIER 2 PR. Reaching Tier 3 at all requires
// the artist to have no comparable local album set, or Tier 2 answers first --
// and until Tier 3 was gated, a fixture built that way could only assert that
// an artist nobody looked at gets auto-linked, which is the DEFECT stated as an
// expectation. The route in is instead a Tier 2 search FAILURE with readable
// albums on disk: the artist has a real catalogue, the Tier 2 query errors, and
// Tier 3 picks the candidate up and now judges it on album evidence.
func TestIdentifyTier3AutoLinksOnCorroboratingCatalogue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	r, artistSvc := tier2SearchFailsRouter(t, "Gate Tier3 Good",
		provider.ArtistSearchResult{
			Name:          "Gate Tier3 Good",
			MusicBrainzID: mbidGateRealArtist,
			Score:         100,
			Source:        string(provider.NameMusicBrainz),
		},
		func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
			return []provider.ReleaseGroupInfo{
				{Title: "First Record"},
				{Title: "Second Record"},
			}, nil
		},
	)

	a := seedBlankArtist(t, artistSvc, "Gate Tier3 Good", albumDir(t, "First Record", "Second Record"))

	got := r.identifyArtist(ctx, a, nil)
	if got.Outcome != outcomeAutoLinked {
		t.Fatalf("Outcome = %v, want autoLinked: a fully corroborated Tier 3 candidate must still be adopted", got.Outcome)
	}
	// THE ROW, matching every other test here: the counter and the database can
	// disagree, and only one of them is what the operator lives with.
	if mbid := reloadMBID(t, artistSvc, a.ID); mbid != mbidGateRealArtist {
		t.Errorf("stored MBID = %q, want %q", mbid, mbidGateRealArtist)
	}
}

// TestReleaseGroupCacheLogsFetchFailure proves the provider error survives
// (#2862).
//
// known=false was always the right ANSWER -- an unretrievable catalogue is not
// a determination -- but the error itself was discarded, so a broken adapter
// and a candidate with a genuinely empty catalogue were indistinguishable to an
// operator reading the logs. The MBID and the underlying cause are asserted,
// not merely that something was logged.
func TestReleaseGroupCacheLogsFetchFailure(t *testing.T) {
	t.Parallel()

	r, _ := testRouter(t)
	var buf bytes.Buffer
	r.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	installAudioDBOrchestrator(t, r, nil, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
		return nil, errors.New("musicbrainz refused the connection")
	})

	cache := r.newReleaseGroupCache()
	if cache == nil {
		t.Fatal("precondition: nil cache means no fetch is attempted, so nothing could be logged")
	}

	titles, known := cache.titles(context.Background(), mbidGateRealArtist)
	// The DECISION is unchanged and asserted alongside the log, so a future
	// change cannot satisfy the diagnostics by loosening the refusal.
	if known || titles != nil {
		t.Fatalf("titles = (%v, %v), want (nil, false): a failed fetch is not a determination", titles, known)
	}

	logged := buf.String()
	for _, want := range []string{"level=WARN", mbidGateRealArtist, "musicbrainz refused the connection"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log missing %q; got: %s", want, logged)
		}
	}
}

// Two more real UUIDs so the rival list can be longer than the budget. The
// budget test below needs THREE distinct rivals, and IsValidMBID is a strict
// shape check, so a readable placeholder would be skipped for the wrong reason.
const (
	mbidGateRival2 = "2c3d4e5f-6071-4b8c-9dae-1f2a3b4c5d6f"
	mbidGateRival3 = "3d4e5f60-7182-4c9d-8ebf-2a3b4c5d6e70"
)

// TestTier3RivalBudgetIsNotSpentOnCacheHits pins #2864.
//
// tier3RivalCatalogueBudget rations PROVIDER CALLS, not loop iterations. Two
// rivals whose catalogues are already cached -- the ordinary case, since Tier 2
// populates the same cache on its fall-through path -- cost nothing to consult.
// Charging them exhausted the budget after ZERO provider round-trips, so the
// third rival, the only genuinely unmeasured one, was never looked at and could
// never contest. That silently narrows contest detection, which is the thing
// the rival measurement exists to provide.
//
// The fixture is built so the THIRD rival is the only one that contests: the
// two cached rivals carry unrelated catalogues, so reaching them and finding
// nothing is not what the assertion turns on.
func TestTier3RivalBudgetIsNotSpentOnCacheHits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	localTitles := []string{"First Record", "Second Record"}
	var fetches []string
	r, _ := newIdentifyTestServer(t, nil,
		func(_ context.Context, mbid string) ([]provider.ReleaseGroupInfo, error) {
			fetches = append(fetches, mbid)
			if mbid == mbidGateRival3 {
				// The contesting catalogue: it matches the local albums, so it
				// clears artist.AlbumOverlapAutoLinkFloor.
				return []provider.ReleaseGroupInfo{{Title: "First Record"}, {Title: "Second Record"}}, nil
			}
			return []provider.ReleaseGroupInfo{{Title: "Unrelated Record"}}, nil
		},
	)

	cache := r.newReleaseGroupCache()
	if cache == nil {
		t.Fatal("precondition: a nil cache measures no rival at all, so the budget would never be reached")
	}

	// PRE-WARM the first two rivals, standing in for the Tier 2 fall-through
	// that already looked at them. These cost no further provider call.
	for _, mbid := range []string{mbidGateRival, mbidGateRival2} {
		if _, known := cache.titles(ctx, mbid); !known {
			t.Fatalf("precondition failed: pre-warming %s did not produce a known entry", mbid)
		}
	}
	// PRECONDITION 1: the two rivals really are cached, so consulting them is
	// free. Without this the test could pass on a fixture that never warmed.
	for _, mbid := range []string{mbidGateRival, mbidGateRival2} {
		if !cache.holds(mbid) {
			t.Fatalf("precondition failed: %s is not in the cache, so this test does not exercise a cache hit", mbid)
		}
	}
	// PRECONDITION 2: the contesting rival is NOT cached and sits beyond the
	// naive (iteration-counting) budget. If it were cached, or if there were
	// fewer preceding rivals than the budget, the buggy code would reach it too
	// and the test would pass vacuously.
	if cache.holds(mbidGateRival3) {
		t.Fatal("precondition failed: the contesting rival is already cached, so no provider call is at stake")
	}
	if precedingRivals := 2; precedingRivals < tier3RivalCatalogueBudget {
		t.Fatalf("precondition failed: %d cached rivals cannot exhaust a budget of %d", precedingRivals, tier3RivalCatalogueBudget)
	}
	warmFetches := len(fetches)

	local := artist.AlbumSet{Titles: localTitles, Evidence: artist.EvidenceFound, Origin: "test"}
	best := &provider.ArtistSearchResult{Name: "Gate Budget", MusicBrainzID: mbidGateRealArtist, Score: 100, Source: string(provider.NameMusicBrainz)}
	results := []provider.ArtistSearchResult{
		*best,
		{Name: "Gate Budget", MusicBrainzID: mbidGateRival, Score: 90, Source: string(provider.NameMusicBrainz)},
		{Name: "Gate Budget", MusicBrainzID: mbidGateRival2, Score: 85, Source: string(provider.NameMusicBrainz)},
		{Name: "Gate Budget", MusicBrainzID: mbidGateRival3, Score: 80, Source: string(provider.NameMusicBrainz)},
	}

	if !r.tier3RivalClearsOverlapFloor(ctx, local, results, best, cache) {
		t.Errorf("tier3RivalClearsOverlapFloor = false, want true: two cached rivals consumed a budget that rations provider calls, "+
			"so the only unmeasured rival (%s) was never consulted", mbidGateRival3)
	}
	// The measurement, not just the verdict: exactly one NEW provider call was
	// made, and it was for the third rival.
	newFetches := fetches[warmFetches:]
	if len(newFetches) != 1 || newFetches[0] != mbidGateRival3 {
		t.Errorf("new provider fetches = %v, want exactly [%s]", newFetches, mbidGateRival3)
	}
}

// TestReleaseGroupCacheHoldsIsNilSafe pins the reason holds exists as a METHOD
// rather than a raw cache.entries read at the call site.
//
// newReleaseGroupCache returns nil on three paths -- no provider registry, no
// registered MusicBrainz provider, and a provider not implementing
// ReleaseGroupFetcher -- and tier3AlbumGateDeclines is nil-safe today only
// because titles guards its own receiver. A map read from outside would panic
// the identify run for every operator who has not configured MusicBrainz: a
// crash introduced by a fix for a non-crashing efficiency bug.
func TestReleaseGroupCacheHoldsIsNilSafe(t *testing.T) {
	t.Parallel()

	var nilCache *releaseGroupCache
	if nilCache.holds(mbidGateRealArtist) {
		t.Error("nil cache holds() = true, want false")
	}

	// The other half: on a live cache holds must actually DISCRIMINATE, or the
	// nil case above would be satisfied by a method that always returns false
	// and the budget fix would be silently inert.
	r, _ := testRouter(t)
	installAudioDBOrchestrator(t, r, nil, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
		return []provider.ReleaseGroupInfo{{Title: "First Record"}}, nil
	})
	cache := r.newReleaseGroupCache()
	if cache == nil {
		t.Fatal("precondition: a nil cache here would make the discrimination check vacuous")
	}
	if cache.holds(mbidGateRealArtist) {
		t.Error("holds() = true before any lookup, want false")
	}
	if _, known := cache.titles(context.Background(), mbidGateRealArtist); !known {
		t.Fatal("precondition failed: the seeding lookup was not a determination")
	}
	if !cache.holds(mbidGateRealArtist) {
		t.Error("holds() = false after a successful lookup, want true")
	}
}
