package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/dupimages"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/publish"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/web/templates"
)

// failingArtistLister makes every sweep fail at its first step, which is how a
// test asks for a publisher that cannot sweep without also asking the
// publisher to be half-wired.
type failingArtistLister struct{}

func (failingArtistLister) List(context.Context, artist.ListParams) ([]artist.Artist, int, error) {
	return nil, 0, errors.New("test: artist listing unavailable")
}

// platformArtistLister is the paged-listing dependency
// ScanPlatformBackdropDuplicates walks the library through
// (publish.Deps.ArtistLister). Spelled out here so a test can hand the
// publisher something other than the real artist service -- a lister that
// parks, or none at all.
type platformArtistLister interface {
	List(ctx context.Context, params artist.ListParams) ([]artist.Artist, int, error)
}

// testRouterWithPlatformPublisher builds a Router with a real
// *publish.Publisher whose ArtistLister is the real artist service, so
// ScanPlatformBackdropDuplicates is fully wired and can be exercised end to
// end. Static assets are wired so assetsFor/renderTempl work for full-page
// renders.
func testRouterWithPlatformPublisher(t *testing.T) *Router {
	t.Helper()
	return testRouterWithPlatformLister(t, nil)
}

// testRouterWithPlatformLister is testRouterWithPlatformPublisher against an
// explicit lister, which is how a test controls how long the platform sweep
// takes (a parked lister) or makes it fail outright.
//
// A nil lister means "use the real artist service".
func testRouterWithPlatformLister(t *testing.T, lister platformArtistLister) *Router {
	t.Helper()

	db := newTestDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}

	authSvc := auth.NewService(db)
	artistSvc := artist.NewService(db)
	connSvc := connection.NewService(db, enc)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	if lister == nil {
		lister = artistSvc
	}

	pub := publish.New(publish.Deps{
		ArtistService:     artistSvc,
		ArtistLister:      lister,
		ConnectionService: connSvc,
		Logger:            logger,
	})

	return NewRouter(RouterDeps{
		SessionSecret:     testSessionSecret,
		AuthService:       authSvc,
		ArtistService:     artistSvc,
		ConnectionService: connSvc,
		RuleService:       ruleSvc,
		DB:                db,
		Logger:            logger,
		StaticFS:          os.DirFS("../../web/static"),
		Publisher:         pub,
	})
}

// TestPlatformBackdropDuplicatesPage_RequiresAdmin pins the admin gate: a
// non-admin GET must be refused (403 or a redirect), never the rendered
// report (#2540 Task 6).
func TestPlatformBackdropDuplicatesPage_RequiresAdmin(t *testing.T) {
	t.Parallel()
	r := testRouterWithPlatformPublisher(t)

	ctx := middleware.WithTestUserID(context.Background(), "u1")
	ctx = middleware.WithTestRole(ctx, "operator")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/reports/platform-backdrop-duplicates", nil)
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPage(w, req)

	if w.Code != http.StatusForbidden && w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 403/redirect for non-admin", w.Code)
	}
}

// TestPlatformBackdropDuplicatesPage_UnauthRendersLoginPage mirrors the
// sibling local-report test: an unauthenticated GET must render the login
// page (200), not a bare 401, because the route uses wrapOptionalAuth.
func TestPlatformBackdropDuplicatesPage_UnauthRendersLoginPage(t *testing.T) {
	t.Parallel()
	r := testRouterWithPlatformPublisher(t)

	req := withI18nCtx(t, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/reports/platform-backdrop-duplicates", nil))
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unauthenticated request should get login page (200), got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "platform-backdrop-duplicates-table") {
		t.Error("unauthenticated visitor must not see the platform-backdrop-duplicates table")
	}
	if !strings.Contains(body, "/api/v1/auth/login") {
		t.Error("login page must have the login form action (/api/v1/auth/login)")
	}
}

// TestPlatformBackdropDuplicatesPage_PublisherNilFailsLoud pins the fail-loud
// contract: r.publisher == nil must return 500 with a logged error, never a
// silent empty report.
func TestPlatformBackdropDuplicatesPage_PublisherNilFailsLoud(t *testing.T) {
	t.Parallel()
	r := testRouterWithPlatformPublisher(t)
	r.publisher = nil

	req := withI18nCtx(t, httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/reports/platform-backdrop-duplicates", nil))
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPage(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 when publisher is nil", w.Code)
	}
}

// Pins what a failing sweep now means to the RENDER path (#3092). This test
// used to assert a 500, because the handler swept inline and the sweep's error
// WAS the response's error. The handler no longer sweeps, so a broken publisher
// fails the BACKGROUND refresh instead (whose error handling is pinned in
// handlers_duplicate_images_nav_test.go).
//
// The page's remaining obligation: with nothing established it renders the
// pending notice, never a table of zeros claiming the platforms are clean.
//
// NOT t.Parallel(): the cold-cache branch reaches dupimages.Shared().
func TestPlatformBackdropDuplicatesPage_SweepFailureRendersPendingNot500(t *testing.T) {
	dupimages.Shared().Reset()
	t.Cleanup(func() { dupimages.Shared().Reset() })

	r := testRouterWithPlatformLister(t, failingArtistLister{})

	req := withI18nCtx(t, httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/reports/platform-backdrop-duplicates", nil))
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a failing background sweep must not fail the render; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="platform-backdrop-duplicates-unavailable-notice"`) {
		t.Errorf("a never-swept cache must render the pending notice; body: %s", body)
	}
	if strings.Contains(body, `id="platform-backdrop-duplicates-empty"`) {
		t.Error("a never-swept cache must not render the clean/empty state: nothing has established that the platforms are clean")
	}
}

// TestPlatformBackdropDuplicatesPage_AuthenticatedRendersPage is the
// authenticated-path regression test: with a swept-and-cached empty report, an
// admin request must render the report table's empty, clean state.
//
// The report is placed in the cache directly (r.storePlatformDupReport) rather
// than swept, because the render path no longer sweeps at all (#3092) -- a
// cache write is exactly what the background sweep leaves behind, and going
// through it here would test the sweep instead of the page.
func TestPlatformBackdropDuplicatesPage_AuthenticatedRendersPage(t *testing.T) {
	t.Parallel()
	r := testRouterWithPlatformPublisher(t)
	r.storePlatformDupReport(publish.PlatformBackdropDupReport{})

	req := withI18nCtx(t, httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/reports/platform-backdrop-duplicates", nil))
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("authenticated admin request should get 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "platform-backdrop-duplicates-table") {
		t.Error("authenticated admin must see the platform-backdrop-duplicates-table in the response")
	}
	// No artists means RedundantBackdrops == 0, so the prune button is
	// correctly withheld (mirrors the sibling report's ExactRedundantSlots
	// gate); assert the empty-state message renders instead.
	if !strings.Contains(body, `id="platform-backdrop-duplicates-empty"`) {
		t.Error("a clean, empty sweep must render the empty-state message")
	}
	// A cached report must say WHEN it was swept, so an operator can tell a
	// snapshot from a live number (#3092 AC-3).
	if !strings.Contains(body, `id="platform-backdrop-duplicates-as-of"`) {
		t.Errorf("a cached report must carry an as-of timestamp; body: %s", body)
	}
}

// TestPlatformBackdropDuplicatesPage_PruneButtonPostsToPruneEndpoint asserts
// that once the scan reports redundant backdrops, the prune button renders
// and posts to the Task 7 prune endpoint.
func TestPlatformBackdropDuplicatesPage_PruneButtonPostsToPruneEndpoint(t *testing.T) {
	t.Parallel()
	view := buildPlatformBackdropDuplicatesView(publish.PlatformBackdropDupReport{
		ConnectionsAffected: 1,
		ArtistsAffected:     1,
		RedundantBackdrops:  2,
		PerArtist: []publish.ArtistPlatformBackdropDup{
			{ArtistID: "a1", Name: "Artist One", ConnectionID: "c-emby", Connection: "emby", Backdrops: 4, Redundant: 2},
		},
	})

	req := withI18nCtx(t, httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/reports/platform-backdrop-duplicates", nil))
	rec := httptest.NewRecorder()
	renderTempl(rec, req, templates.PlatformBackdropDuplicatesPage(templates.AssetPaths{}, view))

	body := rec.Body.String()
	if !strings.Contains(body, `id="platform-backdrop-duplicates-prune-button"`) {
		t.Error("report must expose the Prune Platform Duplicates action when RedundantBackdrops > 0")
	}
	if !strings.Contains(body, "/api/v1/reports/platform-backdrop-duplicates/prune") {
		t.Error("prune button must post to /api/v1/reports/platform-backdrop-duplicates/prune")
	}
	if !strings.Contains(body, "Artist One") {
		t.Errorf("report table must list the affected artist; body: %s", body)
	}
}

// TestBuildPlatformBackdropDuplicatesView pins the report-to-view-model
// conversion: every PerArtist entry becomes a row and the totals pass
// through unchanged.
func TestBuildPlatformBackdropDuplicatesView(t *testing.T) {
	report := publish.PlatformBackdropDupReport{
		ConnectionsAffected: 1,
		ArtistsAffected:     1,
		RedundantBackdrops:  2,
		ScanErrors:          1,
		PerArtist: []publish.ArtistPlatformBackdropDup{
			{ArtistID: "a1", Name: "Artist One", ConnectionID: "c-emby", Connection: "emby", Backdrops: 4, Redundant: 2},
		},
	}
	view := buildPlatformBackdropDuplicatesView(report)

	if view.ConnectionsAffected != 1 || view.ArtistsAffected != 1 || view.RedundantBackdrops != 2 || view.ScanErrors != 1 {
		t.Fatalf("totals did not pass through: %+v", view)
	}
	if len(view.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(view.Rows))
	}
	row := view.Rows[0]
	if row.ArtistID != "a1" || row.Name != "Artist One" || row.Connection != "emby" || row.Backdrops != 4 || row.Redundant != 2 {
		t.Errorf("row mismatch: %+v", row)
	}
}

// keep an explicit reference to the templates package view type so a future
// refactor of the view struct's field set fails to compile here rather than
// silently drifting from the templ template's expectations.
var _ = templates.PlatformBackdropDuplicatesPageView{}

// TestPlatformBackdropDuplicatesPrune_ConflictWhenRunning asserts a request
// returns 409 while the singleton slot is already claimed, mirroring
// TestBackdropDuplicatesRemediate_ConflictWhenRunning (#2540 Task 7).
func TestPlatformBackdropDuplicatesPrune_ConflictWhenRunning(t *testing.T) {
	t.Parallel()
	r := testRouterWithPlatformPublisher(t)

	r.platformPruneMu.Lock()
	r.platformPruneRunning = true
	r.platformPruneMu.Unlock()

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/platform-backdrop-duplicates/prune", nil)
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPrune(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}

	// The rejected concurrent request must not clobber the real prune's claim
	// on the singleton slot: guards against a future defer-misplacement
	// regression that clears platformPruneRunning on the 409 path.
	r.platformPruneMu.Lock()
	stillRunning := r.platformPruneRunning
	r.platformPruneMu.Unlock()
	if !stillRunning {
		t.Fatal("platformPruneRunning = false after a rejected concurrent request, want true (still claimed by the running prune)")
	}
}

// TestPlatformBackdropDuplicatesPrune_NonAdminForbidden mirrors
// TestBackdropDuplicatesRemediate_NonAdminForbidden: the prune endpoint is
// admin-only via requireForeignAdmin, so an authenticated non-admin must get
// 403 rather than triggering the prune.
func TestPlatformBackdropDuplicatesPrune_NonAdminForbidden(t *testing.T) {
	t.Parallel()
	r := testRouterWithPlatformPublisher(t)

	ctx := middleware.WithTestUserID(context.Background(), "u1")
	ctx = middleware.WithTestRole(ctx, "operator")
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/reports/platform-backdrop-duplicates/prune", nil)
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPrune(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("non-admin should get 403; got %d", w.Code)
	}
}

// TestPlatformBackdropDuplicatesPrune_PublisherNilFailsLoud pins the
// fail-loud contract: r.publisher == nil must return 500, never a silent
// empty 200 (this repo forbids silent-failure capability guards).
func TestPlatformBackdropDuplicatesPrune_PublisherNilFailsLoud(t *testing.T) {
	t.Parallel()
	r := testRouterWithPlatformPublisher(t)
	r.publisher = nil

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/platform-backdrop-duplicates/prune", nil)
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPrune(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 when publisher is nil", w.Code)
	}
}

// TestPlatformBackdropDuplicatesPrune_Error pins the prune endpoint's error
// path: if PrunePlatformBackdropDuplicates fails (here, a Publisher wired
// without an ArtistLister, so it is not fully wired), the handler returns
// 500 and releases the singleton so a later request is not permanently
// blocked.
func TestPlatformBackdropDuplicatesPrune_Error(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	db := newTestDB(t)
	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	artistSvc := artist.NewService(db)
	connSvc := connection.NewService(db, enc)
	authSvc := auth.NewService(db)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	// Deliberately omit ArtistLister so the prune is not fully wired and
	// returns an error.
	pub := publish.New(publish.Deps{
		ArtistService:     artistSvc,
		ConnectionService: connSvc,
		Logger:            logger,
	})

	r := NewRouter(RouterDeps{
		SessionSecret:     testSessionSecret,
		AuthService:       authSvc,
		ArtistService:     artistSvc,
		ConnectionService: connSvc,
		RuleService:       ruleSvc,
		DB:                db,
		Logger:            logger,
		StaticFS:          os.DirFS("../../web/static"),
		Publisher:         pub,
	})

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/platform-backdrop-duplicates/prune", nil)
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPrune(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 when the prune errors; body: %s", w.Code, w.Body.String())
	}

	r.platformPruneMu.Lock()
	running := r.platformPruneRunning
	r.platformPruneMu.Unlock()
	if running {
		t.Error("platformPruneRunning must be released after a failed prune")
	}
}

// TestPlatformBackdropDuplicatesPrune_Success asserts the happy path: an
// admin POST against a fully wired publisher with no artists reaches
// PrunePlatformBackdropDuplicates and the JSON body reports its (empty)
// result.
func TestPlatformBackdropDuplicatesPrune_Success(t *testing.T) {
	t.Parallel()
	r := testRouterWithPlatformPublisher(t)

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/platform-backdrop-duplicates/prune", nil)
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPrune(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"artists_processed":0`) {
		t.Errorf("expected artists_processed = 0 in body: %s", body)
	}
	if !strings.Contains(body, `"backdrops_removed":0`) {
		t.Errorf("expected backdrops_removed = 0 in body: %s", body)
	}
	if !strings.Contains(body, `"skipped_changed":0`) {
		t.Errorf("expected skipped_changed = 0 in body: %s", body)
	}
	if !strings.Contains(body, `"failures":0`) {
		t.Errorf("expected failures = 0 in body: %s", body)
	}

	r.platformPruneMu.Lock()
	running := r.platformPruneRunning
	r.platformPruneMu.Unlock()
	if running {
		t.Error("platformPruneRunning must be released after a successful prune")
	}
}

// blockingArtistLister is an artist pager whose List parks until released. It
// stands in for the real cost: the sweep is a round trip per artist per
// connection, measured at 62s. A test cannot afford 62s, so the DURATION is
// simulated by a lister that does not return -- which makes any handler that
// waits for the sweep fail on a deadline rather than merely being slow.
//
// It honors ctx so a drain (via Reset) can still end a background sweep.
type blockingArtistLister struct {
	release chan struct{}
	started chan struct{}
	once    sync.Once
	// sweeps counts how many sweeps reached the lister. One per
	// ScanPlatformBackdropDuplicates call, since the first page of the walk
	// parks and no sweep ever gets to a second page.
	sweeps atomic.Int64
}

func newBlockingArtistLister() *blockingArtistLister {
	return &blockingArtistLister{release: make(chan struct{}), started: make(chan struct{})}
}

func (b *blockingArtistLister) List(ctx context.Context, _ artist.ListParams) ([]artist.Artist, int, error) {
	b.sweeps.Add(1)
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.release:
		return nil, 0, nil
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
}

// The #3092 regression test, and the load-bearing one: the page must answer
// from cache and NEVER wait for ScanPlatformBackdropDuplicates.
//
// It FAILS against the pre-#3092 handler -- the lister parks, the inline scan
// never returns, the handler misses the deadline. The 3s bound sits far below
// the 62s production measurement and far above an honest cache read, so it
// cannot pass by accident on a fast machine or fail on a slow one.
//
// NOT t.Parallel(): reaches dupimages.Shared(), process-wide state.
func TestPlatformBackdropDuplicatesPage_NeverBlocksOnTheSweep(t *testing.T) {
	dupimages.Shared().Reset()
	// LIFO: release first, then Reset -- Reset drains, and a drain that has to
	// wait on a parked lister would take the whole drain timeout.
	t.Cleanup(func() { dupimages.Shared().Reset() })

	lister := newBlockingArtistLister()
	t.Cleanup(func() { close(lister.release) })

	r := testRouterWithPlatformLister(t, lister)

	req := withI18nCtx(t, httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/reports/platform-backdrop-duplicates", nil))
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.handlePlatformBackdropDuplicatesPage(w, req)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("GET /reports/platform-backdrop-duplicates did not return within 3s while the platform sweep was in flight; " +
			"the render path must read the cached snapshot, never wait for ScanPlatformBackdropDuplicates (#3092)")
	}

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="platform-backdrop-duplicates-unavailable-notice"`) {
		t.Errorf("a never-computed cache must render the pending notice; body: %s", body)
	}
	if strings.Contains(body, `id="platform-backdrop-duplicates-table"`) {
		t.Error("a never-computed cache must not render the report table: rendering zeros would claim every platform is clean, which nothing has established")
	}

	// The cold-cache render must also have KICKED the real background sweep --
	// otherwise the page is permanently pending and the operator's reload never
	// finds anything. Asserted through the real chain (TriggerRefresh ->
	// dupImageCache -> platformDupCounts -> ScanPlatformBackdropDuplicates ->
	// artistLister.List), not a seam.
	select {
	case <-lister.started:
	case <-time.After(3 * time.Second):
		t.Fatal("a cold-cache GET must trigger a real background sweep, but ScanPlatformBackdropDuplicates never reached the artist lister")
	}
}

// Pins AC-4: a burst of cold page loads must produce ONE sweep, not one per
// request -- N renders each starting their own would multiply the load on the
// operator's platforms by N.
//
// The guarantee is dupimages.Cache's; this test exists because the handler must
// ROUTE through it. A revision that swept directly would pass every other test
// here and fail this one.
//
// NOT t.Parallel(): asserts against dupimages.Shared(), process-wide state.
func TestPlatformBackdropDuplicatesPage_ConcurrentColdLoadsSweepOnce(t *testing.T) {
	dupimages.Shared().Reset()
	t.Cleanup(func() { dupimages.Shared().Reset() })

	lister := newBlockingArtistLister()
	t.Cleanup(func() { close(lister.release) })

	r := testRouterWithPlatformLister(t, lister)

	const loads = 8
	var wg sync.WaitGroup
	wg.Add(loads)
	for range loads {
		go func() {
			defer wg.Done()
			req := withI18nCtx(t, httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/reports/platform-backdrop-duplicates", nil))
			w := httptest.NewRecorder()
			r.handlePlatformBackdropDuplicatesPage(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", w.Code)
			}
		}()
	}

	// BOUNDED wait, not a bare wg.Wait(). Against a handler that sweeps inline
	// these renders never return at all -- the lister parks -- so a bare Wait
	// wedges the whole package until the go test timeout and reports a stack
	// dump rather than this test's failure. A regression must be legible.
	loadsDone := make(chan struct{})
	go func() { wg.Wait(); close(loadsDone) }()
	select {
	case <-loadsDone:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent page loads did not all return within 5s; the render path must read the cached snapshot, never wait for the sweep (#3092)")
	}

	// Wait for the sweep the burst kicked to actually reach the lister, so
	// "one sweep" is an assertion about a sweep that happened rather than one
	// that had not started yet.
	select {
	case <-lister.started:
	case <-time.After(3 * time.Second):
		t.Fatal("a cold-cache burst must kick a background sweep, but none reached the artist lister")
	}
	// The latch is held for the whole (parked) sweep, so any second sweep would
	// have had to start while this one was in flight -- exactly the concurrency
	// the guard forbids. Give a would-be second one a moment to appear.
	time.Sleep(100 * time.Millisecond)
	if got := lister.sweeps.Load(); got != 1 {
		t.Errorf("%d concurrent cold page loads started %d sweeps, want exactly 1 (single-flight)", loads, got)
	}
}

// Pins the staleness gate (#3092). Triggering only on a never-computed cache
// left the page FROZEN at its first sweep, since the periodic refresh meant to
// cover that has no production caller.
//
// Two assertions, the second keeping the first honest: an aged-out snapshot
// must kick a sweep AND still render its existing rows with their as-of stamp.
// Falling back to the pending notice would discard a real measurement.
//
// NOT t.Parallel(): reaches dupimages.Shared(), process-wide state.
func TestPlatformBackdropDuplicatesPage_StaleSnapshotTriggersRefreshAndStillRenders(t *testing.T) {
	dupimages.Shared().Reset()
	t.Cleanup(func() { dupimages.Shared().Reset() })

	lister := newBlockingArtistLister()
	t.Cleanup(func() { close(lister.release) })
	r := testRouterWithPlatformLister(t, lister)

	// An established report, stamped older than the max age.
	r.storePlatformDupReport(publish.PlatformBackdropDupReport{
		ArtistsAffected:    1,
		RedundantBackdrops: 3,
		PerArtist:          []publish.ArtistPlatformBackdropDup{{ArtistID: "a1", Connection: "emby", Redundant: 3}},
	})
	r.platformDupReportMu.Lock()
	r.platformDupReportAt = time.Now().Add(-platformDupReportMaxAge - time.Minute)
	r.platformDupReportMu.Unlock()

	req := withI18nCtx(t, httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/reports/platform-backdrop-duplicates", nil))
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() { defer close(done); r.handlePlatformBackdropDuplicatesPage(w, req) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("a stale-snapshot render must not block on the sweep it kicks")
	}

	body := w.Body.String()
	if !strings.Contains(body, `id="platform-backdrop-duplicates-table"`) {
		t.Errorf("a stale snapshot must still render its rows, not the pending notice; body: %s", body)
	}
	if !strings.Contains(body, `id="platform-backdrop-duplicates-as-of"`) {
		t.Error("a stale snapshot must carry its as-of stamp so the operator can see how old it is")
	}

	select {
	case <-lister.started:
	case <-time.After(3 * time.Second):
		t.Fatal("an aged-out snapshot must kick a background sweep (the #3092 frozen-forever gap), but the sweep never reached the artist lister")
	}
}

// The control that keeps the gate above from becoming "sweep on every load". A
// FRESH snapshot must NOT trigger: Cache.refresh runs both halves, so each
// needless trigger is the 62s platform sweep plus the 257s library re-hash.
//
// NOT t.Parallel(): reaches dupimages.Shared(), process-wide state.
func TestPlatformBackdropDuplicatesPage_FreshSnapshotDoesNotTriggerASweep(t *testing.T) {
	dupimages.Shared().Reset()
	t.Cleanup(func() { dupimages.Shared().Reset() })

	lister := newBlockingArtistLister()
	t.Cleanup(func() { close(lister.release) })
	r := testRouterWithPlatformLister(t, lister)

	r.storePlatformDupReport(publish.PlatformBackdropDupReport{RedundantBackdrops: 3})

	// BOUNDED, like every sibling here. Against a handler that sweeps inline
	// these renders never return -- the lister parks -- so an unbounded loop
	// wedges the whole package until the go test timeout and reports a panic
	// stack instead of this test's failure. A regression must be legible.
	loads := make(chan struct{})
	go func() {
		defer close(loads)
		for range 3 {
			req := withI18nCtx(t, httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/reports/platform-backdrop-duplicates", nil))
			w := httptest.NewRecorder()
			r.handlePlatformBackdropDuplicatesPage(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", w.Code)
			}
		}
	}()
	select {
	case <-loads:
	case <-time.After(5 * time.Second):
		t.Fatal("repeated page loads did not return within 5s; the render path must read the cached snapshot, never wait for a sweep (#3092)")
	}

	// Give a would-be sweep time to reach the lister before declaring none ran.
	select {
	case <-lister.started:
		t.Fatal("a fresh snapshot must NOT trigger a sweep; repeated loads would put the process's most expensive task on a ~33% duty cycle")
	case <-time.After(300 * time.Millisecond):
	}
}
