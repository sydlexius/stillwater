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
//
// wantMessageSubstr is REQUIRED. An earlier version skipped the check when it
// was empty, and one caller used that bypass -- leaving a case that asserted
// only the ABSENCE of the raw error, so an implementation returning an empty
// or generic message would have passed. "Does not leak" is half the contract;
// "still says which step failed" is the other half, and a sanitizer meeting
// only the first is useless to an operator.
//
// wantArtist/wantOperation pin the LOG's context: the raw error alone is not
// diagnosable if the line does not say which artist and which step produced it.
func assertSanitized(t *testing.T, status, message string, buf *bytes.Buffer, wantMessageSubstr string, wantArtist *artist.Artist, wantOperation string) {
	t.Helper()
	if wantMessageSubstr == "" {
		t.Fatal("assertSanitized requires a non-empty wantMessageSubstr: without it the case " +
			"cannot distinguish a correct sanitized message from an empty one")
	}
	if status != BulkItemFailed {
		t.Fatalf("status = %q, want %q", status, BulkItemFailed)
	}
	if strings.Contains(message, bulkSanitizeSentinelErr) {
		t.Errorf("operator message %q leaks the raw driver error %q", message, bulkSanitizeSentinelErr)
	}
	if !strings.Contains(message, wantMessageSubstr) {
		t.Errorf("operator message %q does not contain expected sanitized text %q", message, wantMessageSubstr)
	}
	logged := buf.String()
	if !strings.Contains(logged, bulkSanitizeSentinelErr) {
		t.Errorf("server log does not contain the raw driver error %q -- diagnosability lost. Log:\n%s", bulkSanitizeSentinelErr, logged)
	}
	if wantArtist != nil {
		if !strings.Contains(logged, wantArtist.ID) {
			t.Errorf("log omits the artist id %q, so the failure cannot be traced to a record. Log:\n%s", wantArtist.ID, logged)
		}
		if !strings.Contains(logged, wantArtist.Name) {
			t.Errorf("log omits the artist name %q. Log:\n%s", wantArtist.Name, logged)
		}
	}
	if wantOperation != "" && !strings.Contains(logged, wantOperation) {
		t.Errorf("log omits the operation %q, so the failing step is unidentifiable. Log:\n%s", wantOperation, logged)
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

	assertSanitized(t, status, message, buf, "saving fetched metadata failed", a, BulkTypeFetchMetadata)
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

	assertSanitized(t, status, message, buf, "saving fetched images failed", a, BulkTypeFetchImages)
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

	assertSanitized(t, status, message, buf, "metadata fetch from providers failed", a, BulkTypeFetchMetadata)
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

// TestRedactURLQueries covers the URL-leak finding on #2960. Go's http.Client
// embeds the FULL requested URL, query string included, in its transport
// errors -- verified: a failed request to `https://host/p?token=SECRET` yields
// `Get "https://host/p?token=SECRET": dial tcp ...`. Image downloads use
// provider URLs that can be SIGNED, so the credential lives in that query
// string, and itemFailure writes the raw error text straight to the log, which
// is a lower-privilege surface than the request that produced it.
//
// The last two cases guard the opposite failure: over-redaction that would
// destroy the diagnostic value the log is kept for.
func TestRedactURLQueries(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "signed url in a transport error",
			in:   `Get "https://cdn.example/img/a.jpg?token=SIGNED_SECRET&exp=99": dial tcp: connection refused`,
			want: `Get "https://cdn.example/img/a.jpg?<redacted>": dial tcp: connection refused`,
		},
		{
			name: "the path survives, so the failure stays diagnosable",
			in:   `fetching https://fanart.tv/artist/mbid/bg.png?api_key=abc123 failed`,
			want: `fetching https://fanart.tv/artist/mbid/bg.png?<redacted> failed`,
		},
		{
			name: "two urls in one message",
			in:   `redirect https://a.example/x?k=1 -> https://b.example/y?k=2`,
			want: `redirect https://a.example/x?<redacted> -> https://b.example/y?<redacted>`,
		},
		{
			name: "a url with no query is untouched",
			in:   `Get "https://cdn.example/img/a.jpg": timeout`,
			want: `Get "https://cdn.example/img/a.jpg": timeout`,
		},
		{
			name: "an ordinary question mark is not a url",
			in:   `could not determine the format. is it an image?`,
			want: `could not determine the format. is it an image?`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactURLQueries(tc.in)
			if got != tc.want {
				t.Errorf("redactURLQueries(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
			for _, secret := range []string{"SIGNED_SECRET", "api_key=abc123"} {
				if strings.Contains(tc.in, secret) && strings.Contains(got, secret) {
					t.Errorf("the credential %q survived redaction: %q", secret, got)
				}
			}
		})
	}
}

// TestFinishJob_PersistsUnderACanceledContext pins a real production bug found
// in review: finishJob passed the CALLER's context to UpdateJob, and its most
// important caller is the cancellation path -- where that context is already
// canceled. The write therefore failed and the row kept its previous status
// while the in-memory struct said "canceled". An operator cancels a bulk job
// and watches it hang forever.
//
// Measured before the fix: in-memory "canceled", PERSISTED "pending".
//
// This is tested directly rather than through run(): reaching the same state
// via run() needs a context that is canceled for the loop check but NOT for
// the SQL underneath it (see cancelAfterNErrChecks above, whose Done() never
// closes for exactly that reason), which is precisely the condition that hides
// this bug. Driving finishJob with a genuinely canceled context is the only
// way to observe it.
func TestFinishJob_PersistsUnderACanceledContext(t *testing.T) {
	db := setupTestDB(t)
	bulkSvc := NewBulkService(db)
	job, err := bulkSvc.CreateJob(context.Background(), "cancel_persist_test", BulkModeYOLO, 0)
	if err != nil {
		t.Fatalf("creating bulk job: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled, exactly as run()'s cancellation branch sees it

	e := &BulkExecutor{bulkService: bulkSvc, logger: testLogger()}
	e.finishJob(ctx, job, BulkStatusCanceled, "bulk job canceled")

	// ASSERT THE ROW, not the struct. The struct is what the code just
	// assigned; the row is what the operator sees.
	reloaded, err := bulkSvc.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("reloading the job: %v", err)
	}
	if reloaded.Status != BulkStatusCanceled {
		t.Errorf("PERSISTED status = %q, want %q -- the terminal state never reached the database, "+
			"so the job appears to hang forever", reloaded.Status, BulkStatusCanceled)
	}
	if reloaded.Error != "bulk job canceled" {
		t.Errorf("PERSISTED error = %q, want %q", reloaded.Error, "bulk job canceled")
	}
}

// TestItemFailure_RedactsURLsInTheLog pins that itemFailure actually CALLS
// redactURLQueries. Testing the function alone leaves the wiring unasserted --
// a perfect redactor nothing invokes redacts nothing, and dropping the call
// site is a one-character edit that the unit test above cannot see (measured:
// that mutation survived until this case existed).
func TestItemFailure_RedactsURLsInTheLog(t *testing.T) {
	buf := &bytes.Buffer{}
	e := &BulkExecutor{logger: slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	a := &artist.Artist{ID: "artist-id-1", Name: "Redact Target"}

	// Shaped exactly like a real http.Client transport error, which embeds the
	// full requested URL including its query string.
	err := errors.New(`Get "https://cdn.example/img/a.jpg?token=SIGNED_SECRET_TOKEN": dial tcp: connection refused`)
	status, message := e.itemFailure(a, BulkTypeFetchImages, "image fetch from providers failed; retry later", err)

	if status != BulkItemFailed {
		t.Fatalf("status = %q, want %q", status, BulkItemFailed)
	}
	logged := buf.String()
	if strings.Contains(logged, "SIGNED_SECRET_TOKEN") {
		t.Errorf("the signed token reached the log -- itemFailure is not redacting. Log:\n%s", logged)
	}
	if !strings.Contains(logged, "<redacted>") {
		t.Errorf("the log shows no redaction marker, so the URL was not passed through the redactor. Log:\n%s", logged)
	}
	// Over-redaction check: the path must survive, or the log stops being
	// diagnostic.
	if !strings.Contains(logged, "/img/a.jpg") {
		t.Errorf("the URL path was destroyed along with the query. Log:\n%s", logged)
	}
	if strings.Contains(message, "SIGNED_SECRET_TOKEN") {
		t.Errorf("the operator message leaks the token: %q", message)
	}
}
