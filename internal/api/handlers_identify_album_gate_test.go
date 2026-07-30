package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// MBIDs for the album-gate fixtures. Real UUIDs, because artist.IsValidMBID is
// a strict shape check and a placeholder would be refused for the WRONG reason,
// making a "no write happened" assertion prove nothing about the album gate.
const (
	mbidGateEmptyStub  = "3f2a1c88-4d5e-4a6b-9c7d-0e1f2a3b4c5d"
	mbidGateRealArtist = "7a8b9c0d-1e2f-4a3b-8c4d-5e6f7a8b9c0d"
	mbidGateTribute    = "9e8d7c6b-5a4f-4e3d-8c2b-1a0f9e8d7c6b"
	mbidGateRival      = "1b2c3d4e-5f60-4a7b-8c9d-0e1f2a3b4c5e"
)

// seedBlankArtist creates an artist with NO MusicBrainz ID and asserts that
// precondition against the store.
//
// The precondition assert is not ceremony. Every test in this file ends by
// checking that the artist's MBID is still blank, and against an artist that
// never had one seeded, that check passes whether or not the gate did anything
// at all. Asserting "blank BEFORE" is what turns "blank after" into evidence.
func seedBlankArtist(t *testing.T, svc *artist.Service, name, path string) *artist.Artist {
	t.Helper()
	ctx := context.Background()
	a := &artist.Artist{Name: name, SortName: name, Type: "group", Path: path}
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if mbid := reloadMBID(t, svc, a.ID); mbid != "" {
		t.Fatalf("precondition failed: seeded MBID = %q, want blank", mbid)
	}
	return a
}

// albumDir builds an on-disk artist folder holding the named album
// subdirectories, which is what FilesystemAlbumSource reads.
//
// Fixture names are neutral by rule: real library artist and album titles are
// private metadata and never land in the repository. The SHAPE is what matters
// to these tests (a matching name, N local albums, a candidate catalogue of a
// given size), not the strings themselves.
func albumDir(t *testing.T, albums ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, alb := range albums {
		if err := os.Mkdir(filepath.Join(dir, alb), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", alb, err)
		}
	}
	return dir
}

// TestIdentifyDeclinesTributeActWithRealCatalogue covers the case the album
// comparison alone CANNOT catch.
//
// A tribute act releases covers of the same albums, so its catalogue overlaps
// the local library heavily -- here perfectly. Every gate that reads names or
// album overlap passes it. Only the disambiguation red flag, a secondary
// signal, withholds the unattended write.
//
// The candidate still reaches the operator: the assertion is on the artist ROW,
// not on the candidate being hidden.
func TestIdentifyDeclinesTributeActWithRealCatalogue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	r, artistSvc := newIdentifyTestServer(t,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			return []provider.ArtistSearchResult{{
				Name:           "Gate Tribute",
				MusicBrainzID:  mbidGateTribute,
				Score:          100,
				Disambiguation: "tribute band",
				Source:         string(provider.NameMusicBrainz),
			}}, nil
		},
		func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
			// A REAL catalogue that matches the local albums exactly, which is
			// what makes this case invisible to the album comparison.
			return []provider.ReleaseGroupInfo{
				{Title: "First Record"},
				{Title: "Second Record"},
			}, nil
		},
	)

	dir := albumDir(t, "First Record", "Second Record")
	a := seedBlankArtist(t, artistSvc, "Gate Tribute", dir)

	got := r.identifyArtist(ctx, a, nil)
	if got.Outcome == outcomeAutoLinked {
		t.Errorf("Outcome = autoLinked, want anything else: a flagged act must not be adopted unattended even at 100%% catalogue overlap")
	}
	if mbid := reloadMBID(t, artistSvc, a.ID); mbid != "" {
		t.Errorf("stored MBID = %q, want blank", mbid)
	}

	// Discriminator: the identical candidate WITHOUT the disambiguation string
	// must auto-link. Without this half the test above would pass against a
	// pipeline that had simply stopped linking anything.
	r2, artistSvc2 := newIdentifyTestServer(t,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			return []provider.ArtistSearchResult{{
				Name:          "Gate Tribute",
				MusicBrainzID: mbidGateRealArtist,
				Score:         100,
				Source:        string(provider.NameMusicBrainz),
			}}, nil
		},
		func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
			return []provider.ReleaseGroupInfo{
				{Title: "First Record"},
				{Title: "Second Record"},
			}, nil
		},
	)
	b := seedBlankArtist(t, artistSvc2, "Gate Tribute", albumDir(t, "First Record", "Second Record"))
	if got := r2.identifyArtist(ctx, b, nil); got.Outcome != outcomeAutoLinked {
		t.Fatalf("precondition failed: the same candidate without the tribute flag = %v, want autoLinked; "+
			"the decline above proves nothing if this fixture never links", got.Outcome)
	}
	if mbid := reloadMBID(t, artistSvc2, b.ID); mbid != mbidGateRealArtist {
		t.Fatalf("precondition failed: unflagged candidate stored MBID = %q, want %q", mbid, mbidGateRealArtist)
	}
}

// TestIdentifyGateDecisionIsIndependentOfAlbumSetOrigin is the SOURCE-UNIFORMITY
// guard on the live path.
//
// AlbumSet.Origin is diagnostics only and no confidence decision may branch on
// it. Here the SAME album titles reach the gate under two different Origins --
// the real filesystem source, and a synthetic set standing in for a future peer
// source -- and both must produce the same gate decision.
//
// The fixtures differ along the axis being tested (Origin) and are identical
// along every other one, which is what makes a divergence attributable.
func TestIdentifyGateDecisionIsIndependentOfAlbumSetOrigin(t *testing.T) {
	t.Parallel()

	titles := []string{"First Record", "Second Record"}
	remote := []provider.ReleaseGroupInfo{{Title: "First Record"}, {Title: "Second Record"}}

	scoreThrough := func(t *testing.T, origin string) artist.AlbumGateDecision {
		t.Helper()
		r, _ := newIdentifyTestServer(t, nil,
			func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
				return remote, nil
			},
		)
		results := []provider.ArtistSearchResult{{
			Name:          "Gate Origin",
			MusicBrainzID: mbidGateRealArtist,
			Score:         100,
			Source:        string(provider.NameMusicBrainz),
		}}
		local := artist.AlbumSet{Titles: titles, Evidence: artist.EvidenceFound, Origin: origin}
		scored := r.enrichAndScoreTier2(context.Background(), results, local, r.newReleaseGroupCache())
		if len(scored) != 1 {
			t.Fatalf("len(scored) = %d, want 1", len(scored))
		}
		if !r.tier2GatePermits(&artist.Artist{Name: "Gate Origin"}, local, &scored[0]) {
			return artist.AlbumGateDecline
		}
		return artist.AlbumGatePermit
	}

	first := scoreThrough(t, "filesystem")
	if first != artist.AlbumGatePermit {
		t.Fatalf("precondition failed: a full-overlap set = %v, want permit; "+
			"a uniformity check over two declines proves nothing", first)
	}
	if second := scoreThrough(t, "peer:emby"); second != first {
		t.Errorf("Origin peer:emby produced %v, want %v; Origin is diagnostics only and must never move a decision",
			second, first)
	}
}

// TestIdentifyStillAutoLinksOnGoodEvidence is the anti-vacuity backstop for
// this whole file.
//
// Every other test here asserts that NOTHING was written, and a pipeline that
// had been broken into refusing everything would pass all of them. This one
// pins the positive case: readable local albums, a candidate with a real
// overlapping catalogue, no red flag -- the artist's row DOES get the ID.
func TestIdentifyStillAutoLinksOnGoodEvidence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	r, artistSvc := newIdentifyTestServer(t,
		func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			return []provider.ArtistSearchResult{{
				Name:          "Gate Good",
				MusicBrainzID: mbidGateRealArtist,
				Score:         100,
				Source:        string(provider.NameMusicBrainz),
			}}, nil
		},
		func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
			return []provider.ReleaseGroupInfo{
				{Title: "First Record"},
				{Title: "Second Record"},
			}, nil
		},
	)

	dir := albumDir(t, "First Record", "Second Record")
	a := seedBlankArtist(t, artistSvc, "Gate Good", dir)

	if got := r.identifyArtist(ctx, a, nil); got.Outcome != outcomeAutoLinked {
		t.Fatalf("Outcome = %v, want autoLinked on corroborating album evidence", got.Outcome)
	}
	if mbid := reloadMBID(t, artistSvc, a.ID); mbid != mbidGateRealArtist {
		t.Errorf("stored MBID = %q, want %q", mbid, mbidGateRealArtist)
	}
}

// TestIdentifyTier1IsDeliberatelyNotAlbumGated pins the SCOPE of #2828 rather
// than a guard, and it is here so the boundary is enforced instead of only
// asserted in a doc comment.
//
// Tier 1 (connection-library fill) does NOT apply the album-evidence gate. The
// fixture is the exact shape the gate would refuse if it ran: an artist with no
// Path, so the local album set is EvidenceUnknown -- the state EvaluateAlbumGate
// declines outright, and the 43% majority of a production library. Tier 1 fills
// the MBID from the platform-supplied ID anyway.
//
// This is a deliberate scope decision, not an omission (see the WHICH WRITE
// PATHS APPLY IT block in internal/artist/albumgate.go): Tier 1 makes no
// provider call, so gating it would require a configured MusicBrainz provider
// before a connection-only install could auto-link anything, and it would
// refuse most of what Tier 1 exists to serve.
//
// WHAT THIS TEST IS FOR. If a later change hoists the localAlbumSet resolution
// above evaluateTier1 and starts gating Tier 1, this test goes RED. That is the
// intended signal: such a change must be a deliberate, tested behavior change
// with this test updated alongside it, never a silent side effect of tidying
// the ordering.
func TestIdentifyTier1IsDeliberatelyNotAlbumGated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// No search or release-group function: reaching either would mean Tier 1
	// did not settle the artist, which the outcome assertion below catches.
	r, artistSvc := newIdentifyTestServer(t, nil, nil)

	// No Path: FilesystemAlbumSource reports EvidenceUnknown, the state the
	// album gate declines outright. If Tier 1 were gated, this would NOT link.
	a := seedBlankArtist(t, artistSvc, "Gate Tier One", "")

	// PRECONDITION on the evidence state. Without it, a fixture that silently
	// became EvidenceFound would make the test pass for the wrong reason -- it
	// would then be pinning "a gated Tier 1 permits good evidence" rather than
	// "Tier 1 is not gated at all".
	if local := r.localAlbumSet(ctx, a); local.Evidence != artist.EvidenceUnknown {
		t.Fatalf("precondition failed: album evidence = %v, want EvidenceUnknown; "+
			"this test only means anything against the state the gate refuses", local.Evidence)
	}

	idx := &connectionIndex{byName: map[string][]connEntry{
		"gate tier one": {{Name: "Gate Tier One", MusicBrainzID: mbidGateRealArtist}},
	}}

	got := r.identifyArtist(ctx, a, idx)
	if got.Outcome != outcomeAutoLinked {
		t.Fatalf("Outcome = %v, want autoLinked: Tier 1 is not album-gated. "+
			"If this changed deliberately, update this test and the scope note in internal/artist/albumgate.go", got.Outcome)
	}
	// THE ROW, matching every other test in this file: the counter and the
	// database can disagree.
	if mbid := reloadMBID(t, artistSvc, a.ID); mbid != mbidGateRealArtist {
		t.Errorf("stored MBID = %q, want %q", mbid, mbidGateRealArtist)
	}
}
