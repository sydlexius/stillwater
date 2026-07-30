package rule

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/provider"
	_ "modernc.org/sqlite"
)

// Real-shaped MusicBrainz IDs, matching the fixture convention in
// fixers_mbid_confidence_test.go (isValidMBID only accepts a 36-character
// UUID, so a fake string gets filtered out before the gate ever runs and the
// test would pass vacuously).
const (
	bulkMBIDConfident = "a74b1b7f-71a5-4011-9441-d0b5e4122711"
	bulkMBIDWeakFirst = "8538e728-ca0b-4321-b7e5-cff6565dd4c0"
)

// mockSearchProvider is a minimal provider.Provider that only implements
// SearchArtist meaningfully; fetchImages's self-heal branch never reaches
// GetArtist/GetImages for these tests since they exercise the MBID gate
// itself, before FetchImages would be called.
type mockSearchProvider struct {
	name    provider.ProviderName
	results []provider.ArtistSearchResult
}

func (m *mockSearchProvider) Name() provider.ProviderName { return m.name }
func (m *mockSearchProvider) RequiresAuth() bool          { return false }
func (m *mockSearchProvider) SearchArtist(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
	return m.results, nil
}
func (m *mockSearchProvider) GetArtist(_ context.Context, _ string) (*provider.ArtistMetadata, error) {
	return nil, nil
}
func (m *mockSearchProvider) GetImages(_ context.Context, _ string) ([]provider.ImageResult, error) {
	return nil, nil
}

// newBulkGateOrchestrator builds a real *provider.Orchestrator backed by a
// single mock MusicBrainz-named provider (MusicBrainz requires no API key,
// so it is always in availableProviders without extra settings fixtures).
func newBulkGateOrchestrator(t *testing.T, results []provider.ArtistSearchResult) *provider.Orchestrator {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening settings db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(context.Background(), `CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TEXT NOT NULL DEFAULT (datetime('now')))`); err != nil {
		t.Fatalf("creating settings table: %v", err)
	}
	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	settings := provider.NewSettingsService(db, enc)
	registry := provider.NewRegistry()
	registry.Register(&mockSearchProvider{name: provider.NameMusicBrainz, results: results})
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return provider.NewOrchestrator(registry, settings, logger, nil)
}

// newBulkGateExecutor wires a BulkExecutor with a real artist.Service (SQLite)
// and the given search results, ready to drive fetchImages's self-heal
// branch. The artist starts with no images present and no MBID so the
// self-heal branch is reached deterministically.
func newBulkGateExecutor(t *testing.T, results []provider.ArtistSearchResult) (*BulkExecutor, *artist.Service, *artist.HistoryService, *artist.Artist) {
	t.Helper()
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	historySvc := artist.NewHistoryService(db)

	a := &artist.Artist{
		Name:     "Radiohead",
		SortName: "Radiohead",
		Path:     t.TempDir(),
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if a.MusicBrainzID != "" {
		t.Fatalf("fixture precondition: artist must start with an empty MBID, got %q", a.MusicBrainzID)
	}

	orch := newBulkGateOrchestrator(t, results)
	e := &BulkExecutor{
		artistService: artistSvc,
		orchestrator:  orch,
		logger:        testLogger(),
	}
	e.SetHistoryService(historySvc)
	return e, artistSvc, historySvc, a
}

// TestFetchImages_MBIDGate_RejectsWeakScoreFloor is issue #2825's own stated
// acceptance criterion, isolated to the SCORE axis alone (a single
// low-scoring hit, no rival): the original "first result with an MBID" loop
// read no score at all, so this is the exact case it adopted wrongly. The
// gated version must decline instead.
func TestFetchImages_MBIDGate_RejectsWeakScoreFloor(t *testing.T) {
	if !artist.IsValidMBID(bulkMBIDWeakFirst) {
		t.Fatalf("fixture precondition: fixture MBID must be a syntactically valid UUID")
	}
	results := []provider.ArtistSearchResult{
		// The only candidate: a matching name, but a score below
		// MBIDMinProviderScore (85). A position-only ("first with an MBID")
		// selection rule adopts this; the score gate must not.
		{Name: "Radiohead", MusicBrainzID: bulkMBIDWeakFirst, Score: 40, Source: "musicbrainz"},
	}
	e, artistSvc, historySvc, a := newBulkGateExecutor(t, results)

	if len(results) != 1 {
		t.Fatalf("fixture precondition: expected 1 search result, got %d", len(results))
	}

	status, msg := e.fetchImages(context.Background(), a, BulkModeYOLO, nil)

	if status != BulkItemSkipped {
		t.Fatalf("status = %q, want %q (message: %q)", status, BulkItemSkipped, msg)
	}
	if a.MusicBrainzID != "" {
		t.Fatalf("a.MusicBrainzID = %q, want empty: the weak-scoring hit must not be adopted", a.MusicBrainzID)
	}
	if !strings.Contains(msg, "declined") || !strings.Contains(msg, "confidence floor") {
		t.Errorf("message %q does not name the score-floor decline reason", msg)
	}

	// Re-read from the DB: confirm nothing persisted.
	reloaded, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("re-reading artist: %v", err)
	}
	if reloaded.MusicBrainzID != "" {
		t.Fatalf("persisted a.MusicBrainzID = %q, want empty", reloaded.MusicBrainzID)
	}

	// No history row should exist: nothing was adopted.
	changes, _, err := historySvc.List(context.Background(), a.ID, 50, 0)
	if err != nil {
		t.Fatalf("listing history: %v", err)
	}
	for _, c := range changes {
		if c.Field == "musicbrainz_id" {
			t.Errorf("unexpected musicbrainz_id history row on a declined candidate: %+v", c)
		}
	}
}

// TestFetchImages_MBIDGate_RejectsNameMismatch isolates the NAME-SIMILARITY
// axis: a high provider score alone must not be sufficient when the returned
// name barely resembles the artist's own name (a well-known artist can score
// 100 for an unrelated hit via popularity/tag matches).
func TestFetchImages_MBIDGate_RejectsNameMismatch(t *testing.T) {
	results := []provider.ArtistSearchResult{
		{Name: "Some Completely Different Act", MusicBrainzID: bulkMBIDConfident, Score: 100, Source: "musicbrainz"},
	}
	e, _, _, a := newBulkGateExecutor(t, results)
	a.Name = "Radiohead"

	status, msg := e.fetchImages(context.Background(), a, BulkModeYOLO, nil)

	if status != BulkItemSkipped {
		t.Fatalf("status = %q, want %q (message: %q)", status, BulkItemSkipped, msg)
	}
	if !strings.Contains(msg, "matches the artist name only") {
		t.Errorf("message %q does not name the name-similarity decline reason", msg)
	}
	if a.MusicBrainzID != "" {
		t.Fatalf("a.MusicBrainzID = %q, want empty", a.MusicBrainzID)
	}
}

// TestFetchImages_MBIDGate_RejectsAmbiguousRivals isolates the ambiguity-
// margin axis specifically (as opposed to the score or name-similarity
// axes), per the fixture-discrimination requirement: two same-named,
// similarly-scored hits with DIFFERENT MBIDs must be rejected as ambiguous,
// not adopted via score/name floors alone.
func TestFetchImages_MBIDGate_RejectsAmbiguousRivals(t *testing.T) {
	results := []provider.ArtistSearchResult{
		{Name: "Nirvana", MusicBrainzID: bulkMBIDConfident, Score: 100, Source: "musicbrainz"},
		{Name: "Nirvana", MusicBrainzID: bulkMBIDWeakFirst, Score: 95, Source: "musicbrainz"},
	}
	e, _, _, a := newBulkGateExecutor(t, results)
	a.Name = "Nirvana"

	status, msg := e.fetchImages(context.Background(), a, BulkModeYOLO, nil)

	if status != BulkItemSkipped {
		t.Fatalf("status = %q, want %q (message: %q)", status, BulkItemSkipped, msg)
	}
	if !strings.Contains(msg, "ambiguous") {
		t.Errorf("message %q does not name the ambiguity reason specifically", msg)
	}
	if a.MusicBrainzID != "" {
		t.Fatalf("a.MusicBrainzID = %q, want empty", a.MusicBrainzID)
	}
}

// TestFetchImages_MBIDGate_AdoptsConfidentMatch is the accept path: a
// confident, unambiguous, single result is adopted, stamped machine-picked,
// and produces a history row with the correct source and an empty old_value.
func TestFetchImages_MBIDGate_AdoptsConfidentMatch(t *testing.T) {
	results := []provider.ArtistSearchResult{
		{Name: "Radiohead", MusicBrainzID: bulkMBIDConfident, Score: 100, Source: "musicbrainz"},
	}
	e, artistSvc, historySvc, a := newBulkGateExecutor(t, results)

	// The self-heal branch is followed by an image fetch that will fail (no
	// orchestrator FetchImages executor wired), which is fine: the assertion
	// only concerns whether the MBID was adopted, stamped, and recorded before
	// that later failure. Since a.ThumbExists/FanartExists/LogoExists are all
	// false, fetchImages proceeds past the self-heal into FetchImages, which
	// with no registered image provider returns an empty result and yields
	// BulkItemSkipped "no suitable images found" -- an outcome distinct from
	// what this test is pinning, which is the adoption side effect below.
	_, _ = e.fetchImages(context.Background(), a, BulkModeYOLO, nil)

	if a.MusicBrainzID != strings.ToLower(bulkMBIDConfident) {
		t.Fatalf("a.MusicBrainzID = %q, want %q", a.MusicBrainzID, strings.ToLower(bulkMBIDConfident))
	}
	if got := a.MetadataSources[artist.SourceKeyMusicBrainzID]; got != artist.SourceMachinePicked {
		t.Errorf("MetadataSources[musicbrainz_id] = %q, want %q", got, artist.SourceMachinePicked)
	}

	reloaded, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("re-reading artist: %v", err)
	}
	if reloaded.MusicBrainzID != strings.ToLower(bulkMBIDConfident) {
		t.Fatalf("persisted a.MusicBrainzID = %q, want %q", reloaded.MusicBrainzID, strings.ToLower(bulkMBIDConfident))
	}

	changes, _, err := historySvc.List(context.Background(), a.ID, 50, 0)
	if err != nil {
		t.Fatalf("listing history: %v", err)
	}
	var found bool
	for _, c := range changes {
		if c.Field != "musicbrainz_id" {
			continue
		}
		found = true
		if c.Source != "rule:bulk_fetch_images_mbid" {
			t.Errorf("history source = %q, want %q", c.Source, "rule:bulk_fetch_images_mbid")
		}
		if c.OldValue != "" {
			t.Errorf("history old_value = %q, want empty (fill-only path)", c.OldValue)
		}
		if c.NewValue != strings.ToLower(bulkMBIDConfident) {
			t.Errorf("history new_value = %q, want %q", c.NewValue, strings.ToLower(bulkMBIDConfident))
		}
	}
	if !found {
		t.Fatal("no musicbrainz_id history row recorded for the adopted MBID")
	}
}

// TestFetchImages_MBIDGate_NormalizesCaseOnPersist pins the
// strings.ToLower(strings.TrimSpace(...)) normalization at the adopt site: the
// PERSISTED value (re-read via GetByID, not the in-memory struct, so a bug
// that normalizes only the local variable cannot pass vacuously) must be
// lowercase even when the provider returned uppercase hex.
//
// This does NOT exercise the TrimSpace half: artist.IsValidMBID requires an
// exact 36-character UUID, so a value with surrounding whitespace fails that
// check inside artist.BestMBIDCandidates before ever becoming a candidate --
// it cannot reach the normalization call through this path at all (the same
// fact fixers.go's own fixMBID documents at its equivalent trim site). Only
// the case-folding half is reachable and is what this test pins.
func TestFetchImages_MBIDGate_NormalizesCaseOnPersist(t *testing.T) {
	upperMBID := strings.ToUpper(bulkMBIDConfident)
	if !artist.IsValidMBID(upperMBID) {
		t.Fatalf("fixture precondition: uppercase fixture MBID must still be syntactically valid")
	}
	if upperMBID == bulkMBIDConfident {
		t.Fatalf("fixture precondition: uppercasing must actually change the fixture (it contains hex letters)")
	}

	results := []provider.ArtistSearchResult{
		{Name: "Radiohead", MusicBrainzID: upperMBID, Score: 100, Source: "musicbrainz"},
	}
	e, artistSvc, _, a := newBulkGateExecutor(t, results)

	_, _ = e.fetchImages(context.Background(), a, BulkModeYOLO, nil)

	reloaded, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("re-reading artist: %v", err)
	}
	want := strings.ToLower(bulkMBIDConfident)
	if reloaded.MusicBrainzID != want {
		t.Fatalf("persisted a.MusicBrainzID = %q, want %q (lowercased): "+
			"MusicBrainz IDs are case-sensitive lookup keys elsewhere in the tree",
			reloaded.MusicBrainzID, want)
	}
}

// failingRecordHistoryRepo is an artist.HistoryRepository whose Record always
// fails. Every other method is unimplemented (nil embed) because
// recordBulkMBIDHistory's only call into HistoryRepository is Record.
type failingRecordHistoryRepo struct {
	artist.HistoryRepository
	calls int
}

var errBulkForcedHistoryWrite = errors.New("forced history write failure")

func (r *failingRecordHistoryRepo) Record(_ context.Context, _ *artist.MetadataChange) error {
	r.calls++
	return errBulkForcedHistoryWrite
}

// TestFetchImages_MBIDGate_HistoryFailureDoesNotFailAdoption is issue
// #2845's stated acceptance criterion for recordBulkMBIDHistory: a failed
// history write is best-effort and must never fail the underlying MBID
// adoption it is auditing (mirrors recordRuleFixHistory's contract in
// fixer.go). Without this test, nothing pins that contract -- only the
// doc comment above recordBulkMBIDHistory asserts it, and an edit that made
// a Record failure propagate would still pass every other test in this file.
func TestFetchImages_MBIDGate_HistoryFailureDoesNotFailAdoption(t *testing.T) {
	results := []provider.ArtistSearchResult{
		{Name: "Radiohead", MusicBrainzID: bulkMBIDConfident, Score: 100, Source: "musicbrainz"},
	}
	e, artistSvc, _, a := newBulkGateExecutor(t, results)

	failingRepo := &failingRecordHistoryRepo{}
	e.SetHistoryService(artist.NewHistoryServiceWithRepo(failingRepo))

	status, msg := e.fetchImages(context.Background(), a, BulkModeYOLO, nil)

	// Precondition: the failing stub was actually invoked and actually
	// returned its error. Without this, a stub that fetchImages never called
	// (e.g. because the adopt path was skipped for an unrelated reason) would
	// pass this test vacuously.
	if failingRepo.calls != 1 {
		t.Fatalf("failingRecordHistoryRepo.Record called %d times, want 1 -- "+
			"the adopt path was not exercised, so this test proves nothing", failingRepo.calls)
	}

	// The load-bearing claim: a failed AUDIT write must not fail the
	// underlying IDENTITY write. BulkItemFailed would mean the history
	// failure propagated and aborted the operation it was only supposed to
	// record.
	if status == BulkItemFailed {
		t.Fatalf("status = %q, want anything but %q (message: %q): "+
			"a failed history write must not fail the bulk item outcome",
			status, BulkItemFailed, msg)
	}

	// The identity write itself must still have succeeded and persisted,
	// proving the history failure was contained to the audit trail alone.
	if a.MusicBrainzID != strings.ToLower(bulkMBIDConfident) {
		t.Fatalf("a.MusicBrainzID = %q, want %q: the MBID adoption must survive a history-write failure",
			a.MusicBrainzID, strings.ToLower(bulkMBIDConfident))
	}
	reloaded, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("re-reading artist: %v", err)
	}
	if reloaded.MusicBrainzID != strings.ToLower(bulkMBIDConfident) {
		t.Fatalf("persisted a.MusicBrainzID = %q, want %q", reloaded.MusicBrainzID, strings.ToLower(bulkMBIDConfident))
	}
}

// TestFetchImages_MBIDGate_UpdateErrorSurfaces proves a failing
// artistService.Update on the self-heal write surfaces as BulkItemFailed
// rather than silently reading as success -- the discarded-error defect
// named in issue #2825/#2845.
type failingUpdateArtistRepo struct {
	artist.Repository
}

var errBulkForcedUpdate = errors.New("forced update failure")

func (r *failingUpdateArtistRepo) Update(_ context.Context, _ *artist.Artist) error {
	return errBulkForcedUpdate
}

func TestFetchImages_MBIDGate_UpdateErrorSurfaces(t *testing.T) {
	db := setupTestDB(t)
	realArtists, providers, members, aliases, images, platformIDs, completeness := artist.NewDefaultRepos(db)
	artistSvc := artist.NewServiceWithRepos(&failingUpdateArtistRepo{Repository: realArtists}, providers, members, aliases, images, platformIDs, completeness)

	// Seed the row directly through the real repo so Create (which is not
	// overridden) succeeds; only Update is forced to fail.
	seedSvc := artist.NewServiceWithRepos(realArtists, providers, members, aliases, images, platformIDs, completeness)
	a := &artist.Artist{Name: "Radiohead", SortName: "Radiohead", Path: t.TempDir()}
	if err := seedSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("seeding artist: %v", err)
	}

	results := []provider.ArtistSearchResult{
		{Name: "Radiohead", MusicBrainzID: bulkMBIDConfident, Score: 100, Source: "musicbrainz"},
	}
	orch := newBulkGateOrchestrator(t, results)
	// A REAL history service over the same test DB, so a wrong ordering
	// (recording the history row BEFORE Update, rather than after) can
	// actually leave a row behind for this test to catch. A nil history
	// service would make TestFetchImages_MBIDGate_UpdateErrorSurfaces blind to
	// that ordering bug: recordBulkMBIDHistory's own nil guard would no-op
	// either way, so the ordering mistake and the correct ordering would look
	// identical without a real service wired in.
	historySvc := artist.NewHistoryService(db)
	e := &BulkExecutor{
		artistService: artistSvc,
		orchestrator:  orch,
		logger:        testLogger(),
	}
	e.SetHistoryService(historySvc)

	// Precondition: the history service is actually installed. Without this,
	// the "no history row" assertion below would pass vacuously against an
	// implementation that never wired history at all, which proves nothing
	// about write ordering.
	if e.getHistoryService() == nil {
		t.Fatal("fixture precondition: history service was not installed on the executor")
	}

	status, msg := e.fetchImages(context.Background(), a, BulkModeYOLO, nil)

	if status != BulkItemFailed {
		t.Fatalf("status = %q, want %q (message: %q)", status, BulkItemFailed, msg)
	}
	if !strings.Contains(msg, "update failed") {
		t.Errorf("message %q does not surface the update failure", msg)
	}

	// The load-bearing claim: when the identity write FAILS, no audit row may
	// exist claiming an MBID was assigned. A history-before-Update ordering
	// bug would leave exactly such a lying row behind.
	changes, _, err := historySvc.List(context.Background(), a.ID, 50, 0)
	if err != nil {
		t.Fatalf("listing history: %v", err)
	}
	for _, c := range changes {
		if c.Field == "musicbrainz_id" {
			t.Errorf("unexpected musicbrainz_id history row after a failed Update: %+v -- "+
				"the audit trail must never claim an assignment that did not actually persist", c)
		}
	}
}
