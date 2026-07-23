package api

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/event"
	"github.com/sydlexius/stillwater/internal/provider"
)

// errBulkRefreshProviderTest is a sentinel error used to exercise the failed
// outcome of refresh_metadata when the provider fetch itself errors.
var errBulkRefreshProviderTest = errors.New("bulk refresh provider test failure")

// bulkRefreshRouter builds a Router wired with a stub pipeline and a stub
// orchestrator whose FetchMetadata returns fetchResult, so the refresh_metadata
// bulk action runs end to end without any provider network call.
func bulkRefreshRouter(t *testing.T, fetchResult *provider.FetchResult) (*Router, *artist.Service) {
	t.Helper()
	r, artistSvc := testRouterWithStubPipeline(t, &stubPipeline{})
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := provider.NewOrchestrator(nil, nil, logger, nil)
	orch.SetExecutor(&stubScraperExecutor{result: fetchResult})
	r.orchestrator = orch
	return r, artistSvc
}

// startBulkRefresh posts a refresh_metadata bulk action for the given IDs and
// asserts the handler accepted it.
func startBulkRefresh(t *testing.T, r *Router, ids ...string) {
	t.Helper()
	payload := `{"action":"refresh_metadata","ids":["` + strings.Join(ids, `","`) + `"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handleBulkAction(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
	}
}

// addRefreshableArtist creates an artist carrying a MusicBrainz ID, which is
// the precondition refresh_metadata requires. Without the MBID the action
// skips, so every "did the refresh run" assertion below would pass vacuously
// against an artist created by the plain addTestArtist helper.
func addRefreshableArtist(t *testing.T, svc *artist.Service, name string) *artist.Artist {
	t.Helper()
	a := addTestArtist(t, svc, name)
	a.MusicBrainzID = "mbid-" + name
	if err := svc.Update(context.Background(), a); err != nil {
		t.Fatalf("seeding MBID for %s: %v", name, err)
	}
	reloaded, err := svc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reloading %s: %v", name, err)
	}
	if reloaded.MusicBrainzID == "" {
		t.Fatalf("precondition failed: %s has no MusicBrainz ID after seeding", name)
	}
	return reloaded
}

// TestBulkAction_RefreshMetadata_Success is the happy path: an artist with a
// MusicBrainz ID is refreshed and the provider-returned biography is persisted.
// Asserting the persisted field (not just the counter) is what distinguishes a
// real refresh from a no-op that reports Succeeded.
func TestBulkAction_RefreshMetadata_Success(t *testing.T) {
	t.Parallel()
	r, artistSvc := bulkRefreshRouter(t, &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			Biography: "Fetched from the provider.",
		},
		AttemptedFields: []string{"biography"},
		PopulatedFields: []string{"biography"},
	})
	a := addRefreshableArtist(t, artistSvc, "Bulk Refresh Artist")

	startBulkRefresh(t, r, a.ID)
	waitBulkActionCompleted(t, r)

	r.bulkActionMu.RLock()
	p := r.bulkActionProgress
	r.bulkActionMu.RUnlock()
	snap := p.snapshot()
	if snap["action"] != "refresh_metadata" {
		t.Errorf("action = %v, want refresh_metadata", snap["action"])
	}
	if snap["succeeded"] != 1 {
		t.Errorf("succeeded = %v, want 1", snap["succeeded"])
	}

	saved, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	if saved.Biography != "Fetched from the provider." {
		t.Errorf("biography = %q, want the provider value; the action reported success without persisting anything", saved.Biography)
	}
}

// TestBulkAction_RefreshMetadata_NoMBIDSkipped covers the divergence from the
// single-artist handler: that handler answers a missing MusicBrainz ID with the
// disambiguation UI, which bulk has no way to render. The artist must be
// counted as Skipped and the run must continue rather than failing.
func TestBulkAction_RefreshMetadata_NoMBIDSkipped(t *testing.T) {
	t.Parallel()
	r, artistSvc := bulkRefreshRouter(t, &provider.FetchResult{
		Metadata:        &provider.ArtistMetadata{Biography: "Should never be applied."},
		AttemptedFields: []string{"biography"},
		PopulatedFields: []string{"biography"},
	})
	// addTestArtist deliberately, not addRefreshableArtist: no MusicBrainz ID.
	a := addTestArtist(t, artistSvc, "No MBID Artist")
	if a.MusicBrainzID != "" {
		t.Fatalf("precondition failed: test artist unexpectedly has MBID %q", a.MusicBrainzID)
	}

	startBulkRefresh(t, r, a.ID)
	waitBulkActionCompleted(t, r)

	r.bulkActionMu.RLock()
	p := r.bulkActionProgress
	r.bulkActionMu.RUnlock()
	snap := p.snapshot()
	if snap["skipped"] != 1 {
		t.Errorf("skipped = %v, want 1", snap["skipped"])
	}
	if snap["failed"] != 0 {
		t.Errorf("failed = %v, want 0 (a missing MBID is a skip, not a failure)", snap["failed"])
	}

	saved, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	if saved.Biography != "" {
		t.Errorf("biography = %q, want empty; a skipped artist must not be written", saved.Biography)
	}
}

// TestBulkAction_RefreshMetadata_LockedSkipped holds the artist-lock contract
// the UI states verbatim: "Provider refreshes and rule fixers will not make
// automated changes." A bulk sweep is exactly such an automated change, so a
// locked artist must be skipped.
//
// The single-artist path states the same contract and is covered by the paired
// tests in handlers_refresh_test.go (TestHandleArtistRefresh_LockedArtistSkipped
// and its positive control TestHandleArtistRefresh_UnlockedArtistStillRefreshes).
// Read the two groups together; #2754 was the gap where this path was gated and
// that one was not.
func TestBulkAction_RefreshMetadata_LockedSkipped(t *testing.T) {
	t.Parallel()
	r, artistSvc := bulkRefreshRouter(t, &provider.FetchResult{
		Metadata:        &provider.ArtistMetadata{Biography: "Should never be applied."},
		AttemptedFields: []string{"biography"},
		PopulatedFields: []string{"biography"},
	})
	a := addRefreshableArtist(t, artistSvc, "Locked Refresh Artist")
	if err := artistSvc.Lock(context.Background(), a.ID, "user"); err != nil {
		t.Fatalf("locking artist: %v", err)
	}
	locked, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	if !locked.Locked {
		t.Fatal("precondition failed: artist is not locked after Lock")
	}

	startBulkRefresh(t, r, a.ID)
	waitBulkActionCompleted(t, r)

	r.bulkActionMu.RLock()
	p := r.bulkActionProgress
	r.bulkActionMu.RUnlock()
	snap := p.snapshot()
	if snap["skipped"] != 1 {
		t.Errorf("skipped = %v, want 1", snap["skipped"])
	}

	saved, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	if saved.Biography != "" {
		t.Errorf("biography = %q, want empty; a locked artist must not be refreshed", saved.Biography)
	}
}

// TestBulkAction_RefreshMetadata_FieldLocksSkipOnlyThoseFields draws the line
// between the two kinds of lock, which behave differently on purpose:
//
//   - The ARTIST lock (a.Locked) suppresses the whole automated refresh; the
//     artist is Skipped (see TestBulkAction_RefreshMetadata_LockedSkipped).
//   - FIELD locks (a.LockedFields) suppress only the pinned fields. The artist
//     still refreshes and still counts as Succeeded; the pinned values survive
//     while every unpinned field updates normally.
//
// Without this test, a future change that widened the artist-level gate to
// "has any lock at all" would silently downgrade every field-pinned artist to
// Skipped, and the counters would still look plausible.
func TestBulkAction_RefreshMetadata_FieldLocksSkipOnlyThoseFields(t *testing.T) {
	t.Parallel()
	r, artistSvc := bulkRefreshRouter(t, &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			Biography: "Provider biography that must NOT land.",
			Origin:    "Provider origin that MUST land.",
		},
		AttemptedFields: []string{"biography", "origin"},
		PopulatedFields: []string{"biography", "origin"},
	})

	a := addRefreshableArtist(t, artistSvc, "Field Locked Artist")
	a.Biography = "User-curated biography."
	a.Origin = "Old origin."
	if err := artistSvc.Update(context.Background(), a); err != nil {
		t.Fatalf("seeding fields: %v", err)
	}
	if err := artistSvc.SetLockedFields(context.Background(), a.ID, []string{string(artist.FieldBiography)}); err != nil {
		t.Fatalf("pinning biography: %v", err)
	}

	// Precondition: the artist carries a field lock but is NOT artist-locked.
	// Without this assertion the test would pass vacuously against an artist
	// that was skipped wholesale for an unrelated reason.
	pre, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	if pre.Locked {
		t.Fatal("precondition failed: artist is artist-locked; this test covers FIELD locks")
	}
	if !slices.Contains(pre.LockedFields, string(artist.FieldBiography)) {
		t.Fatalf("precondition failed: biography not pinned; LockedFields = %v", pre.LockedFields)
	}

	startBulkRefresh(t, r, a.ID)
	waitBulkActionCompleted(t, r)

	r.bulkActionMu.RLock()
	p := r.bulkActionProgress
	r.bulkActionMu.RUnlock()
	snap := p.snapshot()
	if snap["succeeded"] != 1 {
		t.Errorf("succeeded = %v, want 1; a field-locked artist still refreshes", snap["succeeded"])
	}
	if snap["skipped"] != 0 {
		t.Errorf("skipped = %v, want 0; only the pinned FIELD is skipped, not the artist", snap["skipped"])
	}

	saved, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	if saved.Biography != "User-curated biography." {
		t.Errorf("pinned biography overwritten: got %q", saved.Biography)
	}
	if saved.Origin != "Provider origin that MUST land." {
		t.Errorf("unpinned origin not updated: got %q; the refresh must still apply unlocked fields", saved.Origin)
	}
}

// readProviderIDRows returns provider -> provider_id straight from the
// artist_provider_ids table.
//
// It reads the TABLE rather than the hydrated Artist struct on purpose: that
// table is the durable record every downstream consumer queries, so a struct
// field populated in memory but never written would sail past a struct-level
// assertion. There is no exported Service accessor for these rows (the
// repository's GetForArtist is reached only through unexported hydration), so
// the query is issued directly, matching how other tests in this package read
// normalized tables.
func readProviderIDRows(t *testing.T, r *Router, artistID string) map[string]string {
	t.Helper()
	rows, err := r.db.QueryContext(context.Background(),
		`SELECT provider, provider_id FROM artist_provider_ids WHERE artist_id = ?`, artistID)
	if err != nil {
		t.Fatalf("querying artist_provider_ids: %v", err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil {
			t.Errorf("closing rows: %v", cerr)
		}
	}()

	out := map[string]string{}
	for rows.Next() {
		var provider string
		var providerID sql.NullString
		if err := rows.Scan(&provider, &providerID); err != nil {
			t.Fatalf("scanning artist_provider_ids row: %v", err)
		}
		out[provider] = providerID.String
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating artist_provider_ids: %v", err)
	}
	return out
}

// TestBulkAction_RefreshMetadata_PersistsMatchedProviderIDs asserts that every
// provider ID the fetch matched is persisted to artist_provider_ids, not just
// the MusicBrainz ID the refresh keyed off.
//
// This is the payoff of the whole action: the cross-provider IDs are what let
// later refreshes, image fetches, and rule fixers reach a provider directly
// instead of re-resolving by name every time. A bulk refresh that updated the
// biography but dropped the IDs would look successful in every counter and
// still leave the artist unlinked.
//
// The assertion reads the normalized table rather than the Artist struct
// because that table is the durable record downstream code queries; a struct
// field populated in memory but never written would pass a struct-level check.
func TestBulkAction_RefreshMetadata_PersistsMatchedProviderIDs(t *testing.T) {
	t.Parallel()
	r, artistSvc := bulkRefreshRouter(t, &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			MusicBrainzID: "mbid-Provider IDs Artist",
			AudioDBID:     "adb-1234",
			DiscogsID:     "dgs-5678",
			WikidataID:    "Q4242",
			DeezerID:      "dz-9012",
			SpotifyID:     "sp-3456",
		},
		AttemptedFields: []string{"audiodb_id", "discogs_id", "wikidata_id", "deezer_id", "spotify_id"},
		PopulatedFields: []string{"audiodb_id", "discogs_id", "wikidata_id", "deezer_id", "spotify_id"},
	})

	a := addRefreshableArtist(t, artistSvc, "Provider IDs Artist")

	// Precondition: only the MusicBrainz ID exists going in, so a passing
	// assertion below cannot be satisfied by pre-seeded rows.
	before := readProviderIDRows(t, r, a.ID)
	for prov := range before {
		if prov != "musicbrainz" {
			t.Fatalf("precondition failed: unexpected pre-existing provider row %q", prov)
		}
	}

	startBulkRefresh(t, r, a.ID)
	waitBulkActionCompleted(t, r)

	got := readProviderIDRows(t, r, a.ID)

	want := map[string]string{
		"musicbrainz": "mbid-Provider IDs Artist",
		"audiodb":     "adb-1234",
		"discogs":     "dgs-5678",
		"wikidata":    "Q4242",
		"deezer":      "dz-9012",
		"spotify":     "sp-3456",
	}
	for prov, id := range want {
		if got[prov] != id {
			t.Errorf("provider %q: id = %q, want %q (matched provider IDs must survive the refresh)", prov, got[prov], id)
		}
	}
}

// TestBulkAction_RefreshMetadata_RequiresOrchestrator locks in the 503 +
// slot-release path when no provider orchestrator is wired. Without the
// release, a failed start would block every subsequent bulk action with a 409.
func TestBulkAction_RefreshMetadata_RequiresOrchestrator(t *testing.T) {
	t.Parallel()
	// testRouterWithStubPipeline wires an artist service and pipeline but
	// leaves r.orchestrator nil.
	r, _ := testRouterWithStubPipeline(t, &stubPipeline{})
	if r.orchestrator != nil {
		t.Fatal("precondition failed: test router unexpectedly has an orchestrator")
	}

	body := strings.NewReader(`{"action":"refresh_metadata","ids":["abc123"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleBulkAction(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", w.Code, w.Body.String())
	}
	r.bulkActionMu.RLock()
	progress := r.bulkActionProgress
	r.bulkActionMu.RUnlock()
	if progress != nil {
		t.Errorf("bulkActionProgress not released after 503; got %+v", progress)
	}
}

// TestBulkAction_RefreshMetadata_NoPipelineStillRefreshes guards the
// deliberate asymmetry in requireServicesForBulkAction: refresh_metadata does
// NOT require a rule pipeline, because the single-artist refresh handler
// completes the refresh whether or not one is wired (runRulesAfterRefresh
// returns early on nil). Requiring one here would 503 a request the per-artist
// button serves.
func TestBulkAction_RefreshMetadata_NoPipelineStillRefreshes(t *testing.T) {
	t.Parallel()
	r, artistSvc := bulkRefreshRouter(t, &provider.FetchResult{
		Metadata:        &provider.ArtistMetadata{Biography: "Refreshed without a pipeline."},
		AttemptedFields: []string{"biography"},
		PopulatedFields: []string{"biography"},
	})
	r.pipeline = nil
	a := addRefreshableArtist(t, artistSvc, "No Pipeline Artist")

	startBulkRefresh(t, r, a.ID)
	waitBulkActionCompleted(t, r)

	r.bulkActionMu.RLock()
	p := r.bulkActionProgress
	r.bulkActionMu.RUnlock()
	if snap := p.snapshot(); snap["succeeded"] != 1 {
		t.Errorf("succeeded = %v, want 1", snap["succeeded"])
	}

	saved, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	if saved.Biography != "Refreshed without a pipeline." {
		t.Errorf("biography = %q, want the provider value", saved.Biography)
	}
}

// TestBulkAction_RefreshMetadata_ProviderErrorFails covers the failed outcome:
// when the provider fetch itself errors, the artist must count as Failed, not
// Skipped. The distinction matters to the operator -- Skipped reads as "nothing
// to do here", Failed as "this needs attention".
func TestBulkAction_RefreshMetadata_ProviderErrorFails(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithStubPipeline(t, &stubPipeline{})
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := provider.NewOrchestrator(nil, nil, logger, nil)
	orch.SetExecutor(&stubScraperExecutor{err: errBulkRefreshProviderTest})
	r.orchestrator = orch

	a := addRefreshableArtist(t, artistSvc, "Provider Error Artist")

	startBulkRefresh(t, r, a.ID)
	waitBulkActionCompleted(t, r)

	r.bulkActionMu.RLock()
	p := r.bulkActionProgress
	r.bulkActionMu.RUnlock()
	snap := p.snapshot()
	if snap["failed"] != 1 {
		t.Errorf("failed = %v, want 1", snap["failed"])
	}
	if snap["succeeded"] != 0 {
		t.Errorf("succeeded = %v, want 0", snap["succeeded"])
	}
	if snap["skipped"] != 0 {
		t.Errorf("skipped = %v, want 0 (a provider error is a failure, not a skip)", snap["skipped"])
	}
}

// TestBulkAction_RefreshMetadata_PublishesArtistUpdated verifies the per-artist
// ArtistUpdated event fires, which is what drives the SSE-backed live refresh
// of any open artist view. The single-artist handler publishes it; without this
// the bulk path would refresh the database while every open page showed stale
// data until a manual reload.
func TestBulkAction_RefreshMetadata_PublishesArtistUpdated(t *testing.T) {
	t.Parallel()
	r, artistSvc := bulkRefreshRouter(t, &provider.FetchResult{
		Metadata:        &provider.ArtistMetadata{Biography: "Event biography."},
		AttemptedFields: []string{"biography"},
		PopulatedFields: []string{"biography"},
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := event.NewBus(logger, 1024)
	var mu sync.Mutex
	var updatedIDs []string
	bus.Subscribe(event.ArtistUpdated, func(e event.Event) {
		mu.Lock()
		defer mu.Unlock()
		if id, ok := e.Data["artist_id"].(string); ok {
			updatedIDs = append(updatedIDs, id)
		}
	})
	go bus.Start()
	defer bus.Stop()
	r.eventBus = bus

	a := addRefreshableArtist(t, artistSvc, "Event Artist")

	startBulkRefresh(t, r, a.ID)
	waitBulkActionCompleted(t, r)

	// The bus dispatches asynchronously; give it a moment to drain.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(updatedIDs)
		mu.Unlock()
		if got > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(updatedIDs, a.ID) {
		t.Errorf("ArtistUpdated not published for %s; got %v", a.ID, updatedIDs)
	}
}

// TestBulkAction_RefreshMetadata_AppliesProviderName covers the
// applyProviderName hop: a provider that returns a language-promoted name must
// have it persisted, exactly as the single-artist refresh does. Passing nil
// metadata here instead would make applyProviderName a silent no-op, so this
// asserts the persisted name rather than the outcome counter.
func TestBulkAction_RefreshMetadata_AppliesProviderName(t *testing.T) {
	t.Parallel()
	r, artistSvc := bulkRefreshRouter(t, &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			Name:     "Promoted Name",
			SortName: "Promoted Name, The",
		},
		AttemptedFields: []string{"name"},
		PopulatedFields: []string{"name"},
	})
	a := addRefreshableArtist(t, artistSvc, "Original Name")
	if a.Name != "Original Name" {
		t.Fatalf("precondition failed: name = %q", a.Name)
	}

	startBulkRefresh(t, r, a.ID)
	waitBulkActionCompleted(t, r)

	saved, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	if saved.Name != "Promoted Name" {
		t.Errorf("name = %q, want the provider-promoted value", saved.Name)
	}
}

// TestBulkAction_RefreshMetadata_MixedOutcomes runs a batch spanning all three
// outcomes so the progress counters are verified against a single run rather
// than three isolated ones -- the shape an operator actually sees.
func TestBulkAction_RefreshMetadata_MixedOutcomes(t *testing.T) {
	t.Parallel()
	r, artistSvc := bulkRefreshRouter(t, &provider.FetchResult{
		Metadata:        &provider.ArtistMetadata{Biography: "Batch biography."},
		AttemptedFields: []string{"biography"},
		PopulatedFields: []string{"biography"},
	})

	ok := addRefreshableArtist(t, artistSvc, "Mixed OK")
	noMBID := addTestArtist(t, artistSvc, "Mixed No MBID")
	locked := addRefreshableArtist(t, artistSvc, "Mixed Locked")
	if err := artistSvc.Lock(context.Background(), locked.ID, "user"); err != nil {
		t.Fatalf("locking artist: %v", err)
	}

	startBulkRefresh(t, r, ok.ID, noMBID.ID, locked.ID, "nonexistent-id")
	waitBulkActionCompleted(t, r)

	r.bulkActionMu.RLock()
	p := r.bulkActionProgress
	r.bulkActionMu.RUnlock()
	snap := p.snapshot()
	if snap["total"] != 4 {
		t.Errorf("total = %v, want 4", snap["total"])
	}
	if snap["processed"] != 4 {
		t.Errorf("processed = %v, want 4", snap["processed"])
	}
	if snap["succeeded"] != 1 {
		t.Errorf("succeeded = %v, want 1", snap["succeeded"])
	}
	// no-MBID + locked + not-found all count as Skipped.
	if snap["skipped"] != 3 {
		t.Errorf("skipped = %v, want 3", snap["skipped"])
	}
	if snap["failed"] != 0 {
		t.Errorf("failed = %v, want 0", snap["failed"])
	}
}
