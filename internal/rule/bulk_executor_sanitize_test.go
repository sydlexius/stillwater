package rule

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/provider"
	_ "modernc.org/sqlite"
)

// Issue #2881: BulkExecutor failure paths interpolated the raw driver/provider
// error directly into the operator-facing (status, message) tuple, which
// bulk_service.go persists verbatim to bulk_job_items.message /
// bulk_jobs.error and internal/api/handlers_rule.go marshals with no
// redaction. A SQLite or schema-level error string therefore reached an
// operator surface unfiltered.
//
// These tests pin the two-audience split at each failure site: the returned
// message must NOT contain the raw driver error text, while the executor's
// slog output MUST still carry it (plus enough context -- artist id/name,
// operation -- to diagnose it). Each test injects a sentinel error whose text
// ("db is locked: injected") could never appear in a legitimate sanitized
// message, so a mutant that reinstates %v interpolation is caught by the
// "message must not contain" assertion.

const bulkSanitizeSentinelErr = "db is locked: injected"

// sanitizeCapturingExecutor builds a BulkExecutor whose logger writes JSON
// records into a buffer, so a test can assert on both the returned operator
// message and the server-side log in one call.
func sanitizeCapturingExecutor(t *testing.T, artistSvc *artist.Service, orch *provider.Orchestrator) (*BulkExecutor, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &BulkExecutor{
		artistService: artistSvc,
		orchestrator:  orch,
		logger:        logger,
	}, buf
}

// failingUpdateOnlyRepo forces artistService.Update to fail with a
// driver-shaped sentinel error, leaving every other repo method untouched.
type failingUpdateOnlyRepo struct {
	artist.Repository
}

func (r *failingUpdateOnlyRepo) Update(_ context.Context, _ *artist.Artist) error {
	return errors.New(bulkSanitizeSentinelErr)
}

// sanitizeTestArtistService seeds a real artist row (via the unwrapped repo,
// so Create succeeds) then returns a service whose Update always fails with
// the sentinel driver error.
func sanitizeTestArtistService(t *testing.T) (*artist.Service, *artist.Artist) {
	t.Helper()
	db := setupTestDB(t)
	realArtists, providers, members, aliases, images, platformIDs, completeness := artist.NewDefaultRepos(db)

	seedSvc := artist.NewServiceWithRepos(realArtists, providers, members, aliases, images, platformIDs, completeness)
	a := &artist.Artist{Name: "Sanitize Target", SortName: "Sanitize Target", Path: t.TempDir()}
	if err := seedSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("seeding artist: %v", err)
	}

	failingSvc := artist.NewServiceWithRepos(&failingUpdateOnlyRepo{Repository: realArtists}, providers, members, aliases, images, platformIDs, completeness)
	return failingSvc, a
}

// assertSanitized is the shared two-audience assertion: the operator message
// must not leak the driver error, must still say something specific and
// actionable, and the raw error must reach the log.
func assertSanitized(t *testing.T, status, message string, buf *bytes.Buffer, wantMessageSubstr string) {
	t.Helper()
	if status != BulkItemFailed {
		t.Fatalf("status = %q, want %q", status, BulkItemFailed)
	}
	if strings.Contains(message, bulkSanitizeSentinelErr) {
		t.Errorf("operator message %q leaks the raw driver error %q", message, bulkSanitizeSentinelErr)
	}
	if wantMessageSubstr != "" && !strings.Contains(message, wantMessageSubstr) {
		t.Errorf("operator message %q does not contain expected sanitized text %q", message, wantMessageSubstr)
	}
	logged := buf.String()
	if !strings.Contains(logged, bulkSanitizeSentinelErr) {
		t.Errorf("server log does not contain the raw driver error %q -- diagnosability lost. Log:\n%s", bulkSanitizeSentinelErr, logged)
	}
}

// TestFetchMetadata_UpdateFailure_SanitizesOperatorMessage reproduces and pins
// the fix for the update-failure site in fetchMetadata.
func TestFetchMetadata_UpdateFailure_SanitizesOperatorMessage(t *testing.T) {
	artistSvc, a := sanitizeTestArtistService(t)

	result := &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			Name:      a.Name,
			Biography: "a fresh biography that will trigger an update",
		},
		AttemptedProviders: []provider.ProviderName{provider.NameMusicBrainz},
	}
	orch := provider.NewOrchestrator(nil, nil, testLogger(), nil)
	orch.SetExecutor(&stubScrapeAll{result: result})

	e, buf := sanitizeCapturingExecutor(t, artistSvc, orch)

	status, message := e.fetchMetadata(context.Background(), a, BulkModeYOLO)

	assertSanitized(t, status, message, buf, "")
}

// sanitizeImageProvider is a minimal provider.Provider whose GetImages
// returns a single fanart candidate pointing at an httptest server, so
// fetchImages's saveBestImage step has something real to download and save
// before the (forced-to-fail) artistService.Update runs.
type sanitizeImageProvider struct {
	name provider.ProviderName
	url  string
}

func (m *sanitizeImageProvider) Name() provider.ProviderName { return m.name }
func (m *sanitizeImageProvider) RequiresAuth() bool          { return false }
func (m *sanitizeImageProvider) SearchArtist(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
	return nil, nil
}
func (m *sanitizeImageProvider) GetArtist(_ context.Context, _ string) (*provider.ArtistMetadata, error) {
	return nil, nil
}
func (m *sanitizeImageProvider) GetImages(_ context.Context, _ string) ([]provider.ImageResult, error) {
	return []provider.ImageResult{{URL: m.url, Type: provider.ImageFanart, Width: 800, Height: 600, Source: "test"}}, nil
}

// TestFetchImages_SaveUpdateFailure_SanitizesOperatorMessage reproduces and
// pins the fix for the post-save update-failure site in fetchImages: an
// image is fetched and saved to disk successfully, but persisting the
// updated artist row fails with a driver-shaped error.
func TestFetchImages_SaveUpdateFailure_SanitizesOperatorMessage(t *testing.T) {
	artistSvc, a := sanitizeTestArtistService(t)
	a.MusicBrainzID = "a74b1b7f-71a5-4011-9441-d0b5e4122711"
	a.ThumbExists = true
	a.FanartExists = false
	a.LogoExists = true

	testJPEG := makeTestJPEG(t, 800, 600)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(testJPEG)
	}))
	t.Cleanup(srv.Close)

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening settings db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(context.Background(), `CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TEXT NOT NULL DEFAULT (datetime('now')))`); err != nil {
		t.Fatalf("creating settings table: %v", err)
	}
	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	settings := provider.NewSettingsService(db, enc)
	registry := provider.NewRegistry()
	registry.Register(&sanitizeImageProvider{name: provider.NameMusicBrainz, url: srv.URL + "/backdrop.jpg"})
	orchLogger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := provider.NewOrchestrator(registry, settings, orchLogger, nil)

	e, buf := sanitizeCapturingExecutor(t, artistSvc, orch)
	// Plain client: the httptest fixture binds to 127.0.0.1, which the
	// default SSRF-safe transport blocks (mirrors bulk_executor_test.go).
	e.httpClient = &http.Client{Timeout: fetchTimeout}

	status, message := e.fetchImages(context.Background(), a, BulkModeYOLO, nil)

	assertSanitized(t, status, message, buf, "saving fetched images failed")
}

// failingListRepo forces artistService.List to fail with a driver-shaped
// sentinel error, so run()'s "listing artists" failure path can be reached
// without a real SQLite error.
type failingListRepo struct {
	artist.Repository
}

func (r *failingListRepo) List(_ context.Context, _ artist.ListParams) ([]artist.Artist, int, error) {
	return nil, 0, errors.New(bulkSanitizeSentinelErr)
}

// TestBulkRun_ListArtistsFailure_SanitizesJobError reproduces and pins the fix
// for the job-level "listing artists" failure site in run(): BulkJob.Error is
// persisted and returned through the API exactly like BulkJobItem.Message, so
// it must not carry the raw driver error either.
func TestBulkRun_ListArtistsFailure_SanitizesJobError(t *testing.T) {
	db := setupTestDB(t)
	realArtists, providers, members, aliases, images, platformIDs, completeness := artist.NewDefaultRepos(db)
	artistSvc := artist.NewServiceWithRepos(&failingListRepo{Repository: realArtists}, providers, members, aliases, images, platformIDs, completeness)

	bulkSvc := NewBulkService(db)
	job, err := bulkSvc.CreateJob(context.Background(), "unrecognized_type_for_test", BulkModeYOLO, 0)
	if err != nil {
		t.Fatalf("creating bulk job: %v", err)
	}
	// No ArtistIDs: run() takes the paginated artist.List(...) branch that
	// reaches the failure site under test.

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e := &BulkExecutor{
		bulkService:   bulkSvc,
		artistService: artistSvc,
		logger:        logger,
	}

	e.run(context.Background(), job)

	if job.Status != BulkStatusFailed {
		t.Fatalf("job.Status = %q, want %q", job.Status, BulkStatusFailed)
	}
	if strings.Contains(job.Error, bulkSanitizeSentinelErr) {
		t.Errorf("job.Error %q leaks the raw driver error %q", job.Error, bulkSanitizeSentinelErr)
	}
	if !strings.Contains(job.Error, "failed to list artists") {
		t.Errorf("job.Error %q does not contain the expected sanitized text", job.Error)
	}
	logged := buf.String()
	if !strings.Contains(logged, bulkSanitizeSentinelErr) {
		t.Errorf("server log does not contain the raw driver error %q -- diagnosability lost. Log:\n%s", bulkSanitizeSentinelErr, logged)
	}
}

// cancelAfterNErrChecks wraps a real, never-actually-canceled context
// (context.Background(), whose Done() channel is nil so no SQL query it
// backs ever observes a cancellation) and makes Err() itself report
// context.Canceled starting from its (n+1)th call. run()'s loop calls
// ctx.Err() exactly once per top-of-loop check, so triggerAfter=1 makes the
// FIRST iteration's check pass (proceeding to a real, successful List call)
// and the SECOND iteration's check trip the cancellation branch under test
// -- reaching it without ever canceling a real in-flight query, which would
// otherwise also fail the batch-hydrate steps inside artist.Service.List and
// mask the site this test targets.
type cancelAfterNErrChecks struct {
	context.Context
	calls        int32
	triggerAfter int32
}

func (c *cancelAfterNErrChecks) Err() error {
	n := atomic.AddInt32(&c.calls, 1)
	if n > c.triggerAfter {
		return context.Canceled
	}
	return nil
}

// TestBulkRun_CanceledWhileListing_SanitizesJobError reproduces and pins the
// fix for run()'s cancellation-while-listing site: BulkJob.Error must not
// carry the raw context error text (ctx.Err() itself, e.g. "context
// canceled", is not driver-shaped, but the site used the same %v-into-message
// pattern the rest of the audit removed, so it is covered here too).
func TestBulkRun_CanceledWhileListing_SanitizesJobError(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)

	// Exactly pageSize (200, run()'s local const) rows, so the first List
	// call returns a full page and the loop takes a second turn instead of
	// breaking on "page shorter than pageSize" -- that second turn is what
	// reaches the cancellation check under test.
	for i := 0; i < 200; i++ {
		name := fmt.Sprintf("Cancel Page Artist %03d", i)
		a := &artist.Artist{Name: name, SortName: name, Path: t.TempDir()}
		if err := artistSvc.Create(context.Background(), a); err != nil {
			t.Fatalf("seeding artist %d: %v", i, err)
		}
	}

	bulkSvc := NewBulkService(db)
	job, err := bulkSvc.CreateJob(context.Background(), "unrecognized_type_for_test", BulkModeYOLO, 0)
	if err != nil {
		t.Fatalf("creating bulk job: %v", err)
	}

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e := &BulkExecutor{
		bulkService:   bulkSvc,
		artistService: artistSvc,
		logger:        logger,
	}

	ctx := &cancelAfterNErrChecks{Context: context.Background(), triggerAfter: 1}

	e.run(ctx, job)

	if job.Status != BulkStatusCanceled {
		t.Fatalf("job.Status = %q, want %q (job.Error = %q, log = %s)", job.Status, BulkStatusCanceled, job.Error, buf.String())
	}
	if job.Error != "bulk job canceled" {
		t.Errorf("job.Error = %q, want the stable static message %q", job.Error, "bulk job canceled")
	}
	logged := buf.String()
	if !strings.Contains(logged, "job_id") || !strings.Contains(logged, job.ID) {
		t.Errorf("server log does not name the job_id %q -- diagnosability lost. Log:\n%s", job.ID, logged)
	}
}

// failingScrapeAll is a provider.ScraperExecutor that always fails with the
// sentinel driver-shaped error, so fetchMetadata's own FetchMetadata call
// (not the subsequent Update) can be exercised.
type failingScrapeAll struct{}

func (failingScrapeAll) ScrapeAll(_ context.Context, _, _, _ string, _ map[provider.ProviderName]string) (*provider.FetchResult, error) {
	return nil, errors.New(bulkSanitizeSentinelErr)
}

// TestFetchMetadata_FetchFailure_SanitizesOperatorMessage reproduces and pins
// the fix for the provider-fetch failure site in fetchMetadata (the first of
// its two failure sites, before any Update is attempted).
func TestFetchMetadata_FetchFailure_SanitizesOperatorMessage(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	a := &artist.Artist{Name: "Fetch Failure Target", SortName: "Fetch Failure Target", Path: t.TempDir()}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("seeding artist: %v", err)
	}

	orch := provider.NewOrchestrator(nil, nil, testLogger(), nil)
	orch.SetExecutor(failingScrapeAll{})

	e, buf := sanitizeCapturingExecutor(t, artistSvc, orch)

	status, message := e.fetchMetadata(context.Background(), a, BulkModeYOLO)

	assertSanitized(t, status, message, buf, "metadata fetch from providers failed")
}

// TestFetchImages_FetchFailure_SanitizesOperatorMessage reproduces and pins
// the fix for the provider-fetch failure site in fetchImages (before any
// image is downloaded or saved). The artist already carries a valid MBID so
// the self-heal branch is never entered; the failure comes from
// imageProvidersInPriorityOrder erroring on a closed settings DB, mirroring
// searchFailureExecutor's technique in bulk_executor_search_error_log_test.go.
func TestFetchImages_FetchFailure_SanitizesOperatorMessage(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	a := &artist.Artist{
		Name:          "Image Fetch Failure Target",
		SortName:      "Image Fetch Failure Target",
		MusicBrainzID: "a74b1b7f-71a5-4011-9441-d0b5e4122711",
		Path:          t.TempDir(),
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("seeding artist: %v", err)
	}

	settingsDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening settings db: %v", err)
	}
	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	settings := provider.NewSettingsService(settingsDB, enc)
	registry := provider.NewRegistry()
	registry.Register(&mockSearchProvider{name: provider.NameMusicBrainz})
	// Closing the settings DB makes imageProvidersInPriorityOrder (via
	// AvailableProviderNames) return a real error rather than an empty list.
	if err := settingsDB.Close(); err != nil {
		t.Fatalf("closing settings db: %v", err)
	}

	orchLogger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	orch := provider.NewOrchestrator(registry, settings, orchLogger, nil)

	e, buf := sanitizeCapturingExecutor(t, artistSvc, orch)

	status, message := e.fetchImages(context.Background(), a, BulkModeYOLO, nil)

	// This site's error comes from the closed settings DB (via
	// imageProvidersInPriorityOrder), not the sentinel driver error the other
	// sanitize tests inject, so it is asserted directly rather than through
	// assertSanitized's sentinel-specific checks.
	if status != BulkItemFailed {
		t.Fatalf("status = %q, want %q", status, BulkItemFailed)
	}
	if !strings.Contains(message, "image fetch from providers failed") {
		t.Errorf("operator message %q does not contain expected sanitized text", message)
	}
	if strings.Contains(message, "database is closed") {
		t.Errorf("operator message %q leaks the raw driver error", message)
	}
	logged := buf.String()
	if !strings.Contains(logged, "database is closed") {
		t.Errorf("server log does not contain the raw driver error -- diagnosability lost. Log:\n%s", logged)
	}
}
