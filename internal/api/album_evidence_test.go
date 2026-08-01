package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// foundAlbums builds an AlbumSet in the EvidenceFound state. Test helper only:
// production code always gets its evidence from an AlbumSource, never by hand.
func foundAlbums(titles ...string) artist.AlbumSet {
	return artist.AlbumSet{Titles: titles, Evidence: artist.EvidenceFound, Origin: "test"}
}

// foundNoAlbums builds the POSITIVE "this artist has no albums" determination:
// the directory was read and held nothing. Distinct from the zero AlbumSet,
// which is EvidenceUnknown.
func foundNoAlbums() artist.AlbumSet {
	return artist.AlbumSet{Evidence: artist.EvidenceNone, Origin: "test"}
}

// unknownAlbums builds the "could not look" state -- an unreadable directory or
// a missing path.
func unknownAlbums() artist.AlbumSet {
	return artist.AlbumSet{Evidence: artist.EvidenceUnknown, Origin: "test"}
}

// TestAlbumEvidenceReasonDistinguishesUnknownFromNone is the core assertion of
// the migration: the two states that used to collapse into one operator-facing
// string no longer do.
func TestAlbumEvidenceReasonDistinguishesUnknownFromNone(t *testing.T) {
	t.Parallel()

	// Precondition: the zero value really is Unknown, so a forgotten field
	// produces "could not look" rather than a false "no albums" claim. If this
	// ever flips, every assertion below is testing the wrong thing.
	var zero artist.AlbumSet
	if zero.Evidence != artist.EvidenceUnknown {
		t.Fatalf("precondition: zero AlbumSet Evidence = %v, want EvidenceUnknown", zero.Evidence)
	}

	unknown := albumEvidenceReason(artist.EvidenceUnknown)
	none := albumEvidenceReason(artist.EvidenceNone)
	found := albumEvidenceReason(artist.EvidenceFound)

	if unknown != reasonLocalAlbumsUnreadable {
		t.Errorf("Unknown reason = %q, want %q", unknown, reasonLocalAlbumsUnreadable)
	}
	if none != reasonNoAlbumData {
		t.Errorf("None reason = %q, want %q", none, reasonNoAlbumData)
	}
	if unknown == none {
		t.Errorf("Unknown and None share the reason %q; the whole migration is that they differ", unknown)
	}

	// EvidenceFound DOES reach a fallback: the local albums were read fine and
	// nothing could supply the CANDIDATE's albums (no registry, no adapter, an
	// adapter without the album-fetching interface). It must say so.
	//
	// Both wrong answers are asserted against explicitly, because each is a
	// different lie: reasonNoAlbumData claims the artist owns no albums when it
	// may own a shelf full, and reasonLocalAlbumsUnreadable blames a filesystem
	// read that succeeded. The string must also not claim a FETCH failed -- every
	// caller reaches it from a missing-fetcher guard, before any call is made.
	if found != reasonNoCandidateAlbumSource {
		t.Errorf("Found reason = %q, want %q", found, reasonNoCandidateAlbumSource)
	}
	if found == reasonNoAlbumData {
		t.Errorf("Found reason = %q, must not claim the artist has no albums; what was missing was a source for the candidate's", found)
	}
	if found == reasonLocalAlbumsUnreadable {
		t.Errorf("Found reason = %q, must not claim the albums were unreadable", found)
	}

	// All three states must be mutually distinct, or a caller reading the string
	// cannot tell which of the three situations it is in. Asserting the pairs
	// individually above would still pass if two of them silently converged on a
	// value neither test named.
	if found == unknown || found == none {
		t.Errorf("Found reason %q collides with Unknown (%q) or None (%q); the three cases must be tellable apart", found, unknown, none)
	}
}

// TestEnrichAndScoreTier2NilCacheDoesNotFabricateNoAlbumData is the live-path
// guard for the third reason string.
//
// The route in: enrichAndScoreTier2 returns albumEvidenceReason BEFORE it looks
// at the evidence state, so a nil release-group cache (no provider registry, no
// MusicBrainz adapter, or an adapter not implementing ReleaseGroupFetcher) hits
// that line carrying EvidenceFound. Mapping that to reasonNoAlbumData told the
// operator "no album data available", which album_evidence.go documents as a
// POSITIVE determination that the artist owns no albums -- while the fixture
// below owns two.
//
// The gate DECISION was never wrong here (releasesKnown stays false, so the
// gate declines either way), which is exactly why only an assertion on the
// REASON can catch it. Both halves are asserted so a future change cannot fix
// the words by loosening the refusal.
func TestEnrichAndScoreTier2NilCacheDoesNotFabricateNoAlbumData(t *testing.T) {
	t.Parallel()

	r, _, _ := testRouterWithLibrary(t)
	r.providerRegistry = nil // forces newReleaseGroupCache to return nil

	// PRECONDITION: the local set really is a determination holding albums, so a
	// "no album data" reason would be a false claim rather than an accurate one.
	local := foundAlbums("First Record", "Second Record")
	if local.Evidence != artist.EvidenceFound || len(local.Titles) == 0 {
		t.Fatalf("precondition: local = %+v, want EvidenceFound with titles", local)
	}
	cache := r.newReleaseGroupCache()
	if cache != nil {
		t.Fatalf("precondition: newReleaseGroupCache() = %+v, want nil with no registry", cache)
	}

	got := r.enrichAndScoreTier2(context.Background(), []provider.ArtistSearchResult{{
		Name: "Gate Reason", MusicBrainzID: "7a8b9c0d-1e2f-4a3b-8c4d-5e6f7a8b9c0d",
	}}, local, cache)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Reason == reasonNoAlbumData {
		t.Errorf("Reason = %q, but the artist HAS albums (%v); that string is a positive determination and this is not one",
			got[0].Reason, local.Titles)
	}
	if got[0].Reason != reasonNoCandidateAlbumSource {
		t.Errorf("Reason = %q, want %q", got[0].Reason, reasonNoCandidateAlbumSource)
	}
	// The refusal itself is unchanged: a candidate catalogue nothing could supply
	// is not a determination, so the gate must still decline.
	if got[0].releasesKnown {
		t.Error("releasesKnown = true with no fetcher available; the gate would then treat an unmade call as a zero-release finding")
	}

	// THE WORDING INVARIANT, enforced rather than left to the doc comment.
	//
	// This string is reached ONLY from missing-fetcher guards, before any
	// provider call is made, so it must not assert that a retrieval was
	// ATTEMPTED and failed. An earlier draft ("candidate catalogues could not be
	// retrieved") did exactly that, which is the same defect class this reason
	// string was added to fix -- a reason asserting an event that did not
	// happen. Fetches that ARE attempted and fail never land here: those callers
	// log and continue, leaving the candidate on "album comparison".
	// Full words rather than a truncated stem: the repo's typo checker rejects
	// the stem form as a misspelling. Listing "retrieved" and "retrieval"
	// separately covers the same ground for any wording a human would write.
	for _, banned := range []string{"retrieved", "retrieval", "fetch", "failed"} {
		if strings.Contains(strings.ToLower(got[0].Reason), banned) {
			t.Errorf("Reason = %q contains %q, which claims a lookup was attempted; "+
				"this path is reached from a missing-fetcher guard, before any provider call", got[0].Reason, banned)
		}
	}
}

// TestMarkAlbumsUnavailableOnlyOnUnknown pins the JSON key's presence semantics:
// present means "something is wrong", absent means the lookup was a real answer.
func TestMarkAlbumsUnavailableOnlyOnUnknown(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		set  artist.AlbumSet
		want bool
	}{
		{"unknown sets the key", unknownAlbums(), true},
		{"genuinely empty omits the key", foundNoAlbums(), false},
		{"found albums omits the key", foundAlbums("Album One"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp := map[string]any{"results": []any{}}
			markAlbumsUnavailable(resp, tc.set)
			got, present := resp[albumsUnavailableKey]
			if present != tc.want {
				t.Fatalf("key %q present = %v, want %v (resp=%v)", albumsUnavailableKey, present, tc.want, resp)
			}
			if tc.want && got != true {
				t.Errorf("key %q = %v, want true", albumsUnavailableKey, got)
			}
		})
	}
}

// TestLocalAlbumSetLogsUnknown proves the WARN log is real. An Unknown album set
// silently degrades every candidate score on the page, so leaving no trace of it
// is the silent-failure class this series removes.
func TestLocalAlbumSetLogsUnknown(t *testing.T) {
	t.Parallel()

	newRouterWithLog := func(t *testing.T) (*Router, *bytes.Buffer) {
		t.Helper()
		r, _ := testRouter(t)
		var buf bytes.Buffer
		r.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		return r, &buf
	}

	t.Run("unreadable path logs WARN naming the artist and the cause", func(t *testing.T) {
		t.Parallel()
		r, buf := newRouterWithLog(t)

		// A regular file where a directory is expected: os.ReadDir fails, which
		// is the "mount is down" case in a form a test can create reliably.
		notADir := filepath.Join(t.TempDir(), "artist-is-a-file")
		if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}

		set := r.localAlbumSet(context.Background(), &artist.Artist{ID: "a-1", Name: "Unreadable Artist", Path: notADir})
		if set.Evidence != artist.EvidenceUnknown {
			t.Fatalf("Evidence = %v, want EvidenceUnknown for an unreadable path", set.Evidence)
		}

		logged := buf.String()
		if !strings.Contains(logged, "level=WARN") {
			t.Errorf("expected a WARN record, got: %s", logged)
		}
		if !strings.Contains(logged, "Unreadable Artist") {
			t.Errorf("log does not name the artist: %s", logged)
		}
		if !strings.Contains(logged, "a-1") {
			t.Errorf("log does not carry the artist id: %s", logged)
		}
		// The cause is the part that makes the record actionable; without it the
		// operator knows only that something failed.
		if !strings.Contains(logged, "reason=") {
			t.Errorf("log does not carry a reason: %s", logged)
		}
	})

	t.Run("genuinely empty directory is silent", func(t *testing.T) {
		t.Parallel()
		r, buf := newRouterWithLog(t)

		set := r.localAlbumSet(context.Background(), &artist.Artist{ID: "a-2", Name: "Empty Artist", Path: t.TempDir()})
		if set.Evidence != artist.EvidenceNone {
			t.Fatalf("Evidence = %v, want EvidenceNone for an empty readable directory", set.Evidence)
		}
		if got := buf.String(); got != "" {
			t.Errorf("a real determination must not log a warning; got: %s", got)
		}
	})

	t.Run("albums found is silent and carries the titles", func(t *testing.T) {
		t.Parallel()
		r, buf := newRouterWithLog(t)

		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "Album One"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		set := r.localAlbumSet(context.Background(), &artist.Artist{ID: "a-3", Name: "Full Artist", Path: dir})
		if set.Evidence != artist.EvidenceFound {
			t.Fatalf("Evidence = %v, want EvidenceFound", set.Evidence)
		}
		if len(set.Titles) != 1 || set.Titles[0] != "Album One" {
			t.Errorf("Titles = %v, want [Album One]", set.Titles)
		}
		if got := buf.String(); got != "" {
			t.Errorf("a successful lookup must not log a warning; got: %s", got)
		}
	})

	t.Run("no path recorded is Unknown and logged", func(t *testing.T) {
		t.Parallel()
		r, buf := newRouterWithLog(t)

		set := r.localAlbumSet(context.Background(), &artist.Artist{ID: "a-4", Name: "Pathless Artist"})
		if set.Evidence != artist.EvidenceUnknown {
			t.Fatalf("Evidence = %v, want EvidenceUnknown when no path is recorded", set.Evidence)
		}
		if !strings.Contains(buf.String(), "Pathless Artist") {
			t.Errorf("a pathless artist must still be logged; got: %s", buf.String())
		}
	})
}

// TestEnrichAndScoreTier2SetGatesOnEvidence proves the shared MusicBrainz scorer
// is never handed a set that is not a determination. That gate is what keeps
// handlers_identify.go's signature untouched while still making the display
// surfaces evidence-aware.
func TestEnrichAndScoreTier2SetGatesOnEvidence(t *testing.T) {
	t.Parallel()

	results := []provider.ArtistSearchResult{{Name: "A", MusicBrainzID: "mbid-1"}}

	t.Run("unknown never calls the release-group fetcher", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		installAudioDBOrchestrator(t, r, nil, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
			t.Fatal("GetReleaseGroups must not run for an Unknown album set: there is nothing to compare against")
			return nil, nil
		})
		got := r.enrichAndScoreTier2Set(context.Background(), results, unknownAlbums())
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1", len(got))
		}
		if got[0].Reason != reasonLocalAlbumsUnreadable {
			t.Errorf("Reason = %q, want %q", got[0].Reason, reasonLocalAlbumsUnreadable)
		}
		if got[0].AlbumComparison != nil {
			t.Errorf("AlbumComparison = %+v, want nil: no comparison was possible", got[0].AlbumComparison)
		}
	})

	t.Run("genuinely empty is a distinct reason", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		installAudioDBOrchestrator(t, r, nil, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
			t.Fatal("GetReleaseGroups must not run when the artist genuinely has no albums")
			return nil, nil
		})
		got := r.enrichAndScoreTier2Set(context.Background(), results, foundNoAlbums())
		if got[0].Reason != reasonNoAlbumData {
			t.Errorf("Reason = %q, want %q", got[0].Reason, reasonNoAlbumData)
		}
	})

	t.Run("found albums are scored", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		installAudioDBOrchestrator(t, r, nil, func(_ context.Context, mbid string) ([]provider.ReleaseGroupInfo, error) {
			if mbid != "mbid-1" {
				return nil, errors.New("unexpected mbid " + mbid)
			}
			return []provider.ReleaseGroupInfo{{Title: "Album One"}}, nil
		})
		got := r.enrichAndScoreTier2Set(context.Background(), results, foundAlbums("Album One"))
		if got[0].AlbumComparison == nil || got[0].AlbumComparison.MatchPercent != 100 {
			t.Fatalf("AlbumComparison = %+v, want a 100%% match", got[0].AlbumComparison)
		}
		if got[0].Reason == reasonLocalAlbumsUnreadable || got[0].Reason == reasonNoAlbumData {
			t.Errorf("Reason = %q, want the scored reason, not a fallback", got[0].Reason)
		}
	})
}

// TestEnrichDeezerCandidatesEvidenceFallbackReason covers the Deezer-keyed
// scorer's own fallback, which does not go through enrichAndScoreTier2Set.
func TestEnrichDeezerCandidatesEvidenceFallbackReason(t *testing.T) {
	t.Parallel()

	results := []provider.ArtistSearchResult{{Name: "A", ProviderID: "1"}}

	t.Run("unknown", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		installDeezerOrchestrator(t, r, nil, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
			t.Fatal("no album lookup may run for an Unknown album set")
			return nil, nil
		})
		got := r.enrichDeezerCandidates(context.Background(), results, unknownAlbums())
		if got[0].Reason != reasonLocalAlbumsUnreadable {
			t.Errorf("Reason = %q, want %q", got[0].Reason, reasonLocalAlbumsUnreadable)
		}
	})

	t.Run("none", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		installDeezerOrchestrator(t, r, nil, nil)
		got := r.enrichDeezerCandidates(context.Background(), results, foundNoAlbums())
		if got[0].Reason != reasonNoAlbumData {
			t.Errorf("Reason = %q, want %q", got[0].Reason, reasonNoAlbumData)
		}
	})

	t.Run("provider missing keeps the evidence-derived reason", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		r.providerRegistry = provider.NewRegistry() // Deezer not registered
		// The albums ARE readable here; the fallback is caused by the missing
		// provider, so the reason must accuse neither the filesystem nor the
		// artist's shelf -- it must name the candidate-side lookup that failed.
		got := r.enrichDeezerCandidates(context.Background(), results, foundAlbums("Album One"))
		if got[0].Reason != reasonNoCandidateAlbumSource {
			t.Errorf("Reason = %q, want %q (the albums were readable; the candidate lookup is what failed)", got[0].Reason, reasonNoCandidateAlbumSource)
		}
	})
}

// TestEnrichDiscogsCandidatesEvidenceFallbackReason mirrors the Deezer case for
// the Discogs-keyed scorer.
func TestEnrichDiscogsCandidatesEvidenceFallbackReason(t *testing.T) {
	t.Parallel()

	results := []provider.ArtistSearchResult{{Name: "A", ProviderID: "1"}}

	t.Run("unknown", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		installDiscogsOrchestrator(t, r, nil, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
			t.Fatal("no album lookup may run for an Unknown album set")
			return nil, nil
		})
		got := r.enrichDiscogsCandidates(context.Background(), results, unknownAlbums())
		if got[0].Reason != reasonLocalAlbumsUnreadable {
			t.Errorf("Reason = %q, want %q", got[0].Reason, reasonLocalAlbumsUnreadable)
		}
	})

	t.Run("none", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		installDiscogsOrchestrator(t, r, nil, nil)
		got := r.enrichDiscogsCandidates(context.Background(), results, foundNoAlbums())
		if got[0].Reason != reasonNoAlbumData {
			t.Errorf("Reason = %q, want %q", got[0].Reason, reasonNoAlbumData)
		}
	})
}

// TestEnrichWithAlbumComparisonFlagsUnavailable covers the disambiguation
// surface, whose view model carries a bool rather than a reason string.
func TestEnrichWithAlbumComparisonFlagsUnavailable(t *testing.T) {
	t.Parallel()

	results := []provider.ArtistSearchResult{{Name: "A", MusicBrainzID: "mbid-1"}}

	t.Run("unknown flags every candidate and skips the fetch", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		installAudioDBOrchestrator(t, r, nil, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
			t.Fatal("GetReleaseGroups must not run for an Unknown album set")
			return nil, nil
		})
		got := r.enrichWithAlbumComparison(context.Background(), "A", results, unknownAlbums())
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1", len(got))
		}
		if !got[0].AlbumsUnavailable {
			t.Error("AlbumsUnavailable = false, want true for an Unknown album set")
		}
		if got[0].AlbumComparison != nil {
			t.Errorf("AlbumComparison = %+v, want nil", got[0].AlbumComparison)
		}
	})

	t.Run("genuinely empty does not flag unavailable", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		installAudioDBOrchestrator(t, r, nil, nil)
		got := r.enrichWithAlbumComparison(context.Background(), "A", results, foundNoAlbums())
		if got[0].AlbumsUnavailable {
			t.Error("AlbumsUnavailable = true for a genuinely empty artist; that is the false claim in the other direction")
		}
	})

	t.Run("found albums are compared and not flagged", func(t *testing.T) {
		t.Parallel()
		r, _ := testRouter(t)
		installAudioDBOrchestrator(t, r, nil, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
			return []provider.ReleaseGroupInfo{{Title: "Album One"}}, nil
		})
		got := r.enrichWithAlbumComparison(context.Background(), "A", results, foundAlbums("Album One"))
		if got[0].AlbumsUnavailable {
			t.Error("AlbumsUnavailable = true despite a successful album read")
		}
		if got[0].AlbumComparison == nil || got[0].AlbumComparison.MatchPercent != 100 {
			t.Errorf("AlbumComparison = %+v, want a 100%% match", got[0].AlbumComparison)
		}
	})
}

// TestHandleDeezerSearchUnknownAlbumsJSON asserts the MARSHALED response, not a
// struct field: the whole point is what a client actually receives.
func TestHandleDeezerSearchUnknownAlbumsJSON(t *testing.T) {
	t.Parallel()

	// Two artists differing ONLY in whether their album directory is readable.
	// Anything that passes for both is not testing the distinction.
	search := func(t *testing.T, path string) map[string]any {
		t.Helper()
		r, artistSvc := testRouter(t)
		installDeezerOrchestrator(t, r,
			func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
				return []provider.ArtistSearchResult{{Name: "Cand", ProviderID: "555", Score: 60}}, nil
			}, nil)

		a := &artist.Artist{Name: "Subject", SortName: "Subject", Type: "group", Path: path}
		if err := artistSvc.Create(context.Background(), a); err != nil {
			t.Fatalf("create: %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/deezer/search",
			strings.NewReader(`{"query":"Subject"}`))
		req.SetPathValue("id", a.ID)
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(testI18nCtx(t, req.Context()))
		w := httptest.NewRecorder()
		r.handleDeezerSearch(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v; body=%s", err, w.Body.String())
		}
		return body
	}

	unreadable := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(unreadable, []byte("x"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	unknownBody := search(t, unreadable)
	if unknownBody[albumsUnavailableKey] != true {
		t.Errorf("unreadable path: %q = %v, want true; body=%v", albumsUnavailableKey, unknownBody[albumsUnavailableKey], unknownBody)
	}

	emptyBody := search(t, t.TempDir())
	if _, present := emptyBody[albumsUnavailableKey]; present {
		t.Errorf("genuinely empty directory must NOT set %q; body=%v", albumsUnavailableKey, emptyBody)
	}

	// Precondition: both responses carried candidates, so the assertion above is
	// about the evidence key and not about an accidentally empty result set.
	for name, body := range map[string]map[string]any{"unknown": unknownBody, "empty": emptyBody} {
		res, ok := body["results"].([]any)
		if !ok || len(res) != 1 {
			t.Fatalf("%s: results = %v, want exactly 1 candidate", name, body["results"])
		}
	}
}

// TestHandleDeezerSearchUnknownAlbumsHTMX asserts the RENDERED fragment says the
// albums were not checked, rather than silently omitting the badge.
func TestHandleDeezerSearchUnknownAlbumsHTMX(t *testing.T) {
	t.Parallel()

	render := func(t *testing.T, path string) string {
		t.Helper()
		r, artistSvc := testRouter(t)
		installDeezerOrchestrator(t, r,
			func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
				return []provider.ArtistSearchResult{{Name: "Cand", ProviderID: "555", Score: 60}}, nil
			}, nil)

		a := &artist.Artist{Name: "Subject", SortName: "Subject", Type: "group", Path: path}
		if err := artistSvc.Create(context.Background(), a); err != nil {
			t.Fatalf("create: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/deezer/search",
			strings.NewReader(`{"query":"Subject"}`))
		req.SetPathValue("id", a.ID)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("HX-Request", "true")
		req = req.WithContext(testI18nCtx(t, req.Context()))
		w := httptest.NewRecorder()
		r.handleDeezerSearch(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	unreadable := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(unreadable, []byte("x"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// The banner copy is the operator-facing sentence; matching on a fragment of
	// it keeps the assertion tied to what is actually rendered.
	const marker = "could not be read"

	unknownHTML := render(t, unreadable)
	if !strings.Contains(unknownHTML, marker) {
		t.Errorf("unreadable path: rendered fragment does not say the albums could not be read:\n%s", unknownHTML)
	}
	// Precondition: the candidate itself rendered, so the banner is additive and
	// did not replace the list.
	if !strings.Contains(unknownHTML, "Cand") {
		t.Errorf("candidate row missing from the fragment:\n%s", unknownHTML)
	}

	emptyHTML := render(t, t.TempDir())
	if strings.Contains(emptyHTML, marker) {
		t.Errorf("genuinely empty directory must not claim the albums could not be read:\n%s", emptyHTML)
	}
	if !strings.Contains(emptyHTML, "Cand") {
		t.Errorf("candidate row missing from the empty-artist fragment:\n%s", emptyHTML)
	}
}
