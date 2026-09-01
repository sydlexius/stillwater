package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
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
	r.storePlatformDupReport(publish.PlatformBackdropDupReport{}, time.Now())

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

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/platform-backdrop-duplicates/prune", allArtistsPruneBody())
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
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/reports/platform-backdrop-duplicates/prune", allArtistsPruneBody())
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

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/platform-backdrop-duplicates/prune", allArtistsPruneBody())
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

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/platform-backdrop-duplicates/prune", allArtistsPruneBody())
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPrune(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 when the prune errors; body: %s", w.Code, w.Body.String())
	}

	// #3119: even a prune that never touched a platform (publisher not fully
	// wired, so the result is the zero value) must report the JSON shape --
	// the point of the fix is that the shape is uniform, not that this
	// particular failure carries real counts. Asserted on Content-Type too,
	// since http.Error's bare string would still satisfy a body substring
	// check while being unparsable JSON.
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json; a client parsing this as JSON must not silently get text/plain", ct)
	}
	var errBody map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("error response body is not valid JSON: %v (body: %s)", err, w.Body.String())
	}
	if errBody["error"] != "prune failed" {
		t.Errorf(`error body["error"] = %v, want "prune failed"`, errBody["error"])
	}
	if errBody["partial"] != false {
		t.Errorf(`error body["partial"] = %v, want false (result is the zero value: nothing was deleted, nothing failed)`, errBody["partial"])
	}
	// Every count field must be the actual ZERO VALUE, not merely present: an
	// unwired publisher never touched a platform, so a nonzero count here would
	// be a wrong-but-present value that a bare existence check cannot catch.
	for _, field := range []string{"artists_processed", "backdrops_removed", "skipped_changed", "failures"} {
		got, ok := errBody[field]
		if !ok {
			t.Errorf("error body missing %q field: %v", field, errBody)
			continue
		}
		if got != float64(0) {
			t.Errorf(`error body[%q] = %v, want 0 (the publisher never touched a platform)`, field, got)
		}
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

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/platform-backdrop-duplicates/prune", allArtistsPruneBody())
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

	// Build the i18n-enabled context ON THE TEST GOROUTINE. withI18nCtx calls
	// t.Fatalf on a bundle-load failure, and Fatalf/FailNow are only valid from
	// the goroutine running the test: from a worker they neither abort the test
	// correctly nor stop that worker, so a fixture failure would surface as a
	// confusing downstream assertion instead of a clean fatal.
	//
	// Each worker still builds its OWN request from this context: an
	// *http.Request carries per-request state and must not be shared across
	// concurrent handler invocations.
	reqCtx := withI18nCtx(t, httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/reports/platform-backdrop-duplicates", nil)).Context()

	const loads = 8
	var wg sync.WaitGroup
	wg.Add(loads)
	for range loads {
		go func() {
			defer wg.Done()
			req := httptest.NewRequestWithContext(reqCtx, http.MethodGet, "/reports/platform-backdrop-duplicates", nil)
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
	}, time.Now())
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

	r.storePlatformDupReport(publish.PlatformBackdropDupReport{RedundantBackdrops: 3}, time.Now())

	// Built on the TEST goroutine: withI18nCtx calls t.Fatalf, which is only
	// valid from the goroutine running the test. See the sibling burst test.
	reqCtx := withI18nCtx(t, httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/reports/platform-backdrop-duplicates", nil)).Context()

	// BOUNDED, like every sibling here. Against a handler that sweeps inline
	// these renders never return -- the lister parks -- so an unbounded loop
	// wedges the whole package until the go test timeout and reports a panic
	// stack instead of this test's failure. A regression must be legible.
	loads := make(chan struct{})
	go func() {
		defer close(loads)
		for range 3 {
			req := httptest.NewRequestWithContext(reqCtx, http.MethodGet, "/reports/platform-backdrop-duplicates", nil)
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

// SHOULD-FIX-2 (hostile review). A TOTAL platform outage must not blank an
// established report.
//
// When a platform is unreachable EVERY per-artist query fails, so the sweep
// returns an empty PerArtist with err == nil and a high ScanErrors -- a result
// that reads, to unguarded code, exactly like "every platform is clean".
// Storing it replaces real rows with an empty table, and nothing re-triggers for
// 12h (StartDuplicateImageCountRefresh has no production caller), so the
// operator loses the report for half a day during a transient blip.
//
// This is the report-side twin of the guard AC-5 required for the counts.
func TestStorePlatformDupReport_TotalOutageDoesNotBlankAnEstablishedReport(t *testing.T) {
	t.Parallel()
	r := testRouterWithPlatformPublisher(t)

	r.storePlatformDupReport(publish.PlatformBackdropDupReport{
		ArtistsAffected:    2,
		RedundantBackdrops: 7,
		PerArtist: []publish.ArtistPlatformBackdropDup{
			{ArtistID: "a1", Connection: "emby", Redundant: 4},
			{ArtistID: "a2", Connection: "emby", Redundant: 3},
		},
	}, time.Now())

	// Total outage: nothing seen, every query failed, err == nil.
	r.storePlatformDupReport(publish.PlatformBackdropDupReport{ScanErrors: 3800}, time.Now())

	report, _, ok := r.platformDupReportSnapshot()
	if !ok {
		t.Fatal("the established report must survive a total outage")
	}
	if report.RedundantBackdrops != 7 || len(report.PerArtist) != 2 {
		t.Errorf("cached report = %+v, want the established one (7 redundant, 2 rows) carried forward; "+
			"a sweep that saw NOTHING must not blank real rows for the 12h until anything re-triggers", report)
	}
}

// Two controls for the outage guard, both necessary: it must not become "refuse
// every partial sweep" (the page renders its own ScanErrors notice, so a partial
// sweep that still returned rows is more informative than a stale one), and it
// must not leave a never-swept cache pending forever while a platform is down.
func TestStorePlatformDupReport_OutageGuardIsNarrow(t *testing.T) {
	t.Parallel()

	t.Run("a partial sweep WITH rows still replaces the established report", func(t *testing.T) {
		t.Parallel()
		r := testRouterWithPlatformPublisher(t)
		r.storePlatformDupReport(publish.PlatformBackdropDupReport{
			RedundantBackdrops: 7,
			PerArtist:          []publish.ArtistPlatformBackdropDup{{ArtistID: "a1", Redundant: 7}},
		}, time.Now())

		r.storePlatformDupReport(publish.PlatformBackdropDupReport{
			ScanErrors:         3,
			RedundantBackdrops: 2,
			PerArtist:          []publish.ArtistPlatformBackdropDup{{ArtistID: "a2", Redundant: 2}},
		}, time.Now())

		report, _, _ := r.platformDupReportSnapshot()
		if report.RedundantBackdrops != 2 {
			t.Errorf("RedundantBackdrops = %d, want 2: a partial sweep that still saw rows is authoritative for what it saw, and the page flags it", report.RedundantBackdrops)
		}
	})

	t.Run("a total outage still seeds a never-swept cache", func(t *testing.T) {
		t.Parallel()
		r := testRouterWithPlatformPublisher(t)

		r.storePlatformDupReport(publish.PlatformBackdropDupReport{ScanErrors: 3800}, time.Now())

		if _, _, ok := r.platformDupReportSnapshot(); !ok {
			t.Error("with nothing established there is nothing to protect, so the sweep must land; " +
				"refusing it leaves the page pending for as long as the platform is down, which tells the operator nothing")
		}
	})
}

// Pins AC-5 of #3092 through the machinery a real refresh uses. The guard was
// once the report handler's `if report.ScanErrors == 0` gate on an
// opportunistic count store; that store went with the inline sweep, so the
// protection now lives in platformDupCountsFrom, which reports a partial sweep
// as ErrPartialScan so Refresh carries the last known counts forward.
//
// The failure mode is a WIPE, not an undercount: an unreachable platform fails
// EVERY per-artist query, so PerArtist comes back empty with err == nil, and an
// unguarded refresh reads that as "every platform is clean".
//
// Asserted against a real Cache with real sources: the guard returning an error
// proves nothing if the refresh path stops honoring it.
//
// NOT t.Parallel() in spirit but safe here -- its own cache, never Shared().
func TestPlatformDupCounts_PartialSweepDoesNotClearEstablishedCounts(t *testing.T) {
	t.Parallel()
	r := testRouterWithPlatformPublisher(t)

	cache := dupimages.New(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(cache.Reset)
	cache.StorePlatforms([]dupimages.PlatformCount{{Type: "emby", Label: "Emby", Count: 5}})

	// A total outage: nothing seen, every query failed, err == nil.
	outage := stubPlatformDupScanner{report: publish.PlatformBackdropDupReport{ScanErrors: 3800}}
	cache.SetSources(nil, func(ctx context.Context) ([]dupimages.PlatformCount, error) {
		return r.platformDupCountsFrom(ctx, outage)
	})

	if err := cache.Refresh(context.Background()); err == nil {
		t.Fatal("a partial sweep must surface as an error from Refresh")
	}

	got := cache.Get()
	if len(got.Platforms) != 1 || got.Platforms[0].Count != 5 {
		t.Errorf("platform counts = %+v, want the established Emby count of 5 carried forward; a partial sweep must not clear real counts", got.Platforms)
	}
}

// The control that keeps the guard above honest: a COMPLETE sweep that
// legitimately found nothing must still clear the rows, or the sidebar would
// keep claiming duplicates the operator already pruned. Without this, "never
// write" would pass the test above and be badly wrong.
//
// Safe in parallel for the same reason as the test above: its own cache.
func TestPlatformDupCounts_CompleteEmptySweepClearsEstablishedCounts(t *testing.T) {
	t.Parallel()
	r := testRouterWithPlatformPublisher(t)

	cache := dupimages.New(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(cache.Reset)
	cache.StorePlatforms([]dupimages.PlatformCount{{Type: "emby", Label: "Emby", Count: 5}})

	clean := stubPlatformDupScanner{report: publish.PlatformBackdropDupReport{}}
	cache.SetSources(nil, func(ctx context.Context) ([]dupimages.PlatformCount, error) {
		return r.platformDupCountsFrom(ctx, clean)
	})

	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("a complete sweep must succeed; got %v", err)
	}
	if got := cache.Get(); len(got.Platforms) != 0 {
		t.Errorf("platform counts = %+v, want cleared by a complete, clean sweep", got.Platforms)
	}
}

// The REPORT half stores a partial sweep, unlike the counts half above, and
// this pins that split rather than leaving it to a reader to infer from a
// silence. The page has somewhere to put the caveat -- it renders its own
// partial-scan notice off ScanErrors -- so an incomplete sweep arrives
// labeled as incomplete instead of leaving the page saying "pending" for as
// long as a platform is down. Same rule libraryDupCount applies to the local
// report.
func TestPlatformDupReport_PartialSweepIsStoredAndFlagged(t *testing.T) {
	t.Parallel()
	r := testRouterWithPlatformPublisher(t)

	partial := stubPlatformDupScanner{report: publish.PlatformBackdropDupReport{
		ScanErrors:         7,
		RedundantBackdrops: 2,
		PerArtist:          []publish.ArtistPlatformBackdropDup{{ArtistID: "a1", Redundant: 2}},
	}}
	if _, err := r.platformDupCountsFrom(context.Background(), partial); err == nil {
		t.Fatal("a partial sweep must still report itself as an error to the COUNTS half")
	}

	got, _, ok := r.platformDupReportSnapshot()
	if !ok {
		t.Fatal("a partial sweep must still populate the report, so the page can show what it did see")
	}
	if got.ScanErrors != 7 {
		t.Errorf("cached report ScanErrors = %d, want 7 surfaced so the page flags the sweep as incomplete", got.ScanErrors)
	}
}

// Pins AC-6. Stale REPORTING is fine; stale DELETING is not. The prune
// re-derives what to delete from a live platform read, so it must never be fed
// the cached snapshot the page renders from.
//
// Wired so the only way to report nonzero work is to walk the library for real:
// the cache claims offenders while the publisher's lister returns none.
//
// NOT t.Parallel(): the post-prune re-sweep runs through dupimages.Shared(),
// which is process-wide state.
func TestPlatformBackdropDuplicatesPrune_ScansFreshNeverTheCache(t *testing.T) {
	dupimages.Shared().Reset()
	t.Cleanup(func() { dupimages.Shared().Reset() })
	r := testRouterWithPlatformPublisher(t)

	r.storePlatformDupReport(publish.PlatformBackdropDupReport{
		ConnectionsAffected: 1,
		ArtistsAffected:     3,
		RedundantBackdrops:  11,
		PerArtist: []publish.ArtistPlatformBackdropDup{
			{ArtistID: "a1", ConnectionID: "c-emby", Connection: "emby", Backdrops: 5, Redundant: 4},
		},
	}, time.Now())

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/platform-backdrop-duplicates/prune", allArtistsPruneBody())
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPrune(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"artists_processed":0`) || !strings.Contains(body, `"backdrops_removed":0`) {
		t.Errorf("prune must operate on a FRESH scan (the empty library), not the cached report claiming 3 artists and 11 redundant backdrops; body: %s", body)
	}

	// The prune ALSO invalidates the cached report, so the reload its button
	// fires cannot show the pre-prune rows -- images the operator just deleted.
	// Asserted here rather than in its own test because it is the same operator
	// journey: a prune that leaves the page claiming 11 redundant backdrops is
	// indistinguishable, on screen, from a prune that did nothing.
	if _, _, ok := r.platformDupReportSnapshot(); ok {
		t.Error("a successful prune must invalidate the cached report; a reload showing the pre-prune rows reads as a prune that did nothing")
	}
}

// And the render side of that same journey: after a prune the reload must never
// show the pre-prune rows.
//
// The assertion is deliberately about what the operator must NOT see, rather
// than pinning one specific replacement state. Two outcomes are both correct and
// which one appears depends on how fast the sweep is: against a large library
// the 62s sweep is still running, so the page reads "pending"; against a tiny
// one (this test, and a small real library) the refresh the prune kicks can land
// first, and the page shows a freshly-swept empty table -- which is the MORE
// accurate answer, not a worse one.
//
// An earlier version of this test asserted "pending" specifically. Adding the
// sidebar-count refresh (SHOULD-FIX-3) made the sweep land before the reload
// here and the test failed -- correctly reporting that a timing-dependent
// assertion had been pinned as an invariant. What is genuinely invariant is that
// the deleted backdrops are gone from the page, so that is what is asserted.
//
// NOT t.Parallel(): reaches dupimages.Shared() through the prune's refresh kick.
func TestPlatformBackdropDuplicatesPage_AfterPruneNeverShowsPrePruneRows(t *testing.T) {
	dupimages.Shared().Reset()
	t.Cleanup(func() { dupimages.Shared().Reset() })
	r := testRouterWithPlatformPublisher(t)

	const prePruneArtist = "artist-pruned-away"
	r.storePlatformDupReport(publish.PlatformBackdropDupReport{
		ArtistsAffected:    1,
		RedundantBackdrops: 4,
		PerArtist:          []publish.ArtistPlatformBackdropDup{{ArtistID: prePruneArtist, Connection: "emby", Redundant: 4}},
	}, time.Now())

	pruneReq := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/platform-backdrop-duplicates/prune", allArtistsPruneBody())
	pruneRec := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPrune(pruneRec, pruneReq)
	if pruneRec.Code != http.StatusOK {
		t.Fatalf("prune status = %d, want 200 (body: %s)", pruneRec.Code, pruneRec.Body.String())
	}

	pageReq := withI18nCtx(t, httptest.NewRequestWithContext(adminContext(), http.MethodGet, "/reports/platform-backdrop-duplicates", nil))
	pageRec := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPage(pageRec, pageReq)

	body := pageRec.Body.String()
	if strings.Contains(body, prePruneArtist) {
		t.Errorf("the reload after a prune must not list a backdrop the prune deleted; body: %s", body)
	}

	// And it must be in one of the two legitimate post-prune states, never
	// silently rendering something else. Without this the test would pass on a
	// page that failed to render at all.
	pending := strings.Contains(body, `id="platform-backdrop-duplicates-unavailable-notice"`)
	swept := strings.Contains(body, `id="platform-backdrop-duplicates-empty"`)
	if !pending && !swept {
		t.Errorf("after a prune the page must render either the pending notice or a freshly-swept empty report; body: %s", body)
	}
}

// BLOCKING-1 (hostile review). A sweep already in flight when an operator
// prunes must NOT land afterwards and resurrect the rows the prune deleted.
//
// The sequence, which is ordinary rather than exotic: an admin opens the report,
// the age gate fires a 62s sweep, the admin prunes while it runs. The sweep read
// the platforms BEFORE the deletes, so its rows are stale the moment the prune
// commits -- but it finishes last, and an unguarded store would write them back
// with a FRESH "as of" stamp and re-arm a Prune button pointed at images that
// are already gone.
//
// dupimages.Cache's single-flight latch cannot prevent this: it serializes
// sweeps against each OTHER, never a sweep against an invalidation. The fix is
// the compare-and-swap on platformDupReportInvalidatedAt.
//
// Written against the store seam directly rather than by racing two goroutines:
// the defect is an ORDERING rule, and asserting it deterministically is worth
// more than a timing-dependent reproduction that passes when the race loses.
func TestStorePlatformDupReport_SweepStartedBeforeAPruneCannotResurrectRows(t *testing.T) {
	t.Parallel()
	r := testRouterWithPlatformPublisher(t)

	// The sweep begins, reading pre-prune state.
	sweepStartedAt := time.Now()

	// The operator prunes while it is still running.
	r.storePlatformDupReport(publish.PlatformBackdropDupReport{
		ArtistsAffected:    1,
		RedundantBackdrops: 4,
		PerArtist:          []publish.ArtistPlatformBackdropDup{{ArtistID: "a1", Connection: "emby", Redundant: 4}},
	}, sweepStartedAt.Add(-time.Minute)) // an older, established report
	r.invalidatePlatformDupReport()

	// Now the in-flight sweep finishes and tries to store what it saw.
	r.storePlatformDupReport(publish.PlatformBackdropDupReport{
		ArtistsAffected:    1,
		RedundantBackdrops: 4,
		PerArtist:          []publish.ArtistPlatformBackdropDup{{ArtistID: "a1", Connection: "emby", Redundant: 4}},
	}, sweepStartedAt)

	report, _, ok := r.platformDupReportSnapshot()
	if ok {
		t.Fatalf("a sweep that STARTED before the prune must not repopulate the cache; got %+v with a fresh timestamp. "+
			"The operator would see deleted backdrops listed again, with a live Prune button pointed at images that no longer exist", report)
	}
}

// The control that keeps the guard above from becoming "never store again": a
// sweep that started AFTER the prune is exactly the one whose rows are correct,
// and it must land. Without this, the invalidation would be permanent and the
// page would say "pending" for the life of the process.
func TestStorePlatformDupReport_SweepStartedAfterAPruneIsStored(t *testing.T) {
	t.Parallel()
	r := testRouterWithPlatformPublisher(t)

	r.storePlatformDupReport(publish.PlatformBackdropDupReport{RedundantBackdrops: 4}, time.Now().Add(-time.Minute))
	r.invalidatePlatformDupReport()

	// A fresh sweep, begun after the prune committed, finds one survivor.
	// The stamp uses a deliberate forward offset rather than a bare
	// time.Now() (#3166): under parallel load, a goroutine can be
	// descheduled between invalidatePlatformDupReport's internal time.Now()
	// stamp and this one, and on a coarse clock read the two can land on
	// the same instant -- the store's `!After` guard then (correctly) drops
	// a sweep it cannot prove started later, and the test's own assertion
	// that it landed fires. The relation under test is "started after", so
	// state it explicitly instead of racing for it.
	r.storePlatformDupReport(publish.PlatformBackdropDupReport{
		ArtistsAffected:    1,
		RedundantBackdrops: 1,
		PerArtist:          []publish.ArtistPlatformBackdropDup{{ArtistID: "a1", Connection: "emby", Redundant: 1}},
	}, time.Now().Add(time.Second))

	report, _, ok := r.platformDupReportSnapshot()
	if !ok {
		t.Fatal("a sweep that started AFTER the prune must land; otherwise the invalidation is permanent and the page never recovers")
	}
	if report.RedundantBackdrops != 1 {
		t.Errorf("RedundantBackdrops = %d, want 1 from the post-prune sweep", report.RedundantBackdrops)
	}
}

// SHOULD-FIX-3 (hostile review). A prune must also kick the SIDEBAR counts.
// They live in a separate cache whose lazy trigger only fires on !Computed --
// already false once any sweep has landed -- so without an explicit kick the
// pill keeps advertising duplicates the operator just deleted, while the report
// itself correctly reads "not yet measured". The two surfaces would permanently
// disagree, which is a gap this PR widens: before it, both were consistently
// stale together.
//
// The lister CANNOT be a parking one here, which is what an earlier version of
// this test got wrong: the prune itself walks the same artist lister, so a
// lister that never returns parks the PRUNE and the handler never reaches the
// refresh kick at all -- the test then hangs for the package timeout instead of
// failing. The real publisher over an empty library returns immediately, so the
// kick is observed through the cache's own state rather than a blocked sweep.
//
// NOT t.Parallel(): drives dupimages.Shared(), process-wide state.
func TestPlatformBackdropDuplicatesPrune_AlsoRefreshesTheSidebarCounts(t *testing.T) {
	dupimages.Shared().Reset()
	t.Cleanup(func() { dupimages.Shared().Reset() })
	r := testRouterWithPlatformPublisher(t)

	// Pre-load a stale platform count, as a pre-prune sweep would have left.
	// Computed is now true, so the lazy !Computed trigger is disarmed -- exactly
	// the state in which the sidebar would otherwise stay wrong forever.
	cache := r.dupImageCache()
	cache.StorePlatforms([]dupimages.PlatformCount{{Type: "emby", Label: "Emby", Count: 9}})
	if got := cache.Get(); len(got.Platforms) != 1 || !got.Computed {
		t.Fatalf("precondition: wanted an established platform count, got %+v", got)
	}

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/platform-backdrop-duplicates/prune", allArtistsPruneBody())
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPrune(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("prune status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	// The kick runs the REAL chain (TriggerRefresh -> dupImageCache ->
	// platformDupCounts -> ScanPlatformBackdropDuplicates) against this empty
	// library, which finds no offenders and clears the stale count. Polled
	// rather than asserted once: TriggerRefresh is fire-and-forget by design.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if len(cache.Get().Platforms) == 0 {
			return // the stale count was cleared: the kick landed
		}
		if time.Now().After(deadline) {
			t.Fatal("a successful prune must kick a sidebar count refresh; the stale Emby count of 9 was still showing 3s later, " +
				"so the pill would keep advertising backdrops the operator just deleted")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// pagingFailureArtistLister returns one FULL page of artists and then fails,
// which is the only shape that reproduces a PARTIAL prune through the public
// handler. PrunePlatformBackdropDuplicates deletes as it walks, and its single
// hard return is a paging failure, so the interesting state -- "artwork is
// already gone AND the call returns an error" -- only exists when page 1
// succeeded and a later page did not.
//
// page1 is settable after construction because the router (and therefore the
// artist whose real ID the page must carry) does not exist yet when the lister
// is handed to the publisher.
//
// Page 1 is padded to exactly params.PageSize entries rather than a hardcoded
// count: the publisher stops paging when a page comes back SHORT, so a page of
// any other size ends the walk before page 2 is ever requested and the test
// would silently exercise the success path. Reading PageSize off the request
// keeps that true if publish's scanBackdropPageSize is ever retuned.
type pagingFailureArtistLister struct {
	mu    sync.Mutex
	page1 []artist.Artist
	pages atomic.Int32
}

func (l *pagingFailureArtistLister) setPage1(as []artist.Artist) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.page1 = as
}

func (l *pagingFailureArtistLister) List(_ context.Context, params artist.ListParams) ([]artist.Artist, int, error) {
	l.pages.Add(1)
	if params.Page > 1 {
		return nil, 0, errors.New("test: artist listing failed at page 2")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]artist.Artist, 0, params.PageSize)
	out = append(out, l.page1...)
	// Filler artists carry IDs that exist in no table, so GetPlatformIDs
	// returns nothing for them and they prune nothing -- they exist only to
	// make the page full so the walk continues to the failing page 2.
	for i := len(out); i < params.PageSize; i++ {
		out = append(out, artist.Artist{ID: fmt.Sprintf("filler-%d", i), Name: "filler"})
	}
	return out, params.PageSize + 1, nil
}

// duplicateBackdropPlatform is a minimal Emby stand-in holding one artist whose
// backdrops are byte-identical, so a prune has something real to delete. It
// serves the three endpoints the prune path touches (artist detail, indexed
// backdrop read, indexed image delete) and records the delete count, which is
// how the test proves the prune actually removed artwork rather than passing
// vacuously against a platform that had nothing to remove.
type duplicateBackdropPlatform struct {
	mu        sync.Mutex
	backdrops [][]byte
	deletes   atomic.Int32
	details   atomic.Int32
}

func (p *duplicateBackdropPlatform) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.backdrops)
}

func (p *duplicateBackdropPlatform) handler(platformArtistID, platformUserID string) http.HandlerFunc {
	detailPath := "/Users/" + platformUserID + "/Items/" + platformArtistID
	imagePrefix := "/Items/" + platformArtistID + "/Images/Backdrop/"
	return func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodGet && req.URL.Path == detailPath:
			p.details.Add(1)
			tags := make([]string, 0, p.count())
			for i := 0; i < p.count(); i++ {
				tags = append(tags, fmt.Sprintf(`"tag%d"`, i))
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"Name":"Dup","Id":%q,"SortName":"Dup","ImageTags":{},"BackdropImageTags":[%s],`+
				`"ProviderIds":{},"Overview":"","Genres":[],"Tags":[],"PremiereDate":"","EndDate":"","LockedFields":[],"LockData":false}`,
				platformArtistID, strings.Join(tags, ","))
		case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, imagePrefix):
			idx, err := strconv.Atoi(strings.TrimPrefix(req.URL.Path, imagePrefix))
			p.mu.Lock()
			defer p.mu.Unlock()
			if err != nil || idx < 0 || idx >= len(p.backdrops) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(p.backdrops[idx])
		case req.Method == http.MethodDelete && strings.HasPrefix(req.URL.Path, imagePrefix):
			idx, err := strconv.Atoi(strings.TrimPrefix(req.URL.Path, imagePrefix))
			p.mu.Lock()
			defer p.mu.Unlock()
			if err != nil || idx < 0 || idx >= len(p.backdrops) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			// Emby re-indexes after a delete, which is why the prune deletes
			// high-index-first; modeled here so the re-verify reads that follow
			// see the same shifted state a real platform would show.
			p.backdrops = append(p.backdrops[:idx], p.backdrops[idx+1:]...)
			p.deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// A prune that FAILS after it has already deleted must still invalidate the
// caches. This is #3092 on the error path: the publisher walks the library page
// by page and deletes as it goes, so a paging failure on page 2 returns an
// error with page 1's artwork already removed. Before the fix the handler
// returned 500 straight out, leaving the cached report -- the page's ONLY data
// source -- listing backdrops that are no longer on the platform, with the
// Prune button re-armed against them.
//
// This is the one failure shape that distinguishes the two behaviors, which is
// why the lister must return a FULL page before failing rather than failing
// outright the way TestPlatformBackdropDuplicatesPrune_Error's does.
//
// NOT t.Parallel(): the invalidation kicks dupimages.Shared(), process-wide
// state.
func TestPlatformBackdropDuplicatesPrune_PartialFailureStillInvalidatesTheCache(t *testing.T) {
	dupimages.Shared().Reset()
	t.Cleanup(func() { dupimages.Shared().Reset() })

	const (
		platformArtistID = "emby-dup-artist"
		platformUserID   = "test-user-1"
		connID           = "conn-emby-partial"
	)

	// Three byte-identical backdrops: indices 1 and 2 are redundant copies of
	// the survivor at 0, so a prune has exactly two deletes to perform.
	plat := &duplicateBackdropPlatform{backdrops: [][]byte{
		[]byte("identical-backdrop-bytes"),
		[]byte("identical-backdrop-bytes"),
		[]byte("identical-backdrop-bytes"),
	}}
	srv := httptest.NewServer(plat.handler(platformArtistID, platformUserID))
	defer srv.Close()

	lister := &pagingFailureArtistLister{}
	r := testRouterWithPlatformLister(t, lister)

	a := addTestArtist(t, r.artistService, "DupArtist")
	if err := r.connectionService.Create(context.Background(), &connection.Connection{
		ID:      connID,
		Name:    "My Emby",
		Type:    "emby",
		URL:     srv.URL,
		APIKey:  "test-key",
		Enabled: true,
		Status:  "ok",
		// FeatureImageWrite is what admits this connection to the PRUNE path
		// (pruneOneArtist skips a connection without it), so without it the
		// test would delete nothing and pass against the old code too.
		Emby: &connection.EmbyConfig{PlatformUserID: platformUserID, FeatureImageWrite: true},
	}); err != nil {
		t.Fatalf("creating test connection: %v", err)
	}
	if err := r.artistService.SetPlatformID(context.Background(), a.ID, connID, platformArtistID); err != nil {
		t.Fatalf("setting platform ID: %v", err)
	}
	lister.setPage1([]artist.Artist{*a})

	// PRECONDITION: an ESTABLISHED cached report. Without this, "the cache is
	// empty afterwards" would be indistinguishable from "the cache was never
	// populated" and the assertion below would hold no matter what the handler
	// did.
	r.storePlatformDupReport(publish.PlatformBackdropDupReport{
		ConnectionsAffected: 1,
		ArtistsAffected:     1,
		RedundantBackdrops:  2,
		PerArtist: []publish.ArtistPlatformBackdropDup{
			{ArtistID: a.ID, ConnectionID: connID, Connection: "My Emby", Backdrops: 3, Redundant: 2},
		},
	}, time.Now())
	if _, _, ok := r.platformDupReportSnapshot(); !ok {
		t.Fatal("precondition: the cached report must be established before the prune, or 'invalidated' is indistinguishable from 'never set'")
	}

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/platform-backdrop-duplicates/prune", allArtistsPruneBody())
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPrune(w, req)

	// PRECONDITION: page 1 really deleted. Asserted against the platform's own
	// delete count rather than the response body, which on a 500 carries no
	// result at all. A run that removed nothing would leave the cache
	// legitimately valid, so the test would prove nothing about the fix.
	if got := plat.deletes.Load(); got != 2 {
		t.Fatalf("precondition: page 1 must actually delete before the paging failure; platform recorded %d deletes, want 2. "+
			"With no deletions there is no stale cache to invalidate and this test is vacuous", got)
	}
	if got := plat.count(); got != 1 {
		t.Fatalf("precondition: the platform must be left holding 1 backdrop after the prune, got %d", got)
	}
	if got := lister.pages.Load(); got < 2 {
		t.Fatalf("precondition: the prune must reach the failing page 2; the lister saw %d page requests", got)
	}

	// (a) The operator still gets an honest failure -- a partial prune is not a
	// success and must not be reported as one.
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when the prune fails partway; body: %s", w.Code, w.Body.String())
	}

	// (b) ...and the cached report is gone, so the reload cannot list the two
	// backdrops that were just deleted.
	if report, _, ok := r.platformDupReportSnapshot(); ok {
		t.Errorf("a prune that deleted and THEN failed must still invalidate the cached report; it still holds %+v. "+
			"The operator's reload would list backdrops that are no longer on the platform, with the Prune button armed against them", report)
	}

	// The singleton must also be released, exactly as on the outright-failure
	// path, or one partial prune blocks every later one for the process's life.
	r.platformPruneMu.Lock()
	running := r.platformPruneRunning
	r.platformPruneMu.Unlock()
	if running {
		t.Error("platformPruneRunning must be released after a partially failed prune")
	}

	// And the SIDEBAR cache is kicked too. It is a separate store whose lazy
	// trigger only fires on !Computed, so nothing else would ever re-count it
	// and the pill would keep advertising the deleted backdrops while the
	// report reads "not yet measured".
	//
	// Observed through the platform's own detail-request count: the kicked
	// sweep walks the same library and re-reads this artist. Polled because
	// TriggerRefresh is fire-and-forget by design. The prune itself made
	// exactly one detail read (its detection pass), so anything beyond that is
	// the refresh.
	deadline := time.Now().Add(3 * time.Second)
	for plat.details.Load() <= 1 {
		if time.Now().After(deadline) {
			t.Fatalf("a partially failed prune must also kick the sidebar count refresh; the platform saw only %d detail reads in 3s, "+
				"so the pill would keep advertising backdrops that were just deleted", plat.details.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestPlatformBackdropDuplicatesPrune_ErrorBodyCarriesPartialAccounting is the
// #3119 regression test. It reuses the #3116 partial-failure fixture
// (pagingFailureArtistLister + duplicateBackdropPlatform: page 1 deletes real
// backdrops, page 2 fails paging) but asserts the RESPONSE BODY rather than
// the cache side effects that test already covers: the operator must be able
// to see, from the 500 itself, that the run deleted 2 backdrops before it
// failed. Pre-fix this asserts against a bare "prune failed" string and fails.
func TestPlatformBackdropDuplicatesPrune_ErrorBodyCarriesPartialAccounting(t *testing.T) {
	dupimages.Shared().Reset()
	t.Cleanup(func() { dupimages.Shared().Reset() })

	const (
		platformArtistID = "emby-dup-artist-3119"
		platformUserID   = "test-user-3119"
		connID           = "conn-emby-partial-3119"
	)

	// Three byte-identical backdrops: indices 1 and 2 are redundant copies of
	// the survivor at 0, so a prune has exactly two deletes to perform before
	// the paging failure aborts the run.
	plat := &duplicateBackdropPlatform{backdrops: [][]byte{
		[]byte("identical-backdrop-bytes"),
		[]byte("identical-backdrop-bytes"),
		[]byte("identical-backdrop-bytes"),
	}}
	srv := httptest.NewServer(plat.handler(platformArtistID, platformUserID))
	defer srv.Close()

	lister := &pagingFailureArtistLister{}
	r := testRouterWithPlatformLister(t, lister)

	a := addTestArtist(t, r.artistService, "DupArtist3119")
	if err := r.connectionService.Create(context.Background(), &connection.Connection{
		ID:      connID,
		Name:    "My Emby",
		Type:    "emby",
		URL:     srv.URL,
		APIKey:  "test-key",
		Enabled: true,
		Status:  "ok",
		Emby:    &connection.EmbyConfig{PlatformUserID: platformUserID, FeatureImageWrite: true},
	}); err != nil {
		t.Fatalf("creating test connection: %v", err)
	}
	if err := r.artistService.SetPlatformID(context.Background(), a.ID, connID, platformArtistID); err != nil {
		t.Fatalf("setting platform ID: %v", err)
	}
	lister.setPage1([]artist.Artist{*a})

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/platform-backdrop-duplicates/prune", allArtistsPruneBody())
	w := httptest.NewRecorder()
	r.handlePlatformBackdropDuplicatesPrune(w, req)

	// PRECONDITION: page 1 really deleted, and the run genuinely failed
	// partway. Without this, "the body reports 2 removed" could be a fixture
	// bug rather than the fix under test -- see the sibling cache-invalidation
	// test's identical precondition block for the rationale.
	if got := plat.deletes.Load(); got != 2 {
		t.Fatalf("precondition: page 1 must actually delete before the paging failure; platform recorded %d deletes, want 2", got)
	}
	if got := lister.pages.Load(); got < 2 {
		t.Fatalf("precondition: the prune must reach the failing page 2; the lister saw %d page requests", got)
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("precondition: status = %d, want 500 (the run must have actually failed for this test to mean anything); body: %s", w.Code, w.Body.String())
	}

	// THE FIX: the 500 body must carry the partial accounting, not a bare
	// "prune failed" string, so the operator can tell 2 backdrops were
	// already deleted rather than retrying blind.
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("error response body is not valid JSON: %v (body: %s)", err, w.Body.String())
	}
	// Every field asserted against its ACTUAL VALUE for this fixture, not just
	// its presence. Derived from pruneOneArtist's accounting: one real artist
	// (DupArtist3119) with two redundant backdrops, both re-verified clean and
	// both deleted before the page-2 listing error aborts the walk -- the
	// filler artists padding page 1 have no platform IDs, so they contribute
	// no failures and are never counted as processed. The paging failure
	// itself is the function's hard-return error, not a per-artist Failures
	// entry, so failures stays 0.
	wantFields := map[string]float64{
		"artists_processed": 1,
		"backdrops_removed": 2,
		"skipped_changed":   0,
		"failures":          0,
	}
	for field, want := range wantFields {
		if got := body[field]; got != want {
			t.Errorf("body[%q] = %v, want %v", field, got, want)
		}
	}
	if body["partial"] != true {
		t.Errorf(`body["partial"] = %v, want true (backdrops_removed > 0)`, body["partial"])
	}
	if body["error"] != "prune failed" {
		t.Errorf(`body["error"] = %v, want "prune failed"`, body["error"])
	}
}

// allArtistsPruneBody returns the request body every library-wide prune test
// must now send. The endpoint refuses a request carrying no scope rather than
// defaulting to the whole library (#3139), so a nil body is a 400 and would
// no longer reach the publisher at all.
//
// A fresh reader per call, deliberately: several tests issue two requests
// (the concurrency test in particular), and a shared reader would be drained
// by the first, silently turning the second into an empty-body request that
// fails for a reason unrelated to what the test is asserting.
func allArtistsPruneBody() io.Reader {
	return strings.NewReader(`{"all_artists": true}`)
}
