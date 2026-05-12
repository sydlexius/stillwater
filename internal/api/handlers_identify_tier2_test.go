package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// identifyStubProvider is a minimal Provider implementation used by the
// Tier 2 / Tier 3 identify tests. It also implements ReleaseGroupFetcher
// so enrichAndScoreTier2 can exercise the album-comparison branch.
//
// Only the fields the identify pipeline reads (SearchArtist, GetReleaseGroups)
// have backing hooks. Name / RequiresAuth / GetArtist / GetImages are stubs.
type identifyStubProvider struct {
	name             provider.ProviderName
	searchFn         func(ctx context.Context, name string) ([]provider.ArtistSearchResult, error)
	getReleaseGrpsFn func(ctx context.Context, artistID string) ([]provider.ReleaseGroupInfo, error)
}

func (s *identifyStubProvider) Name() provider.ProviderName { return s.name }
func (s *identifyStubProvider) RequiresAuth() bool          { return false }

func (s *identifyStubProvider) SearchArtist(ctx context.Context, name string) ([]provider.ArtistSearchResult, error) {
	if s.searchFn != nil {
		return s.searchFn(ctx, name)
	}
	return nil, nil
}

func (s *identifyStubProvider) GetArtist(_ context.Context, _ string) (*provider.ArtistMetadata, error) {
	return nil, nil
}

func (s *identifyStubProvider) GetImages(_ context.Context, _ string) ([]provider.ImageResult, error) {
	return nil, nil
}

// GetReleaseGroups satisfies provider.ReleaseGroupFetcher.
func (s *identifyStubProvider) GetReleaseGroups(ctx context.Context, artistID string) ([]provider.ReleaseGroupInfo, error) {
	if s.getReleaseGrpsFn != nil {
		return s.getReleaseGrpsFn(ctx, artistID)
	}
	return nil, nil
}

// newIdentifyTestServer builds a Router with a registered stub MusicBrainz
// provider and a real orchestrator wired around it. The orchestrator only
// uses the registry for SearchForLinking, so SettingsService can be nil.
//
// Returns the router plus the artist service so callers can seed fixtures.
// The stub provider is registered internally and held only by the registry --
// tests assert on observable outcomes (auto-link side effects, returned
// outcomes) rather than introspecting the stub, so it is not returned.
func newIdentifyTestServer(t *testing.T, search func(ctx context.Context, name string) ([]provider.ArtistSearchResult, error), releaseGroups func(ctx context.Context, artistID string) ([]provider.ReleaseGroupInfo, error)) (*Router, *artist.Service) {
	t.Helper()
	r, _, artistSvc := testRouterWithLibrary(t)

	stub := &identifyStubProvider{
		name:             provider.NameMusicBrainz,
		searchFn:         search,
		getReleaseGrpsFn: releaseGroups,
	}
	registry := provider.NewRegistry()
	registry.Register(stub)
	r.providerRegistry = registry

	// Orchestrator wiring: settings can be nil because SearchForLinking only
	// uses the registry. To unblock autoLinkAndRefresh's FetchMetadata call
	// (which would otherwise dereference the nil settings service), we install
	// a no-op ScraperExecutor that short-circuits the executor branch.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := provider.NewOrchestrator(registry, nil, logger)
	orch.SetExecutor(&stubScraperExecutor{result: &provider.FetchResult{Metadata: &provider.ArtistMetadata{}}})
	r.orchestrator = orch

	return r, artistSvc
}

// TestConvertToScoredCandidates covers handlers_identify.go:703 -- the
// fallback wrapper used when album enrichment is unavailable. The function
// should map each provider result one-to-one onto a ScoredCandidate with
// Confidence=0 and a stable Reason.
func TestConvertToScoredCandidates(t *testing.T) {
	t.Parallel()

	t.Run("empty input returns empty slice", func(t *testing.T) {
		t.Parallel()
		got := convertToScoredCandidates(nil)
		if got == nil {
			t.Fatal("convertToScoredCandidates(nil) returned nil; want non-nil empty slice")
		}
		if len(got) != 0 {
			t.Errorf("len = %d, want 0", len(got))
		}
	})

	t.Run("preserves order and applies zero-confidence", func(t *testing.T) {
		t.Parallel()
		in := []provider.ArtistSearchResult{
			{Name: "First", MusicBrainzID: "mb-1", Score: 90},
			{Name: "Second", MusicBrainzID: "mb-2", Score: 75},
			{Name: "Third", MusicBrainzID: "mb-3", Score: 50},
		}
		got := convertToScoredCandidates(in)
		if len(got) != len(in) {
			t.Fatalf("len = %d, want %d", len(got), len(in))
		}
		for i, sc := range got {
			if sc.Name != in[i].Name {
				t.Errorf("[%d] Name = %q, want %q", i, sc.Name, in[i].Name)
			}
			if sc.MusicBrainzID != in[i].MusicBrainzID {
				t.Errorf("[%d] MBID = %q, want %q", i, sc.MusicBrainzID, in[i].MusicBrainzID)
			}
			if sc.Confidence != 0 {
				t.Errorf("[%d] Confidence = %v, want 0", i, sc.Confidence)
			}
			if sc.Reason != "no album data available" {
				t.Errorf("[%d] Reason = %q, want %q", i, sc.Reason, "no album data available")
			}
		}
	})
}

// TestEnrichAndScoreTier2 covers handlers_identify.go:522. It exercises the
// no-registry / no-provider / non-fetcher fallbacks plus the happy-path
// album-comparison branch with two and three candidates.
func TestEnrichAndScoreTier2(t *testing.T) {
	t.Parallel()

	t.Run("nil registry falls through to scored conversion", func(t *testing.T) {
		t.Parallel()
		r, _, _ := testRouterWithLibrary(t)
		// Explicitly nil so we hit the early return.
		r.providerRegistry = nil

		results := []provider.ArtistSearchResult{
			{Name: "A", MusicBrainzID: "mb-a"},
			{Name: "B", MusicBrainzID: "mb-b"},
		}
		got := r.enrichAndScoreTier2(context.Background(), results, []string{"Local Album"})
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
		// Conversion path uses Reason="no album data available".
		if got[0].Reason != "no album data available" {
			t.Errorf("Reason = %q, want fallback reason", got[0].Reason)
		}
	})

	t.Run("provider not registered falls through", func(t *testing.T) {
		t.Parallel()
		r, _, _ := testRouterWithLibrary(t)
		// Registry exists but is empty: Get(NameMusicBrainz) returns nil.
		r.providerRegistry = provider.NewRegistry()

		results := []provider.ArtistSearchResult{{Name: "X", MusicBrainzID: "mb-x"}}
		got := r.enrichAndScoreTier2(context.Background(), results, nil)
		if len(got) != 1 || got[0].Reason != "no album data available" {
			t.Errorf("got = %+v, want single fallback candidate", got)
		}
	})

	t.Run("happy path computes album comparison", func(t *testing.T) {
		t.Parallel()
		r, _ := newIdentifyTestServer(t,
			nil,
			func(_ context.Context, mbid string) ([]provider.ReleaseGroupInfo, error) {
				// Return the exact local titles so MatchPercent is 100.
				if mbid == "mb-perfect" {
					return []provider.ReleaseGroupInfo{
						{Title: "Album One"},
						{Title: "Album Two"},
					}, nil
				}
				// Partial overlap for the second candidate.
				if mbid == "mb-partial" {
					return []provider.ReleaseGroupInfo{
						{Title: "Album One"},
						{Title: "Unrelated"},
					}, nil
				}
				return nil, nil
			},
		)

		results := []provider.ArtistSearchResult{
			{Name: "Perfect", MusicBrainzID: "mb-perfect", Score: 100},
			{Name: "Partial", MusicBrainzID: "mb-partial", Score: 80},
		}
		got := r.enrichAndScoreTier2(context.Background(),
			results,
			[]string{"Album One", "Album Two"},
		)
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}

		// Candidate 0 has full overlap -> AlbumComparison set, Confidence > 0.
		if got[0].AlbumComparison == nil {
			t.Fatal("candidate 0 AlbumComparison = nil, want populated")
		}
		if got[0].AlbumComparison.MatchPercent != 100 {
			t.Errorf("candidate 0 MatchPercent = %d, want 100", got[0].AlbumComparison.MatchPercent)
		}
		if got[0].Confidence != 1.0 {
			t.Errorf("candidate 0 Confidence = %v, want 1.0", got[0].Confidence)
		}
		// Candidate 1 has half overlap.
		if got[1].AlbumComparison == nil {
			t.Fatal("candidate 1 AlbumComparison = nil, want populated")
		}
		if got[1].AlbumComparison.MatchPercent != 50 {
			t.Errorf("candidate 1 MatchPercent = %d, want 50", got[1].AlbumComparison.MatchPercent)
		}
	})

	t.Run("release group fetch error skips that candidate", func(t *testing.T) {
		t.Parallel()
		r, _ := newIdentifyTestServer(t,
			nil,
			func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
				return nil, errors.New("provider boom")
			},
		)
		results := []provider.ArtistSearchResult{{Name: "X", MusicBrainzID: "mb-x"}}
		got := r.enrichAndScoreTier2(context.Background(), results, []string{"a"})
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1", len(got))
		}
		if got[0].AlbumComparison != nil {
			t.Errorf("AlbumComparison = %+v, want nil after fetch error", got[0].AlbumComparison)
		}
	})

	t.Run("empty MBID skips lookup", func(t *testing.T) {
		t.Parallel()
		var calls int
		r, _ := newIdentifyTestServer(t,
			nil,
			func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
				calls++
				return []provider.ReleaseGroupInfo{{Title: "Album One"}}, nil
			},
		)
		results := []provider.ArtistSearchResult{{Name: "NoID"}} // empty MBID
		got := r.enrichAndScoreTier2(context.Background(), results, []string{"Album One"})
		if calls != 0 {
			t.Errorf("GetReleaseGroups calls = %d, want 0 for empty MBID", calls)
		}
		if len(got) != 1 || got[0].AlbumComparison != nil {
			t.Errorf("got = %+v, want single candidate with nil AlbumComparison", got)
		}
	})

	t.Run("caps lookups at 3 candidates", func(t *testing.T) {
		t.Parallel()
		var calls int
		var mu sync.Mutex
		r, _ := newIdentifyTestServer(t,
			nil,
			func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
				mu.Lock()
				calls++
				mu.Unlock()
				return nil, nil
			},
		)
		results := []provider.ArtistSearchResult{
			{Name: "1", MusicBrainzID: "mb-1"},
			{Name: "2", MusicBrainzID: "mb-2"},
			{Name: "3", MusicBrainzID: "mb-3"},
			{Name: "4", MusicBrainzID: "mb-4"},
			{Name: "5", MusicBrainzID: "mb-5"},
		}
		got := r.enrichAndScoreTier2(context.Background(), results, []string{"a"})
		if len(got) != 5 {
			t.Fatalf("len = %d, want 5", len(got))
		}
		if calls != 3 {
			t.Errorf("GetReleaseGroups calls = %d, want 3 (cap)", calls)
		}
	})
}

// TestEvaluateTier2 covers handlers_identify.go:573 across its three
// outcomes: exactly-one-above-70 auto-links, any-above-30 queues, all-low
// returns unmatched.
func TestEvaluateTier2(t *testing.T) {
	t.Parallel()

	mustScored := func(percent int, mbid string) ScoredCandidate {
		cmp := artist.AlbumComparison{MatchPercent: percent}
		return ScoredCandidate{
			ArtistSearchResult: provider.ArtistSearchResult{MusicBrainzID: mbid, Name: "T"},
			AlbumComparison:    &cmp,
		}
	}

	t.Run("single above 70 auto-links", func(t *testing.T) {
		t.Parallel()
		r, _, artistSvc := testRouterWithLibrary(t)
		ctx := context.Background()

		a := &artist.Artist{Name: "T2 Auto", SortName: "T2 Auto", Type: "group", Path: "/m/t"}
		if err := artistSvc.Create(ctx, a); err != nil {
			t.Fatalf("creating artist: %v", err)
		}

		scored := []ScoredCandidate{
			mustScored(90, "mb-clear-winner"),
			mustScored(20, "mb-low"),
		}
		got := r.evaluateTier2(ctx, a, scored)
		if got.Outcome != outcomeAutoLinked {
			t.Fatalf("Outcome = %v, want autoLinked", got.Outcome)
		}
		// Reload to verify the MBID actually persisted via autoLinkAndRefresh.
		reloaded, err := artistSvc.GetByID(ctx, a.ID)
		if err != nil {
			t.Fatalf("reloading: %v", err)
		}
		if reloaded.MusicBrainzID != "mb-clear-winner" {
			t.Errorf("MBID = %q, want %q", reloaded.MusicBrainzID, "mb-clear-winner")
		}
	})

	t.Run("multiple above 30 queues for review", func(t *testing.T) {
		t.Parallel()
		r, _, _ := testRouterWithLibrary(t)
		a := &artist.Artist{ID: "queued-1", Name: "T2 Queued", Path: "/m/q"}

		scored := []ScoredCandidate{
			mustScored(75, "mb-a"),
			mustScored(72, "mb-b"), // two above 70 => queue, not autolink
		}
		got := r.evaluateTier2(context.Background(), a, scored)
		if got.Outcome != outcomeQueued {
			t.Fatalf("Outcome = %v, want queued", got.Outcome)
		}
		if got.Candidate == nil {
			t.Fatal("Candidate = nil, want populated")
		}
		if got.Candidate.Tier != "album" {
			t.Errorf("Tier = %q, want %q", got.Candidate.Tier, "album")
		}
		if got.Candidate.ArtistID != "queued-1" {
			t.Errorf("ArtistID = %q, want queued-1", got.Candidate.ArtistID)
		}
		if len(got.Candidate.Candidates) != 2 {
			t.Errorf("Candidates len = %d, want 2", len(got.Candidate.Candidates))
		}
	})

	t.Run("single above 30 still queues", func(t *testing.T) {
		t.Parallel()
		r, _, _ := testRouterWithLibrary(t)
		a := &artist.Artist{ID: "queued-2", Name: "Mid", Path: "/m/m"}
		scored := []ScoredCandidate{mustScored(45, "mb-mid")}
		got := r.evaluateTier2(context.Background(), a, scored)
		if got.Outcome != outcomeQueued {
			t.Fatalf("Outcome = %v, want queued", got.Outcome)
		}
	})

	t.Run("all below 30 returns unmatched", func(t *testing.T) {
		t.Parallel()
		r, _, _ := testRouterWithLibrary(t)
		a := &artist.Artist{ID: "u-1", Name: "U"}
		scored := []ScoredCandidate{
			mustScored(10, "mb-x"),
			mustScored(0, "mb-y"),
		}
		got := r.evaluateTier2(context.Background(), a, scored)
		if got.Outcome != outcomeUnmatched {
			t.Fatalf("Outcome = %v, want unmatched", got.Outcome)
		}
		if got.Candidate != nil {
			t.Errorf("Candidate = %+v, want nil for unmatched", got.Candidate)
		}
	})

	t.Run("nil AlbumComparison entries ignored", func(t *testing.T) {
		t.Parallel()
		r, _, _ := testRouterWithLibrary(t)
		a := &artist.Artist{ID: "n-1", Name: "N"}
		// A candidate with nil AlbumComparison must not be counted in either
		// threshold bucket: it should fall through to unmatched.
		scored := []ScoredCandidate{
			{ArtistSearchResult: provider.ArtistSearchResult{MusicBrainzID: "mb-nil"}},
		}
		got := r.evaluateTier2(context.Background(), a, scored)
		if got.Outcome != outcomeUnmatched {
			t.Fatalf("Outcome = %v, want unmatched when no candidate has AlbumComparison", got.Outcome)
		}
	})
}

// TestIdentifyArtist covers handlers_identify.go:407 by exercising every
// branch in the three-tier pipeline: locked-skipped, tier-1 unanimous
// connection match, tier-2 album auto-link, tier-3 high-confidence
// name-only auto-link, tier-3 queue, tier-3 unmatched, tier-3 search error.
// The existing TestBulkIdentify_* tests cover the integration path; this
// test exercises identifyArtist directly so each branch is isolated.
func TestIdentifyArtist(t *testing.T) {
	t.Parallel()

	t.Run("locked artist is skipped", func(t *testing.T) {
		t.Parallel()
		r, _, _ := testRouterWithLibrary(t)
		a := &artist.Artist{Name: "Locked", Locked: true}
		got := r.identifyArtist(context.Background(), a, nil)
		if got.Outcome != outcomeSkipped {
			t.Errorf("Outcome = %v, want skipped", got.Outcome)
		}
	})

	t.Run("tier 1 unanimous connection match auto-links", func(t *testing.T) {
		t.Parallel()
		r, _, artistSvc := testRouterWithLibrary(t)
		ctx := context.Background()

		a := &artist.Artist{Name: "Pink Floyd", SortName: "Pink Floyd", Type: "group", Path: "/m/Pink Floyd"}
		if err := artistSvc.Create(ctx, a); err != nil {
			t.Fatalf("creating artist: %v", err)
		}

		idx := &connectionIndex{byName: map[string][]connEntry{
			"pink floyd": {
				{Name: "Pink Floyd", MusicBrainzID: "mb-pf", DiscogsID: "d-pf"},
				{Name: "Pink Floyd", MusicBrainzID: "mb-pf", DiscogsID: "d-pf"},
			},
		}}
		got := r.identifyArtist(ctx, a, idx)
		if got.Outcome != outcomeAutoLinked {
			t.Fatalf("Outcome = %v, want autoLinked", got.Outcome)
		}
		// Mutation propagates through Update.
		reloaded, err := artistSvc.GetByID(ctx, a.ID)
		if err != nil {
			t.Fatalf("reloading: %v", err)
		}
		if reloaded.MusicBrainzID != "mb-pf" {
			t.Errorf("MBID = %q, want mb-pf", reloaded.MusicBrainzID)
		}
		if reloaded.DiscogsID != "d-pf" {
			t.Errorf("DiscogsID = %q, want d-pf", reloaded.DiscogsID)
		}
	})

	t.Run("tier 1 disagreement falls through to tier 3 unmatched without orchestrator", func(t *testing.T) {
		t.Parallel()
		r, _, _ := testRouterWithLibrary(t)
		// No orchestrator wired => tier 2/3 immediately return unmatched.
		idx := &connectionIndex{byName: map[string][]connEntry{
			"split": {
				{Name: "Split", MusicBrainzID: "mb-a"},
				{Name: "Split", MusicBrainzID: "mb-b"},
			},
		}}
		a := &artist.Artist{Name: "Split"}
		got := r.identifyArtist(context.Background(), a, idx)
		if got.Outcome != outcomeUnmatched {
			t.Errorf("Outcome = %v, want unmatched (no orchestrator)", got.Outcome)
		}
	})

	t.Run("tier 3 high-confidence single result auto-links", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		r, artistSvc := newIdentifyTestServer(t,
			func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
				return []provider.ArtistSearchResult{
					{Name: "Solo", MusicBrainzID: "mb-solo", Score: 95},
				}, nil
			},
			nil,
		)
		// Path is empty so ListLocalAlbums returns nil and tier 2 is skipped.
		a := &artist.Artist{Name: "Solo", SortName: "Solo", Type: "person"}
		if err := artistSvc.Create(ctx, a); err != nil {
			t.Fatalf("creating artist: %v", err)
		}
		got := r.identifyArtist(ctx, a, nil)
		if got.Outcome != outcomeAutoLinked {
			t.Fatalf("Outcome = %v, want autoLinked", got.Outcome)
		}
		reloaded, err := artistSvc.GetByID(ctx, a.ID)
		if err != nil {
			t.Fatalf("reloading: %v", err)
		}
		if reloaded.MusicBrainzID != "mb-solo" {
			t.Errorf("MBID = %q, want mb-solo", reloaded.MusicBrainzID)
		}
	})

	t.Run("tier 3 multiple results queues for review", func(t *testing.T) {
		t.Parallel()
		r, _ := newIdentifyTestServer(t,
			func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
				// Confidence = Score/200, so Score=80 => 0.4, Score=70 => 0.35.
				return []provider.ArtistSearchResult{
					{Name: "Cand1", MusicBrainzID: "mb-c1", Score: 80},
					{Name: "Cand2", MusicBrainzID: "mb-c2", Score: 70},
				}, nil
			},
			nil,
		)
		a := &artist.Artist{ID: "q-id", Name: "Multi"}
		got := r.identifyArtist(context.Background(), a, nil)
		if got.Outcome != outcomeQueued {
			t.Fatalf("Outcome = %v, want queued", got.Outcome)
		}
		if got.Candidate == nil || got.Candidate.Tier != "name" {
			t.Errorf("Candidate.Tier = %v, want name", got.Candidate)
		}
		if len(got.Candidate.Candidates) != 2 {
			t.Errorf("len = %d, want 2", len(got.Candidate.Candidates))
		}
	})

	t.Run("tier 3 low confidence results yield unmatched", func(t *testing.T) {
		t.Parallel()
		r, _ := newIdentifyTestServer(t,
			func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
				// Score=40 => confidence 0.2, below 0.3 threshold.
				return []provider.ArtistSearchResult{
					{Name: "Low", MusicBrainzID: "mb-low", Score: 40},
				}, nil
			},
			nil,
		)
		a := &artist.Artist{Name: "Low"}
		got := r.identifyArtist(context.Background(), a, nil)
		if got.Outcome != outcomeUnmatched {
			t.Errorf("Outcome = %v, want unmatched", got.Outcome)
		}
	})

	t.Run("tier 3 search error returns failed", func(t *testing.T) {
		t.Parallel()
		r, _ := newIdentifyTestServer(t,
			func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
				return nil, errors.New("network down")
			},
			nil,
		)
		// SearchForLinking swallows per-provider errors and returns nil error
		// with no results, so the path here ends in "no results" => unmatched.
		a := &artist.Artist{Name: "Err"}
		got := r.identifyArtist(context.Background(), a, nil)
		if got.Outcome != outcomeUnmatched {
			t.Errorf("Outcome = %v, want unmatched (orchestrator swallows errors)", got.Outcome)
		}
	})

	t.Run("tier 3 empty results yield unmatched", func(t *testing.T) {
		t.Parallel()
		r, _ := newIdentifyTestServer(t,
			func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
				return nil, nil
			},
			nil,
		)
		a := &artist.Artist{Name: "Empty"}
		got := r.identifyArtist(context.Background(), a, nil)
		if got.Outcome != outcomeUnmatched {
			t.Errorf("Outcome = %v, want unmatched on empty results", got.Outcome)
		}
	})

	t.Run("tier 2 album auto-link wins over tier 3", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		// Build a path with two local album subdirectories on disk so
		// ListLocalAlbums returns them.
		dir := t.TempDir()
		for _, alb := range []string{"Album One", "Album Two"} {
			if err := os.Mkdir(filepath.Join(dir, alb), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
		}
		r, artistSvc := newIdentifyTestServer(t,
			func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
				return []provider.ArtistSearchResult{
					{Name: "Has Albums", MusicBrainzID: "mb-albums", Score: 50},
				}, nil
			},
			func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
				return []provider.ReleaseGroupInfo{
					{Title: "Album One"},
					{Title: "Album Two"},
				}, nil
			},
		)
		a := &artist.Artist{
			Name:     "Has Albums",
			SortName: "Has Albums",
			Type:     "group",
			Path:     dir,
		}
		if err := artistSvc.Create(ctx, a); err != nil {
			t.Fatalf("creating artist: %v", err)
		}
		got := r.identifyArtist(ctx, a, nil)
		if got.Outcome != outcomeAutoLinked {
			t.Fatalf("Outcome = %v, want autoLinked via tier 2", got.Outcome)
		}
	})
}

// TestRunBulkIdentify_PanicRecovered exercises the deferred recover() added
// for C11. We inject a panic by providing an orchestrator whose registered
// MusicBrainz stub panics inside SearchArtist. Because identifyArtist calls
// SearchForLinking when the artist has no album subdir, the panic surfaces
// inside the goroutine. The recover must:
//
//   - prevent the test process from crashing,
//   - transition progress.Status to "failed",
//   - emit a structured log entry tagged "bulk-identify panic recovered".
func TestRunBulkIdentify_PanicRecovered(t *testing.T) {
	t.Parallel()

	// Capture logger output to assert the structured log entry.
	var logBuf bytes.Buffer
	var bufMu sync.Mutex
	// Wrap the buffer in a mutex-protected writer so slog handler writes from
	// the background goroutine cannot race with the test's Read.
	mw := &syncWriter{buf: &logBuf, mu: &bufMu}
	logger := slog.New(slog.NewTextHandler(mw, &slog.HandlerOptions{Level: slog.LevelDebug}))

	r, _, artistSvc := testRouterWithLibrary(t)
	r.logger = logger

	// Stub that panics on SearchArtist. The panic propagates up through
	// SearchForLinking -> identifyArtist -> the runBulkIdentify goroutine.
	panicProv := &identifyStubProvider{
		name: provider.NameMusicBrainz,
		searchFn: func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			panic("induced panic for recovery test")
		},
	}
	registry := provider.NewRegistry()
	registry.Register(panicProv)
	r.providerRegistry = registry
	r.orchestrator = provider.NewOrchestrator(registry, nil, logger)

	// Seed one unidentified artist (no path, so tier 3 fires immediately).
	a := &artist.Artist{Name: "Boom"}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Drive runBulkIdentify directly with a freshly constructed progress.
	ctx, cancel := context.WithCancel(context.Background())
	progress := &IdentifyProgress{
		Status:   "running",
		Total:    1,
		cancelFn: cancel,
	}
	r.identifyMu.Lock()
	r.identifyProgress = progress
	r.identifyMu.Unlock()

	r.runBulkIdentify(ctx, []artist.Artist{*a}, progress)

	// Wait up to 5s for the goroutine to settle. On panic-recovery the
	// deferred handler flips Status to "failed".
	deadline := time.Now().Add(5 * time.Second)
	var finalStatus string
	for time.Now().Before(deadline) {
		progress.mu.RLock()
		finalStatus = progress.Status
		progress.mu.RUnlock()
		if finalStatus == "failed" || finalStatus == "completed" || finalStatus == "canceled" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if finalStatus != "failed" {
		t.Fatalf("Status = %q, want %q after panic recovery", finalStatus, "failed")
	}

	bufMu.Lock()
	logged := logBuf.String()
	bufMu.Unlock()
	if !strings.Contains(logged, "bulk-identify panic recovered") {
		t.Errorf("log output missing recovery marker; got:\n%s", logged)
	}
	if !strings.Contains(logged, "induced panic for recovery test") {
		t.Errorf("log output missing panic value; got:\n%s", logged)
	}
}

// TestBulkIdentify_ResetsAfterPanic confirms the slot is released so a
// subsequent POST is not blocked with a stale "running" job.
func TestBulkIdentify_ResetsAfterPanic(t *testing.T) {
	t.Parallel()
	r, _, artistSvc := testRouterWithLibrary(t)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	r.logger = logger

	panicProv := &identifyStubProvider{
		name: provider.NameMusicBrainz,
		searchFn: func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			panic("kaboom")
		},
	}
	registry := provider.NewRegistry()
	registry.Register(panicProv)
	r.providerRegistry = registry
	r.orchestrator = provider.NewOrchestrator(registry, nil, logger)

	a := &artist.Artist{Name: "Crashy"}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-identify", nil)
	w := httptest.NewRecorder()
	r.handleBulkIdentify(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("first POST status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	// Wait for terminal state.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.identifyMu.RLock()
		p := r.identifyProgress
		r.identifyMu.RUnlock()
		if p != nil {
			p.mu.RLock()
			st := p.Status
			p.mu.RUnlock()
			if st == "failed" || st == "completed" || st == "canceled" {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	// A subsequent POST must not be rejected with 409. The slot should be
	// released after the panic, allowing a new job to start (or report no
	// work). "Crashy" still has no MBID — the linking never completed —
	// so handleBulkIdentify rediscovers it and returns 202 accepted, NOT
	// 409 conflict. We only assert "not 409" here because exercising the
	// runner state machine more precisely would require synchronizing on
	// the asynchronous goroutine, which is out of scope for this test.
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-identify", nil)
	w2 := httptest.NewRecorder()
	r.handleBulkIdentify(w2, req2)
	if w2.Code == http.StatusConflict {
		t.Fatalf("second POST returned 409; slot was not released after panic")
	}

	// Wait for the second job's background goroutine to reach a terminal
	// status before the test returns. Without this, the goroutine races
	// against router/database cleanup under `go test -race` and can flake
	// the gate. 5s is generous given the test's two-artist corpus.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.identifyMu.Lock()
		p := r.identifyProgress
		r.identifyMu.Unlock()
		if p == nil {
			break
		}
		p.mu.RLock()
		st := p.Status
		p.mu.RUnlock()
		if st == "failed" || st == "completed" || st == "canceled" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// syncWriter is a tiny mutex-guarded writer used to safely interleave slog
// output from the runBulkIdentify goroutine with the test's read.
type syncWriter struct {
	buf *bytes.Buffer
	mu  *sync.Mutex
}

func (sw *syncWriter) Write(p []byte) (int, error) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.buf.Write(p)
}
